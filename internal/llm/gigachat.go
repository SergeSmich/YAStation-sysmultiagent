package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// APIGigaChat is the llm_api value of GigaChat (Sber). The wire format is the
// OpenAI-style /chat/completions, but the bearer is not the key: the key
// ("authorization key") is exchanged for an access token that lives 30 minutes.
const APIGigaChat = "gigachat"

const (
	gigaOAuthURL = "https://ngw.devices.sberbank.ru:9443/api/v2/oauth"
	gigaScope    = "GIGACHAT_API_PERS"
	// gigaSkew refreshes the token a little before it expires.
	gigaSkew = 60 * time.Second
)

// GigaChat's servers are signed by the Russian Ministry of Digital Development
// CA, which is not in the system pool of the station. Both certificates come
// from https://gu-st.ru/content/lending/ (the Ministry's own download page) and
// are added to the pool for GigaChat connections only.
//
//go:embed ru_root_ca.pem
var ruRootCA []byte

//go:embed ru_sub_ca.pem
var ruSubCA []byte

var (
	gigaPoolOnce sync.Once
	gigaPool     *x509.CertPool
)

func gigaCertPool() *x509.CertPool {
	gigaPoolOnce.Do(func() {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(ruRootCA)
		pool.AppendCertsFromPEM(ruSubCA)
		gigaPool = pool
	})
	return gigaPool
}

// GigaHTTPClient returns an HTTP client that trusts the Russian CA.
// The transport is shared: a new one per call would keep its idle connections
// open for good.
func GigaHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: gigaTransport()}
}

var (
	gigaTransportOnce sync.Once
	gigaTransportVal  *http.Transport
)

func gigaTransport() *http.Transport {
	gigaTransportOnce.Do(func() {
		gigaTransportVal = &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     &tls.Config{RootCAs: gigaCertPool(), MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 15 * time.Second,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
		}
	})
	return gigaTransportVal
}

type gigaToken struct {
	token   string
	expires time.Time
}

var (
	gigaMu     sync.Mutex
	gigaTokens = map[[32]byte]gigaToken{}
)

func gigaRqUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// GigaAccessToken exchanges the authorization key for an access token and keeps
// it until shortly before it expires. The key is never logged or put in an error.
func GigaAccessToken(ctx context.Context, authKey string) (string, error) {
	authKey = strings.TrimSpace(authKey)
	authKey = strings.TrimSpace(strings.TrimPrefix(authKey, "Basic "))
	if authKey == "" {
		return "", errors.New("GigaChat authorization key is empty")
	}
	id := sha256.Sum256([]byte(authKey))
	gigaMu.Lock()
	cached, ok := gigaTokens[id]
	gigaMu.Unlock()
	if ok && time.Until(cached.expires) > gigaSkew {
		return cached.token, nil
	}
	form := url.Values{"scope": {gigaScope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gigaOAuthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("invalid GigaChat OAuth endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("RqUID", gigaRqUID())
	req.Header.Set("Authorization", "Basic "+authKey)
	resp, err := GigaHTTPClient(20 * time.Second).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("GigaChat OAuth: %w", ctx.Err())
		}
		return "", errors.New("GigaChat OAuth is unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GigaChat OAuth returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", errors.New("cannot read GigaChat OAuth response")
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &answer); err != nil || strings.TrimSpace(answer.AccessToken) == "" {
		return "", errors.New("invalid GigaChat OAuth response")
	}
	expires := time.UnixMilli(answer.ExpiresAt)
	if answer.ExpiresAt <= 0 || time.Until(expires) <= 0 || time.Until(expires) > 24*time.Hour {
		expires = time.Now().Add(25 * time.Minute)
	}
	gigaMu.Lock()
	gigaTokens[id] = gigaToken{token: answer.AccessToken, expires: expires}
	gigaMu.Unlock()
	return answer.AccessToken, nil
}

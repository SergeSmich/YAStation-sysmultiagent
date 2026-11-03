package llm

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// The embedded Russian CA files must be real certificates: a broken file would
// only show up as a TLS error against Sber on the station.
func TestGigaEmbeddedCertsParse(t *testing.T) {
	for name, data := range map[string][]byte{"root": ruRootCA, "sub": ruSubCA} {
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			t.Fatalf("%s: not a PEM certificate", name)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !cert.IsCA {
			t.Fatalf("%s: not a CA certificate", name)
		}
		if !strings.Contains(cert.Subject.String(), "Russian") {
			t.Fatalf("%s: unexpected subject %q", name, cert.Subject.String())
		}
	}
	if gigaCertPool() == nil {
		t.Fatal("empty pool")
	}
}

// A cached token is returned without a network call, and the prefix "Basic " a
// user may paste with the key is ignored.
func TestGigaAccessTokenUsesCache(t *testing.T) {
	const key = "dGVzdC1rZXk6c2VjcmV0"
	id := sha256.Sum256([]byte(key))
	gigaMu.Lock()
	gigaTokens[id] = gigaToken{token: "cached-token", expires: time.Now().Add(10 * time.Minute)}
	gigaMu.Unlock()
	t.Cleanup(func() {
		gigaMu.Lock()
		delete(gigaTokens, id)
		gigaMu.Unlock()
	})
	for _, in := range []string{key, "Basic " + key, "  " + key + " "} {
		got, err := GigaAccessToken(context.Background(), in)
		if err != nil || got != "cached-token" {
			t.Fatalf("%q: got %q, %v", in, got, err)
		}
	}
	if _, err := GigaAccessToken(context.Background(), "  "); err == nil {
		t.Fatal("empty key accepted")
	}
}

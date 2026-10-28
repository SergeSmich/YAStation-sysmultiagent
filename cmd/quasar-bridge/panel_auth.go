package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The panel is guarded by one operator login. Only a PBKDF2-HMAC-SHA256 digest
// is stored, next to the configuration, in a 0600 file.
const (
	panelSessionCookie = "midi_panel"
	panelSessionTTL    = 12 * time.Hour
	panelFailureLimit  = 5
	panelFailureWindow = 5 * time.Minute
	panelDefaultRounds = 210_000
	panelSaltLen       = 16
	panelKeyLen        = 32
	panelPasswordMin   = 8
)

// panelCredential is the on-disk record created by `-panel-passwd`.
type panelCredential struct {
	Login  string `json:"login"`
	Salt   string `json:"salt"`
	Hash   string `json:"hash"`
	Rounds int    `json:"rounds"`
}

// newPanelCredential hashes a password for storage. The login name is kept in
// clear: it is not a secret, and the operator has to know what to type.
func newPanelCredential(login, password string) (panelCredential, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return panelCredential{}, errors.New("panel login must not be empty")
	}
	if len([]rune(password)) < panelPasswordMin {
		return panelCredential{}, fmt.Errorf("panel password must have at least %d characters", panelPasswordMin)
	}
	salt := make([]byte, panelSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return panelCredential{}, err
	}
	sum := pbkdf2SHA256([]byte(password), salt, panelDefaultRounds, panelKeyLen)
	return panelCredential{
		Login:  login,
		Salt:   base64.StdEncoding.EncodeToString(salt),
		Hash:   base64.StdEncoding.EncodeToString(sum),
		Rounds: panelDefaultRounds,
	}, nil
}

// derive computes the digest for a candidate password. A damaged record yields
// an error, never a match.
func (c panelCredential) derive(password string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(c.Salt)
	if err != nil || len(salt) == 0 {
		return nil, errors.New("panel credential is damaged")
	}
	rounds := c.Rounds
	if rounds <= 0 {
		rounds = panelDefaultRounds
	}
	return pbkdf2SHA256([]byte(password), salt, rounds, panelKeyLen), nil
}

// verify compares both the digest and the login in constant time. The digest is
// derived even when the login is wrong, so a typo in the name is not faster than
// a typo in the password.
func (c panelCredential) verify(login, password string) bool {
	got, err := c.derive(password)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(c.Hash)
	if err != nil {
		return false
	}
	digestOK := subtle.ConstantTimeCompare(got, want) == 1
	loginOK := subtle.ConstantTimeCompare([]byte(c.Login), []byte(login)) == 1
	return digestOK && loginOK
}

// pbkdf2SHA256 is PBKDF2 (RFC 8018) with HMAC-SHA256. It is written out here
// because the module cache of the station build has no golang.org/x/crypto.
func pbkdf2SHA256(password, salt []byte, rounds, keyLen int) []byte {
	if rounds < 1 {
		rounds = 1
	}
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	var counter [4]byte
	mac := hmac.New(sha256.New, password)
	for block := 1; block <= blocks; block++ {
		counter[0] = byte(block >> 24)
		counter[1] = byte(block >> 16)
		counter[2] = byte(block >> 8)
		counter[3] = byte(block)
		mac.Reset()
		mac.Write(salt)
		mac.Write(counter[:])
		u := mac.Sum(nil)
		t := make([]byte, hashLen)
		copy(t, u)
		for i := 1; i < rounds; i++ {
			mac.Reset()
			mac.Write(u)
			next := mac.Sum(nil)
			u = next
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// loadPanelCredential reads and checks the stored login.
func loadPanelCredential(path string) (panelCredential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return panelCredential{}, fmt.Errorf("panel needs a password: run `quasar-bridge -panel-passwd -config %s`", filepath.Base(path))
		}
		return panelCredential{}, fmt.Errorf("cannot read %s: %w", filepath.Base(path), err)
	}
	var c panelCredential
	dec := stdjson.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return panelCredential{}, fmt.Errorf("%s is not a panel credential", filepath.Base(path))
	}
	if c.Login == "" || c.Salt == "" || c.Hash == "" {
		return panelCredential{}, fmt.Errorf("%s is incomplete", filepath.Base(path))
	}
	if _, err := c.derive(""); err != nil {
		return panelCredential{}, fmt.Errorf("%s is damaged", filepath.Base(path))
	}
	return c, nil
}

// writePanelCredential stores the record atomically with 0600 permissions.
func writePanelCredential(path string, c panelCredential) error {
	raw, err := stdjson.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeFileAtomic(path, raw)
}

// panelSessions keeps the issued tokens in memory: a restart of the bridge logs
// every panel session out, which is the desired default for a home device.
type panelSessions struct {
	mu    sync.Mutex
	items map[string]time.Time
	now   func() time.Time
}

func newPanelSessions() *panelSessions {
	return &panelSessions{items: make(map[string]time.Time), now: time.Now}
}

func (s *panelSessions) issue() (string, time.Time) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := s.now().Add(panelSessionTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, deadline := range s.items {
		if now.After(deadline) {
			delete(s.items, id)
		}
	}
	s.items[token] = expires
	return token, expires
}

func (s *panelSessions) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, ok := s.items[token]
	if !ok {
		return false
	}
	if s.now().After(deadline) {
		delete(s.items, token)
		return false
	}
	return true
}

func (s *panelSessions) revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, token)
}

// panelLimiter counts failed logins per remote address: five misses inside five
// minutes turn into 429 until the oldest miss ages out.
type panelLimiter struct {
	mu      sync.Mutex
	entries map[string]panelFailures
	now     func() time.Time
}

type panelFailures struct {
	count int
	since time.Time
}

func newPanelLimiter() *panelLimiter {
	return &panelLimiter{entries: make(map[string]panelFailures), now: time.Now}
}

func (l *panelLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[ip]
	if !ok {
		return false
	}
	if l.now().Sub(entry.since) > panelFailureWindow {
		delete(l.entries, ip)
		return false
	}
	return entry.count >= panelFailureLimit
}

// fail records a miss and reports whether the address is now blocked.
func (l *panelLimiter) fail(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry, ok := l.entries[ip]
	if !ok || now.Sub(entry.since) > panelFailureWindow {
		entry = panelFailures{since: now}
	}
	entry.count++
	l.entries[ip] = entry
	return entry.count >= panelFailureLimit
}

func (l *panelLimiter) forget(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// panelClientIP is the address the login limiter counts: the TCP peer, never a
// header, because the panel is reached directly over the LAN.
func panelClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// panelSessionToken returns the session cookie value, if any.
func panelSessionToken(r *http.Request) string {
	cookie, err := r.Cookie(panelSessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func panelSetSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     panelSessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

func panelClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     panelSessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// setPanelPassword implements `-panel-passwd`: it reads the password from stdin
// when something is piped in and generates one otherwise, so the operator never
// has to put a password into a shell history or a process list.
func setPanelPassword(authPath, login string, stdin io.Reader, stdout io.Writer) error {
	password, generated, err := readPanelPassword(stdin)
	if err != nil {
		return err
	}
	credential, err := newPanelCredential(login, password)
	if err != nil {
		return err
	}
	if err := writePanelCredential(authPath, credential); err != nil {
		return fmt.Errorf("cannot write %s: %w", filepath.Base(authPath), err)
	}
	fmt.Fprintf(stdout, "panel login: %s\n", credential.Login)
	if generated {
		fmt.Fprintf(stdout, "panel password: %s\n", password)
		fmt.Fprintln(stdout, "(shown once: copy it now)")
	}
	fmt.Fprintf(stdout, "written: %s\n", authPath)
	return nil
}

// readPanelPassword takes the first line of stdin when stdin is a pipe or a
// file, and generates a random password when it is a terminal or empty.
func readPanelPassword(stdin io.Reader) (string, bool, error) {
	if file, ok := stdin.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return generatePanelPassword(), true, nil
		}
	}
	if stdin != nil {
		reader := newLineReader(stdin)
		line, err := reader.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			return line, false, nil
		}
	}
	return generatePanelPassword(), true, nil
}

// lineReader reads a single line without pulling in bufio for one call site.
type lineReader struct {
	reader io.Reader
	buf    [1]byte
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{reader: r} }

func (l *lineReader) readLine() (string, error) {
	var b strings.Builder
	for {
		n, err := l.reader.Read(l.buf[:])
		if n > 0 {
			if l.buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(l.buf[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
}

const panelPasswordAlphabet = "abcdefghijkmnopqrstuvwxyz23456789"

// generatePanelPassword returns 20 characters of a 32-letter alphabet (~100
// bits), avoiding the characters that are misread when copied by hand.
func generatePanelPassword() string {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand does not fail on Linux; keep a readable fallback anyway.
		return fmt.Sprintf("midi-%d", time.Now().UnixNano())
	}
	out := make([]byte, len(raw))
	for i, b := range raw {
		out[i] = panelPasswordAlphabet[int(b)%len(panelPasswordAlphabet)]
	}
	return string(out)
}

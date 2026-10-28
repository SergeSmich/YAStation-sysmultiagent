package main

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"quasarbridge/internal/journal"
)

// pbkdf2SHA256 must match the published RFC 8018 vectors, otherwise an operator
// could lock themselves out with a password that only this build accepts.
func TestPBKDF2SHA256Vectors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		salt     string
		rounds   int
		want     string
	}{
		{name: "one round", password: "password", salt: "salt", rounds: 1,
			want: "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{name: "two rounds", password: "password", salt: "salt", rounds: 2,
			want: "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{name: "four thousand", password: "password", salt: "salt", rounds: 4096,
			want: "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hex.EncodeToString(pbkdf2SHA256([]byte(tc.password), []byte(tc.salt), tc.rounds, 32))
			if got != tc.want {
				t.Fatalf("pbkdf2SHA256() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPanelCredentialVerify(t *testing.T) {
	credential, err := newPanelCredential("admin", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Rounds != panelDefaultRounds {
		t.Fatalf("rounds = %d", credential.Rounds)
	}
	if !credential.verify("admin", "correct horse") {
		t.Fatal("valid password rejected")
	}
	for _, tc := range []struct{ login, password string }{
		{"admin", "wrong horse"},
		{"adm1n", "correct horse"},
		{"", "correct horse"},
		{"admin", ""},
	} {
		if credential.verify(tc.login, tc.password) {
			t.Fatalf("verify(%q, %q) accepted", tc.login, tc.password)
		}
	}
	if _, err := newPanelCredential("admin", "short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestPanelCredentialFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel_auth.json")
	credential, err := newPanelCredential("admin", "long enough")
	if err != nil {
		t.Fatal(err)
	}
	if err := writePanelCredential(path, credential); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX permissions: chmod(0600) shows up as 0666 there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	loaded, err := loadPanelCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.verify("admin", "long enough") {
		t.Fatal("stored credential does not verify")
	}
	if _, err := loadPanelCredential(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestSetPanelPasswordReadsAndGenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel_auth.json")
	var out strings.Builder
	if err := setPanelPassword(path, "admin", strings.NewReader("from stdin\n"), &out); err != nil {
		t.Fatal(err)
	}
	credential, err := loadPanelCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	if !credential.verify("admin", "from stdin") {
		t.Fatal("password from stdin not stored")
	}
	if strings.Contains(out.String(), "from stdin") {
		t.Fatal("given password echoed back")
	}

	out.Reset()
	if err := setPanelPassword(path, "admin", strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	generated := out.String()
	if !strings.Contains(generated, "panel password: ") {
		t.Fatalf("generated password not printed: %q", generated)
	}
	if _, err := loadPanelCredential(path); err != nil {
		t.Fatal(err)
	}
}

func TestPanelLimiterBlocksAfterFiveFailures(t *testing.T) {
	limiter := newPanelLimiter()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return now }
	for i := 1; i <= 4; i++ {
		if limiter.fail("192.168.1.50") {
			t.Fatalf("blocked after %d failures", i)
		}
		if limiter.blocked("192.168.1.50") {
			t.Fatalf("blocked after %d failures", i)
		}
	}
	if !limiter.fail("192.168.1.50") {
		t.Fatal("fifth failure did not block")
	}
	if !limiter.blocked("192.168.1.50") {
		t.Fatal("blocked() false after five failures")
	}
	if limiter.blocked("192.168.1.51") {
		t.Fatal("another address blocked")
	}
	now = now.Add(panelFailureWindow + time.Second)
	if limiter.blocked("192.168.1.50") {
		t.Fatal("still blocked after the window")
	}
	limiter.fail("192.168.1.50")
	limiter.forget("192.168.1.50")
	if limiter.blocked("192.168.1.50") {
		t.Fatal("forget did not clear the entry")
	}
}

func TestPanelSessionsExpire(t *testing.T) {
	sessions := newPanelSessions()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	sessions.now = func() time.Time { return now }
	token, _ := sessions.issue()
	if token == "" || !sessions.valid(token) {
		t.Fatal("fresh session invalid")
	}
	now = now.Add(panelSessionTTL + time.Minute)
	if sessions.valid(token) {
		t.Fatal("expired session valid")
	}
	token, _ = sessions.issue()
	sessions.revoke(token)
	if sessions.valid(token) {
		t.Fatal("revoked session valid")
	}
	if sessions.valid("") {
		t.Fatal("empty token valid")
	}
}

func panelTestServer(t *testing.T) (*panelServer, panelCredential) {
	t.Helper()
	credential, err := newPanelCredential("admin", "test password")
	if err != nil {
		t.Fatal(err)
	}
	voice := new(voiceState)
	panel, err := newPanelServer(defaultPanelConfig(), credential,
		func() map[string]any { return map[string]any{"status": "ok", "mode": "hybrid"} },
		voice,
		func() journal.Config { return journal.Config{Enabled: false} })
	if err != nil {
		t.Fatal(err)
	}
	return panel, credential
}

func TestPanelRequiresSession(t *testing.T) {
	panel, _ := panelTestServer(t)
	for _, path := range []string{"/api/panel/state", "/api/panel/voice", "/api/panel/turns", "/api/panel/logs"} {
		recorder := httptest.NewRecorder()
		panel.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s = %d, want 401", path, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("index without session = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestPanelLoginFlow(t *testing.T) {
	panel, _ := panelTestServer(t)

	form := url.Values{"login": {"admin"}, "password": {"test password"}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "192.168.1.60:5555"
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("login = %d %s", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != panelSessionCookie {
		t.Fatalf("cookies = %+v", cookies)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags = %+v", cookie)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/panel/state", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state with session = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{"\"bridge\"", "\"voice\"", "\"supported\"", "omazh"} {
		if !strings.Contains(body, want) {
			t.Fatalf("state lacks %s: %s", want, body)
		}
	}

	request = httptest.NewRequest(http.MethodPost, "/logout", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/panel/state", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("state after logout = %d", recorder.Code)
	}
}

func TestPanelLoginRateLimit(t *testing.T) {
	panel, _ := panelTestServer(t)
	form := url.Values{"login": {"admin"}, "password": {"wrong"}}
	for i := 1; i <= passwordAttempts(); i++ {
		request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.RemoteAddr = "192.168.1.70:5555"
		recorder := httptest.NewRecorder()
		panel.ServeHTTP(recorder, request)
		want := http.StatusUnauthorized
		if i >= panelFailureLimit {
			want = http.StatusTooManyRequests
		}
		if recorder.Code != want {
			t.Fatalf("attempt %d = %d, want %d", i, recorder.Code, want)
		}
	}
}

func passwordAttempts() int { return panelFailureLimit }

func TestPanelRejectsOversizedLoginBody(t *testing.T) {
	panel, _ := panelTestServer(t)
	body := "login=admin&password=" + strings.Repeat("x", panelLoginBodyLimit)
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "192.168.1.80:5555"
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized login = %d", recorder.Code)
	}
}

func TestReadPanelTurnsAggregatesJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "turns.jsonl")
	lines := []string{
		`{"time":"2026-10-28T09:00:00Z","session":"s1","request":"r1","event":"request","text":"включи свет"}`,
		`{"time":"2026-10-28T09:00:01Z","session":"s1","request":"r1","event":"transcript","speech":"включи свет"}`,
		`{"time":"2026-10-28T09:00:01Z","session":"s1","request":"r1","event":"route","route":"llm"}`,
		`{"time":"2026-10-28T09:00:03Z","session":"s1","request":"r1","event":"response","text":"Готово","delivered":true,"elapsed_ms":2100}`,
		`not json`,
		`{"time":"2026-10-28T09:01:00Z","session":"s2","request":"r2","event":"request","text":"который час"}`,
		`{"time":"2026-10-28T09:01:02Z","session":"s2","request":"r2","event":"response","speech":"Полдень","delivered":false}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	turns, truncated, err := readPanelTurns(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("small journal reported as truncated")
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2: %+v", len(turns), turns)
	}
	newest := turns[0]
	if newest.Session != "s2" || newest.Answer != "Полдень" || newest.Delivered {
		t.Fatalf("newest turn = %+v", newest)
	}
	older := turns[1]
	if older.Session != "s1" || older.Route != "llm" || older.Speech != "включи свет" || older.Answer != "Готово" || !older.Delivered || older.ElapsedMS != 2100 {
		t.Fatalf("older turn = %+v", older)
	}

	limited, _, err := readPanelTurns(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Session != "s2" {
		t.Fatalf("limit not applied: %+v", limited)
	}
	if _, _, err := readPanelTurns(filepath.Join(dir, "missing.jsonl"), 10); err == nil {
		t.Fatal("missing journal accepted")
	}
}

func TestReadPanelLogFiltersAndTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.log")
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, `{"time":"2026-10-28T09:00:00Z","level":"INFO","msg":"frame","n":`+itoa(i)+`}`)
	}
	lines = append(lines, `{"time":"2026-10-28T09:00:00Z","level":"WARN","msg":"local_voice_unsupported"}`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	all, truncated, err := readPanelLog(path, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("small log reported as truncated")
	}
	if len(all) != 5 {
		t.Fatalf("lines = %d, want 5", len(all))
	}
	if !strings.Contains(all[len(all)-1], "local_voice_unsupported") {
		t.Fatalf("last line = %q", all[len(all)-1])
	}
	warn, _, err := readPanelLog(path, 50, "WARN")
	if err != nil {
		t.Fatal(err)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "local_voice_unsupported") {
		t.Fatalf("warn lines = %v", warn)
	}
	if _, _, err := readPanelLog(filepath.Join(dir, "missing.log"), 5, ""); err == nil {
		t.Fatal("missing log accepted")
	}
}

func TestPanelQueryInt(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", 50},
		{"?limit=10", 10},
		{"?limit=0", 1},
		{"?limit=1000", 200},
		{"?limit=abc", 50},
		{"?limit=-5", 50},
		{"?limit=12x", 50},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/panel/turns"+tc.query, nil)
		if got := panelQueryInt(request, "limit", 50, 1, 200); got != tc.want {
			t.Fatalf("panelQueryInt(%q) = %d, want %d", tc.query, got, tc.want)
		}
	}
}

func TestEnsurePanelTLSCoversLoopback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	certPath, keyPath, err := ensurePanelTLS(dir, []string{"192.168.1.210", "midi.local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{certPath, keyPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", filepath.Base(path), info.Mode().Perm())
		}
	}
	cert := readPanelCertificate(t, certPath)
	if cert.Subject.CommonName != "Midi panel" {
		t.Fatalf("common name = %q", cert.Subject.CommonName)
	}
	hosts := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		hosts[ip.String()] = true
	}
	for _, name := range cert.DNSNames {
		hosts[name] = true
	}
	for _, want := range []string{"127.0.0.1", "::1", "localhost", "192.168.1.210", "midi.local"} {
		if !hosts[want] {
			t.Fatalf("certificate lacks %s: %v", want, hosts)
		}
	}
	if cert.NotAfter.Before(time.Now().Add(4 * 365 * 24 * time.Hour)) {
		t.Fatalf("certificate expires too early: %s", cert.NotAfter)
	}

	// A second start must reuse the same files instead of rotating them.
	before, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensurePanelTLS(dir, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("certificate rotated on the second start")
	}
}

func readPanelCertificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestPanelPathsResolveNextToConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	configPath := filepath.Join(dir, "config.json")
	auth, tls, logs := panelPaths(configPath, defaultPanelConfig())
	if auth != filepath.Join(dir, "panel_auth.json") {
		t.Fatalf("auth = %s", auth)
	}
	if tls != filepath.Join(dir, "tls") {
		t.Fatalf("tls = %s", tls)
	}
	if logs != filepath.Join(dir, "bridge.log") {
		t.Fatalf("logs = %s", logs)
	}
	absolute := panelConfig{TLSDir: filepath.Join(dir, "certs"), AuthFile: "auth.json", LogPath: "b.log"}
	if _, tls, _ = panelPaths(configPath, absolute); tls != filepath.Join(dir, "certs") {
		t.Fatalf("absolute tls = %s", tls)
	}
}

// itoa avoids importing strconv just for the log fixture.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

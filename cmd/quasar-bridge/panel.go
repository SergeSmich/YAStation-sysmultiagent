package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	stdjson "encoding/json"
	"encoding/pem"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
)

// panelAssets carries the single-page UI: no build step, no network fetches,
// everything served from the bridge itself.
//
//go:embed panel.html panel_login.html panel.css panel.js manifest.json icons/icon.svg
var panelAssets embed.FS

// Bounds for the two read-only views. The panel must never load a whole log or
// a whole conversation file into the memory of a 1 GB station.
const (
	panelLogTailBytes    = 512 << 10
	panelJournalBytes    = 4 << 20
	panelLogLineLimit    = 400
	panelTurnLimit       = 200
	panelLoginBodyLimit  = 4 << 10
	panelCertificateDays = 5 * 365
)

// panelConfig switches on the LAN monitoring panel: a second, TLS-only listener
// that shows the bridge state, the recent turns and the log. Stage A was
// read-only; stage B adds the few actions worth having from a browser, each
// behind a session, a CSRF token and an audit record.
type panelConfig struct {
	Enabled     bool     `json:"enabled"`
	Listen      string   `json:"listen"`
	TLSDir      string   `json:"tls_dir"`
	AuthFile    string   `json:"auth_file"`
	AuditFile   string   `json:"audit_file"`
	LogPath     string   `json:"log_path"`
	MetricsFile string   `json:"metrics_file"`
	Hosts       []string `json:"hosts"`
	// WakeProxy exposes the wake-word daemon's own training page on a second
	// TLS listener with the same certificate and session. The daemon listens on
	// loopback only, and its page uses its own paths and inline styles, so a
	// separate listener is safer than rewriting paths on the main one.
	WakeProxy       bool   `json:"wake_proxy"`
	WakeProxyListen string `json:"wake_proxy_listen"`
}

func defaultPanelConfig() panelConfig {
	return panelConfig{
		Listen:          "0.0.0.0:8444",
		TLSDir:          "tls",
		AuthFile:        "panel_auth.json",
		AuditFile:       "panel_audit.jsonl",
		LogPath:         "bridge.log",
		MetricsFile:     "metrics.json",
		WakeProxy:       true,
		WakeProxyListen: "0.0.0.0:8445",
	}
}

// Validate checks the section only when the panel is enabled, so a disabled
// panel can keep the defaults.
func (c panelConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return errors.New("panel.listen must be host:port")
	}
	if strings.TrimSpace(c.TLSDir) == "" {
		return errors.New("panel.tls_dir must not be empty")
	}
	if strings.TrimSpace(c.AuthFile) == "" {
		return errors.New("panel.auth_file must not be empty")
	}
	if strings.TrimSpace(c.AuditFile) == "" {
		return errors.New("panel.audit_file must not be empty")
	}
	if strings.TrimSpace(c.LogPath) == "" {
		return errors.New("panel.log_path must not be empty")
	}
	if strings.TrimSpace(c.MetricsFile) == "" {
		return errors.New("panel.metrics_file must not be empty")
	}
	for _, host := range c.Hosts {
		if strings.TrimSpace(host) == "" {
			return errors.New("panel.hosts must name hosts")
		}
	}
	if c.WakeProxy {
		if _, _, err := net.SplitHostPort(c.WakeProxyListen); err != nil {
			return errors.New("panel.wake_proxy_listen must be host:port")
		}
	}
	return nil
}

// panelDeps are the pieces of the running bridge the panel is allowed to touch.
// The panel never opens its own WebSocket, journal writer or configuration
// parser: it reads the same snapshot as /healthz and writes through the same
// hot-apply path as PUT /api/voice-config.
type panelDeps struct {
	ConfigPath string
	AuthPath   string
	CAPath     string
	Auth       *panelAuthStore
	Snapshot   func() map[string]any
	Voice      *voiceState
	Journal    func() journal.Config
	Live       func() *bridge.Native
	Metrics    *panelMetrics
	Settings   settingsDeps
	// Decide runs one phrase through the live routing policy. The panel's test
	// box uses it, so the operator sees the same decision the next utterance
	// would get instead of a description of the rules.
	Decide func(context.Context, string) bridge.RouteDecision
	// SysHub is the sysadmin hub (hosts, policy, memory, approvals). May be nil.
	SysHub *sysadminHub
}

// panelServer is the second listener. It reads the same live state as /healthz
// through a snapshot closure, so the panel never opens a second WebSocket or a
// second journal writer.
type panelServer struct {
	auth       *panelAuthStore
	audit      *panelAudit
	authPath   string
	caPath     string
	configPath string
	sessions   *panelSessions
	limiter    *panelLimiter
	snapshot   func() map[string]any
	voice      *voiceState
	live       func() *bridge.Native
	journal    func() journal.Config
	metrics    *panelMetrics
	settings   settingsDeps
	decide     func(context.Context, string) bridge.RouteDecision
	sysHub     *sysadminHub
	updates    *updateChecker
	// stationVolume remembers the level the operator set, next to the config.
	stationVolume stationVolumeStore
	logPath       string
	login         *template.Template
	mux           *http.ServeMux
	// panelPort is the port of the main listener, used to send a session-less
	// visitor of the training page back to the panel login.
	panelPort string
	// wakePage is the proxied training page, nil when the extra listener is
	// disabled or its address is invalid.
	wakePage     http.Handler
	wakePagePort string
}

func newPanelServer(cfg panelConfig, deps panelDeps) (*panelServer, error) {
	loginPage, err := panelAssets.ReadFile("panel_login.html")
	if err != nil {
		return nil, err
	}
	login, err := template.New("login").Parse(string(loginPage))
	if err != nil {
		return nil, err
	}
	p := &panelServer{
		auth:          deps.Auth,
		audit:         newPanelAudit(cfg.AuditFile),
		authPath:      deps.AuthPath,
		caPath:        deps.CAPath,
		configPath:    deps.ConfigPath,
		sessions:      newPanelSessions(),
		limiter:       newPanelLimiter(),
		snapshot:      deps.Snapshot,
		voice:         deps.Voice,
		live:          deps.Live,
		journal:       deps.Journal,
		metrics:       deps.Metrics,
		settings:      deps.Settings,
		decide:        deps.Decide,
		sysHub:        deps.SysHub,
		updates:       newUpdateChecker(),
		logPath:       cfg.LogPath,
		login:         login,
		panelPort:     portOfListen(cfg.Listen),
		stationVolume: newStationVolumeStore(deps.ConfigPath),
	}
	// A panel built without a metrics collector still serves the endpoint: the
	// series is simply empty, and the charts draw "нет данных" instead of the
	// page failing to load.
	if p.metrics == nil {
		p.metrics = newPanelMetrics("")
	}
	if cfg.WakeProxy {
		if proxy, err := newWakePageProxy(wakeBaseURL()); err != nil {
			slog.Warn("wake_proxy_disabled", "error", err)
		} else {
			p.wakePage = p.wakePageHandler(proxy)
			p.wakePagePort = portOfListen(cfg.WakeProxyListen)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", p.index)
	mux.HandleFunc("GET /login", p.showLogin)
	mux.HandleFunc("POST /login", p.submitLogin)
	mux.HandleFunc("POST /logout", p.logout)
	mux.HandleFunc("GET /panel.css", p.asset("panel.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /panel.js", p.asset("panel.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /manifest.json", p.asset("manifest.json", "application/manifest+json"))
	mux.HandleFunc("GET /icons/icon.svg", p.asset("icons/icon.svg", "image/svg+xml"))
	// Deliberately outside requireSession: the operator downloads the root
	// certificate from a page the browser still calls untrusted, before logging in.
	mux.HandleFunc("GET /ca.crt", p.rootCertificate)
	mux.HandleFunc("GET /api/panel/session", p.requireSession(p.apiSession))
	mux.HandleFunc("GET /api/panel/state", p.requireSession(p.apiState))
	mux.HandleFunc("GET /api/panel/voice", p.requireSession(p.apiVoice))
	mux.HandleFunc("GET /api/panel/settings", p.requireSession(p.apiSettings))
	mux.HandleFunc("GET /api/panel/wake/dictionary", p.requireSession(p.apiWakeDictionary))
	mux.HandleFunc("GET /api/panel/turns", p.requireSession(p.apiTurns))
	mux.HandleFunc("GET /api/panel/logs", p.requireSession(p.apiLogs))
	mux.HandleFunc("GET /api/panel/metrics", p.requireSession(p.apiMetrics))
	mux.HandleFunc("POST /api/panel/voice", p.requireMutation(p.apiVoiceUpdate))
	mux.HandleFunc("POST /api/panel/settings", p.requireMutation(p.apiSettingsUpdate))
	mux.HandleFunc("POST /api/panel/llm/test", p.requireMutation(p.apiLLMTest))
	mux.HandleFunc("POST /api/panel/llm/models", p.requireMutation(p.apiLLMModels))
	mux.HandleFunc("POST /api/panel/llm/detect", p.requireMutation(p.apiLLMDetect))
	mux.HandleFunc("POST /api/panel/llm/check", p.requireMutation(p.apiLLMCheck))
	mux.HandleFunc("POST /api/panel/llm/ask", p.requireMutation(p.apiLLMAsk))
	mux.HandleFunc("POST /api/panel/llm/chat", p.requireMutation(p.apiLLMChat))
	mux.HandleFunc("POST /api/panel/voice/test", p.requireMutation(p.apiVoiceTest))
	mux.HandleFunc("POST /api/panel/password", p.requireMutation(p.apiPassword))
	mux.HandleFunc("POST /api/panel/routing/test", p.requireMutation(p.apiRoutingTest))
	mux.HandleFunc("GET /api/panel/update", p.requireSession(p.apiUpdate))
	mux.HandleFunc("GET /api/panel/backup", p.requireSession(p.apiBackup))
	mux.HandleFunc("GET /api/panel/station/volume", p.requireSession(p.apiStationVolume))
	mux.HandleFunc("POST /api/panel/station/volume", p.requireMutation(p.apiStationVolumeUpdate))
	mux.HandleFunc("GET /api/panel/sysadmin/state", p.requireSession(p.apiSysadminState))
	mux.HandleFunc("POST /api/panel/sysadmin/approve", p.requireMutation(p.apiSysadminApprove))
	mux.HandleFunc("POST /api/panel/sysadmin/host", p.requireMutation(p.apiSysadminHost))
	mux.HandleFunc("POST /api/panel/sysadmin/host/delete", p.requireMutation(p.apiSysadminHostDelete))
	mux.HandleFunc("POST /api/panel/sysadmin/policy", p.requireMutation(p.apiSysadminPolicy))
	mux.HandleFunc("POST /api/panel/sysadmin/charter", p.requireMutation(p.apiSysadminCharter))
	mux.HandleFunc("POST /api/panel/sysadmin/memory/forget", p.requireMutation(p.apiSysadminMemoryForget))
	mux.HandleFunc("POST /api/panel/sysadmin/passport", p.requireMutation(p.apiSysadminPassport))
	mux.HandleFunc("POST /api/panel/sysadmin/telegram", p.requireMutation(p.apiSysadminTelegram))
	p.mux = mux
	return p, nil
}

// ServeHTTP adds the headers a small self-hosted UI should always send. The
// policy forbids inline script and inline style, so the page ships them as
// separate embedded files.
func (p *panelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; manifest-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	p.mux.ServeHTTP(w, r)
}

// WakePageHandler is the proxied wake-word training page. The extra listener is
// started only when this is not nil.
func (p *panelServer) WakePageHandler() http.Handler { return p.wakePage }

// portOfListen extracts the port from a host:port address, so a page served on
// another port can link back to this listener.
func portOfListen(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return ""
}

func (p *panelServer) index(w http.ResponseWriter, r *http.Request) {
	if !p.sessions.valid(panelSessionToken(r)) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	page, err := panelAssets.ReadFile("panel.html")
	if err != nil {
		http.Error(w, "panel is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(versionedAssets(page))
}

// versionedAssets adds ?v=<hash of the file> to the script and style links. The
// files are cached for an hour, so without it a browser keeps the old script
// next to a new page after an update (the page itself is never cached).
func versionedAssets(page []byte) []byte {
	out := string(page)
	for _, name := range []string{"panel.js", "panel.css"} {
		data, err := panelAssets.ReadFile(name)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		out = strings.ReplaceAll(out, `"/`+name+`"`, `"/`+name+`?v=`+hex.EncodeToString(sum[:4])+`"`)
	}
	return []byte(out)
}

func (p *panelServer) asset(name, contentType string) http.HandlerFunc {
	data, err := panelAssets.ReadFile(name)
	return func(w http.ResponseWriter, _ *http.Request) {
		if err != nil {
			http.NotFound(w, nil)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(data)
	}
}

// panelCAName is the root certificate the panel's own certificate chains to. It
// sits next to panel.crt/panel.key in the TLS directory; the bridge only serves
// it and never creates one — a locally trusted CA is the operator's decision.
const panelCAName = "panel-ca.crt"

// rootCertificate hands that CA to the browser as a download. Chromium keeps the
// password manager off while a site's certificate is untrusted, so installing
// this file into the OS trust store is what makes "сохранить пароль" appear at
// all; the page itself already posts the form natively.
func (p *panelServer) rootCertificate(w http.ResponseWriter, _ *http.Request) {
	data, err := os.ReadFile(p.caPath)
	if p.caPath == "" || err != nil {
		http.Error(w, "root certificate is not configured", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="midi-ca.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (p *panelServer) showLogin(w http.ResponseWriter, r *http.Request) {
	if p.sessions.valid(panelSessionToken(r)) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p.renderLogin(w, http.StatusOK, "")
}

func (p *panelServer) submitLogin(w http.ResponseWriter, r *http.Request) {
	ip := panelClientIP(r)
	if p.limiter.blocked(ip) {
		p.renderLogin(w, http.StatusTooManyRequests, "Слишком много неудачных попыток. Подождите несколько минут.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelLoginBodyLimit)
	if err := r.ParseForm(); err != nil {
		p.renderLogin(w, http.StatusBadRequest, "Некорректный запрос.")
		return
	}
	login := strings.TrimSpace(r.PostFormValue("login"))
	password := r.PostFormValue("password")
	if !p.auth.verify(login, password) {
		blocked := p.limiter.fail(ip)
		slog.Warn("panel_login_failed", "ip", ip, "blocked", blocked)
		p.audit.record("login", r, "denied", map[string]any{"login": login})
		if blocked {
			p.renderLogin(w, http.StatusTooManyRequests, "Слишком много неудачных попыток. Подождите несколько минут.")
			return
		}
		p.renderLogin(w, http.StatusUnauthorized, "Неверный логин или пароль.")
		return
	}
	token, _, expires := p.sessions.issue()
	if token == "" {
		p.renderLogin(w, http.StatusInternalServerError, "Не удалось создать сессию.")
		return
	}
	p.limiter.forget(ip)
	panelSetSessionCookie(w, token, expires)
	p.audit.record("login", r, "ok", map[string]any{"login": login})
	slog.Info("panel_login_ok", "ip", ip, "login", login)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (p *panelServer) logout(w http.ResponseWriter, r *http.Request) {
	p.sessions.revoke(panelSessionToken(r))
	panelClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (p *panelServer) renderLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = p.login.Execute(w, struct{ Error string }{Error: message})
}

func (p *panelServer) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.sessions.valid(panelSessionToken(r)) {
			writePanelJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (p *panelServer) apiState(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"bridge": p.snapshot(),
		"voice":  map[string]any{"local_voice": p.voice.Load(), "revision": p.voice.Revision(), "supported": bridge.KnownVoices},
		"panel":  map[string]any{"log_path": p.logPath, "now": time.Now().Format(time.RFC3339)},
	}
	writePanelJSON(w, http.StatusOK, payload)
}

func (p *panelServer) apiVoice(w http.ResponseWriter, _ *http.Request) {
	writePanelJSON(w, http.StatusOK, map[string]any{
		"local_voice": p.voice.Load(),
		"revision":    p.voice.Revision(),
		"supported":   bridge.KnownVoices,
	})
}

// apiTurns aggregates the journal into one row per spoken turn. The panel shows
// the same records the LLM history tool reads, so what the operator sees is
// what the assistant remembered.
func (p *panelServer) apiTurns(w http.ResponseWriter, r *http.Request) {
	limit := panelQueryInt(r, "limit", 50, 1, panelTurnLimit)
	cfg := p.journal()
	if !cfg.Enabled {
		writePanelJSON(w, http.StatusOK, map[string]any{"enabled": false, "turns": []panelTurn{}})
		return
	}
	turns, truncated, err := readPanelTurns(cfg.Path, limit)
	if err != nil {
		writePanelJSON(w, http.StatusOK, map[string]any{"enabled": true, "turns": []panelTurn{}, "error": "журнал недоступен: " + filepath.Base(cfg.Path)})
		return
	}
	writePanelJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"path":      cfg.Path,
		"turns":     turns,
		"truncated": truncated,
	})
}

func (p *panelServer) apiLogs(w http.ResponseWriter, r *http.Request) {
	limit := panelQueryInt(r, "lines", 120, 1, panelLogLineLimit)
	level := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("level")))
	lines, truncated, err := readPanelLog(p.logPath, limit, level)
	if err != nil {
		writePanelJSON(w, http.StatusOK, map[string]any{"path": p.logPath, "lines": []string{}, "error": "лог недоступен"})
		return
	}
	writePanelJSON(w, http.StatusOK, map[string]any{
		"path":      p.logPath,
		"level":     level,
		"lines":     lines,
		"truncated": truncated,
	})
}

// panelTurn is one spoken exchange, assembled from several journal events that
// share the session id and the request id.
type panelTurn struct {
	Time      time.Time `json:"time"`
	Session   string    `json:"session,omitempty"`
	Route     string    `json:"route,omitempty"`
	Speech    string    `json:"speech,omitempty"`
	Answer    string    `json:"answer,omitempty"`
	Delivered bool      `json:"delivered,omitempty"`
	ElapsedMS int64     `json:"elapsed_ms,omitempty"`
}

// readPanelTurns parses the tail of the NDJSON journal, newest turn first.
func readPanelTurns(path string, limit int) ([]panelTurn, bool, error) {
	lines, truncated, err := readTailLines(path, panelJournalBytes)
	if err != nil {
		return nil, false, err
	}
	var order []string
	byKey := make(map[string]*panelTurn)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event journal.Event
		if err := stdjson.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		key := event.Session + "\x00" + event.Request
		switch event.Event {
		case "request":
			turn := &panelTurn{Time: event.Time, Session: event.Session}
			byKey[key] = turn
			order = append(order, key)
		case "transcript":
			if turn := byKey[key]; turn != nil {
				if event.Speech != "" {
					turn.Speech = event.Speech
				} else if event.Text != "" {
					turn.Speech = event.Text
				}
			}
		case "route":
			if turn := byKey[key]; turn != nil && event.Route != "" {
				turn.Route = event.Route
			}
		case "response", "cloud_response":
			turn := byKey[key]
			if turn == nil {
				continue
			}
			if event.Text != "" {
				turn.Answer = event.Text
			} else if event.Speech != "" {
				turn.Answer = event.Speech
			}
			turn.Delivered = event.Delivered
			if event.ElapsedMS > 0 {
				turn.ElapsedMS = event.ElapsedMS
			}
		}
	}
	turns := make([]panelTurn, 0, limit)
	for i := len(order) - 1; i >= 0 && len(turns) < limit; i-- {
		turn := byKey[order[i]]
		if turn == nil {
			continue
		}
		turns = append(turns, *turn)
	}
	return turns, truncated, nil
}

// readPanelLog returns the last lines of the bridge log, optionally filtered to
// one slog level. Non-JSON lines written by the run.sh wrapper are kept unmatched
// only in the unfiltered view.
func readPanelLog(path string, limit int, level string) ([]string, bool, error) {
	lines, truncated, err := readTailLines(path, panelLogTailBytes)
	if err != nil {
		return nil, false, err
	}
	selected := make([]string, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(selected) < limit; i-- {
		line := lines[i]
		if level != "" && !strings.Contains(strings.ToUpper(line), "\"LEVEL\":\""+level+"\"") && !strings.Contains(strings.ToUpper(line), level) {
			continue
		}
		selected = append(selected, line)
	}
	// Reverse back into file order: the UI reads top-down.
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return selected, truncated, nil
}

// readTailLines returns the lines of the last maxBytes of a file. The boolean
// reports that older lines exist outside that window.
func readTailLines(path string, maxBytes int64) ([]string, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.IsDir() {
		return nil, false, errors.New("not a file")
	}
	size := info.Size()
	start := int64(0)
	truncated := false
	if size > maxBytes {
		start = size - maxBytes
		truncated = true
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil, false, err
	}
	lines := strings.Split(string(data), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line is likely cut in half
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, truncated, nil
}

func panelQueryInt(r *http.Request, name string, fallback, low, high int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value := 0
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return fallback
		}
		value = value*10 + int(digit-'0')
		if value > high {
			return high
		}
	}
	if value < low {
		return low
	}
	return value
}

func writePanelJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := stdjson.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

// ensurePanelTLS generates a self-signed certificate on first start and reuses
// it afterwards, so the browser warning stays stable across restarts.
func ensurePanelTLS(dir string, extraHosts []string) (string, string, error) {
	certPath := filepath.Join(dir, "panel.crt")
	keyPath := filepath.Join(dir, "panel.key")
	certOK := panelRegularFile(certPath)
	keyOK := panelRegularFile(keyPath)
	if certOK && keyOK {
		return certPath, keyPath, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if err := generatePanelCertificate(certPath, keyPath, panelTLSHosts(extraHosts)); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func panelRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func generatePanelCertificate(certPath, keyPath string, hosts []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Midi panel", Organization: []string{"quasar-bridge"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 0, panelCertificateDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := writeFileAtomic(keyPath, keyPEM); err != nil {
		return err
	}
	return writeFileAtomic(certPath, certPEM)
}

// panelTLSHosts lists every name the certificate should cover: the loopback, the
// station hostname, every interface address and the configured extras. The
// browser is then happy with whichever address the operator typed.
func panelTLSHosts(extra []string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if name, err := os.Hostname(); err == nil && strings.TrimSpace(name) != "" {
		hosts = append(hosts, name)
	}
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP != nil {
					hosts = append(hosts, ipnet.IP.String())
				}
			}
		}
	}
	hosts = append(hosts, extra...)
	seen := make(map[string]bool, len(hosts))
	unique := hosts[:0]
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		unique = append(unique, host)
	}
	return unique
}

// panelFiles are the state files the panel owns, all resolved next to the
// configuration file.
type panelFiles struct {
	Auth    string
	TLS     string
	Log     string
	Audit   string
	Metrics string
}

// panelPaths resolves the panel files next to the configuration file, so the
// process can be started from any working directory.
func panelPaths(configPath string, cfg panelConfig) panelFiles {
	dir := filepath.Dir(configPath)
	resolve := func(value string) string {
		if filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(dir, value)
	}
	return panelFiles{
		Auth:    resolve(cfg.AuthFile),
		TLS:     resolve(cfg.TLSDir),
		Log:     resolve(cfg.LogPath),
		Audit:   resolve(cfg.AuditFile),
		Metrics: resolve(cfg.MetricsFile),
	}
}

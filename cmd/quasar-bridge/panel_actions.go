package main

import (
	"context"
	"crypto/subtle"
	stdjson "encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"quasarbridge/internal/bridge"
)

// panelCSRFHeader is the header the page sends on every mutating request. The
// token is bound to the session and is never written to a cookie, so a form
// posted by another site cannot produce it.
const panelCSRFHeader = "X-Panel-CSRF"

// panelAuditLimit caps the audit file: when it grows past this size the previous
// one is kept as `panel_audit.jsonl.1`. Old records are not precious, but recent
// actions should survive.
const panelAuditLimit = 1 << 20

// panelMutationBodyLimit bounds the JSON a mutating request may carry.
const panelMutationBodyLimit = 16 << 10

// panelAudit appends one NDJSON line per mutating action. A failure to write the
// audit log is reported but never fails the action itself.
type panelAudit struct {
	mu   sync.Mutex
	path string
}

func newPanelAudit(path string) *panelAudit { return &panelAudit{path: path} }

func (a *panelAudit) record(action string, r *http.Request, result string, extra map[string]any) {
	if a == nil || strings.TrimSpace(a.path) == "" {
		return
	}
	entry := make(map[string]any, len(extra)+4)
	entry["time"] = time.Now().Format(time.RFC3339)
	entry["action"] = action
	entry["result"] = result
	if r != nil {
		entry["ip"] = panelClientIP(r)
	}
	for key, value := range extra {
		entry[key] = value
	}
	raw, err := stdjson.Marshal(entry)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if info, err := os.Stat(a.path); err == nil && info.Size() > panelAuditLimit {
		_ = os.Remove(a.path + ".1")
		_ = os.Rename(a.path, a.path+".1")
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		slog.Warn("panel_audit_failed", "error", err)
		return
	}
	if _, err := file.Write(raw); err != nil {
		slog.Warn("panel_audit_failed", "error", err)
	}
	_ = file.Close()
}

func panelTokensEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// panelSameOrigin rejects a mutating request that a browser marked as coming
// from another site. The CSRF token is the primary defence; this closes the
// cases where a token leaks through a Referer or a proxy header.
func panelSameOrigin(r *http.Request) bool {
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" {
			return false
		}
		return strings.EqualFold(parsed.Host, r.Host)
	}
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" {
		return site == "same-origin" || site == "none"
	}
	return true
}

// requireMutation wraps a handler that changes something: a live session is not
// enough, the request must also come from the panel itself and carry the CSRF
// token issued with that session.
func (p *panelServer) requireMutation(next http.HandlerFunc) http.HandlerFunc {
	return p.requireSession(func(w http.ResponseWriter, r *http.Request) {
		if !panelSameOrigin(r) {
			p.audit.record("request_rejected", r, "origin", map[string]any{"path": r.URL.Path})
			writePanelJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
			return
		}
		if !panelTokensEqual(p.sessions.csrf(panelSessionToken(r)), r.Header.Get(panelCSRFHeader)) {
			p.audit.record("request_rejected", r, "csrf", map[string]any{"path": r.URL.Path})
			writePanelJSON(w, http.StatusForbidden, map[string]string{"error": "invalid csrf token"})
			return
		}
		next(w, r)
	})
}

// apiSession hands the page its CSRF token and the login name. The token lives
// only in the page's memory: it is not a cookie, so another origin cannot read
// it even when the session cookie is sent automatically.
func (p *panelServer) apiSession(w http.ResponseWriter, r *http.Request) {
	csrf := p.sessions.csrf(panelSessionToken(r))
	if csrf == "" {
		writePanelJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writePanelJSON(w, http.StatusOK, map[string]any{
		"csrf":  csrf,
		"login": p.auth.current().Login,
	})
}

// apiVoiceUpdate is the panel's write path to local_voice. It shares
// applyVoicePatch with PUT /api/voice-config, so the file handling, validation,
// backup and hot-apply are identical; only the transport differs.
func (p *panelServer) apiVoiceUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, panelMutationBodyLimit)
	var patch voicePatch
	if err := stdjson.NewDecoder(r.Body).Decode(&patch); err != nil {
		p.audit.record("voice_update", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	status, payload := applyVoicePatch(p.configPath, p.voice, patch, p.live)
	loaded := p.voice.Load()
	p.audit.record("voice_update", r, panelAuditResult(status), map[string]any{
		"status":     status,
		"voice":      loaded.Voice,
		"candidates": loaded.Candidates,
		"volume":     loaded.Volume,
		"speed":      loaded.Speed,
	})
	writePanelJSON(w, status, payload)
}

// apiVoiceTest synthesizes one sample through the live upstream session. With
// `play` the sample is forwarded to the station, so the voice is heard where it
// will actually speak; without it the probe only checks that the voice is
// accepted by the synthesizer.
func (p *panelServer) apiVoiceTest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, panelMutationBodyLimit)
	var body struct {
		Voice     string  `json:"voice"`
		Text      string  `json:"text"`
		Play      bool    `json:"play"`
		PerCaseMS int64   `json:"per_case_ms"`
		Volume    float64 `json:"volume"`
		Speed     float64 `json:"speed"`
	}
	if err := stdjson.NewDecoder(r.Body).Decode(&body); err != nil {
		p.audit.record("voice_test", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	native := p.live()
	if native == nil {
		p.audit.record("voice_test", r, "offline", nil)
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no upstream session"})
		return
	}
	current := p.voice.Load()
	voiceCase := bridge.VoiceProbeCase{
		Voice:   strings.TrimSpace(body.Voice),
		Emotion: current.Emotion,
		Speed:   current.Speed,
		Text:    panelProbeText(body.Text),
		Play:    body.Play,
	}
	if voiceCase.Voice == "" {
		voiceCase.Voice = current.Voice
	}
	if body.Volume >= 0.1 && body.Volume <= 3 {
		voiceCase.Volume = body.Volume
	} else {
		voiceCase.Volume = current.Volume
	}
	if body.Speed >= 0.1 && body.Speed <= 3 {
		voiceCase.Speed = body.Speed
	}
	if body.Play {
		// Heard on the station: the phrase is read as an answer of the station's
		// own turn, because the speaker plays nothing it did not ask for.
		if !bridge.IsKnownVoice(voiceCase.Voice) {
			p.audit.record("voice_test", r, "bad_request", map[string]any{"voice": voiceCase.Voice})
			writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown voice"})
			return
		}
		started := time.Now()
		_, err := native.Audition(context.WithoutCancel(r.Context()), voiceCase.Text, bridge.VoiceSettings{
			Voice: voiceCase.Voice, Emotion: voiceCase.Emotion, Speed: voiceCase.Speed, Volume: voiceCase.Volume,
		})
		result := bridge.VoiceProbeResult{VoiceProbeCase: voiceCase, OK: err == nil, ElapsedMS: time.Since(started).Milliseconds()}
		outcome := "ok"
		if err != nil {
			result.Error = err.Error()
			outcome = "error"
		}
		p.audit.record("voice_test", r, outcome, map[string]any{"voice": voiceCase.Voice, "play": true})
		writePanelJSON(w, http.StatusOK, map[string]any{"results": []bridge.VoiceProbeResult{result}, "count": 1, "play": true})
		return
	}
	// Probe itself caps a case at 5 s; keep the panel inside the same window so
	// the reported elapsed time matches what the synthesizer was allowed to use.
	perCase := time.Duration(body.PerCaseMS) * time.Millisecond
	if perCase < time.Second || perCase > 5*time.Second {
		perCase = 5 * time.Second
	}
	results, err := native.Probe(r.Context(), []bridge.VoiceProbeCase{voiceCase}, perCase)
	if err != nil {
		p.audit.record("voice_test", r, "error", map[string]any{"voice": voiceCase.Voice, "play": voiceCase.Play, "error": err.Error()})
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ok := len(results) == 1 && results[0].OK
	result := "error"
	if ok {
		result = "ok"
	}
	p.audit.record("voice_test", r, result, map[string]any{"voice": voiceCase.Voice, "play": voiceCase.Play})
	writePanelJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results), "play": voiceCase.Play})
}

// panelProbeText keeps the synthesized phrase short: the station is a small
// device and the probe blocks the synthesizer for the duration of the sample.
func panelProbeText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "Привет, это Миди. Так звучит мой голос."
	}
	runes := []rune(text)
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return string(runes)
}

// apiPassword replaces the panel credential. The current password is required
// again even though the session is valid: a walked-away browser must not let a
// stranger take the panel over.
func (p *panelServer) apiPassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, panelMutationBodyLimit)
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := stdjson.NewDecoder(r.Body).Decode(&body); err != nil {
		p.audit.record("password_change", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	credential := p.auth.current()
	if !credential.verify(credential.Login, body.Current) {
		// A wrong current password counts as a failed login for the limiter, so
		// a stolen session cannot be used to brute-force the old one quickly.
		blocked := p.limiter.fail(panelClientIP(r))
		p.audit.record("password_change", r, "denied", map[string]any{"blocked": blocked})
		writePanelJSON(w, http.StatusForbidden, map[string]string{"error": "текущий пароль неверен"})
		return
	}
	next, err := newPanelCredential(credential.Login, body.Next)
	if err != nil {
		p.audit.record("password_change", r, "rejected", map[string]any{"error": err.Error()})
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := writePanelCredential(p.authPath, next); err != nil {
		p.audit.record("password_change", r, "error", map[string]any{"error": err.Error()})
		writePanelJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось записать пароль"})
		return
	}
	if err := p.auth.reload(); err != nil {
		p.audit.record("password_change", r, "error", map[string]any{"error": err.Error()})
		writePanelJSON(w, http.StatusInternalServerError, map[string]string{"error": "пароль записан, но не перечитан"})
		return
	}
	p.sessions.revokeAllExcept(panelSessionToken(r))
	p.limiter.forget(panelClientIP(r))
	p.audit.record("password_change", r, "ok", map[string]any{"login": credential.Login})
	writePanelJSON(w, http.StatusOK, map[string]any{"ok": true, "login": credential.Login})
}

func panelAuditResult(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "ok"
	case status == http.StatusBadRequest:
		return "rejected"
	default:
		return "error"
	}
}

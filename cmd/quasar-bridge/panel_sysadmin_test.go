package main
import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"

	"testing"
)

func newSysadminTestPanel(t *testing.T) *panelServer {
	t.Helper()
	dir := t.TempDir()
	hub := NewSysadminHub(dir, nil)
	p := &panelServer{sysHub: hub, sessions: newPanelSessions(), audit: newPanelAudit("")}
	return p
}

func newSysadminSession(t *testing.T, p *panelServer) (token, csrf string) {
	t.Helper()
	token, csrf, _ = p.sessions.issue()
	return
}
func TestSysadminStateUnauthorized(t *testing.T) {
	p := newSysadminTestPanel(t)
	h := p.requireSession(p.apiSysadminState)
	req := httptest.NewRequest(http.MethodGet, "/api/panel/sysadmin/state", nil)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rr.Code)
	}
}
func TestSysadminTelegramNoCSRF(t *testing.T) {
	p := newSysadminTestPanel(t)
	token, _ := newSysadminSession(t, p)
	h := p.requireMutation(p.apiSysadminTelegram)
	b, _ := json.Marshal(map[string]any{"enabled": false, "users": []int64{1}, "token": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/panel/sysadmin/telegram", bytes.NewBuffer(b))
	req.AddCookie(&http.Cookie{Name: "midi_panel", Value: token})
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rr.Code)
	}
}

func TestSysadminTelegramNoSession(t *testing.T) {
	p := newSysadminTestPanel(t)
	h := p.requireMutation(p.apiSysadminTelegram)
	b, _ := json.Marshal(map[string]any{"enabled": false, "users": []int64{1}, "token": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/panel/sysadmin/telegram", bytes.NewBuffer(b))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rr.Code)
	}
}

func TestSysadminTelegramSecretNotLeaked(t *testing.T) {
	dir := t.TempDir()
	hub := NewSysadminHub(dir, nil)
	p := &panelServer{sysHub: hub, sessions: newPanelSessions(), audit: newPanelAudit(dir + string(os.PathSeparator) + "panel_audit.jsonl")}
	token, csrf := newSysadminSession(t, p)
	secret := "SECRET-TOKEN-123:ABC"
	b, _ := json.Marshal(map[string]any{"enabled": true, "users": []int64{1}, "token": secret})
	h := p.requireMutation(p.apiSysadminTelegram)
	req := httptest.NewRequest(http.MethodPost, "/api/panel/sysadmin/telegram", bytes.NewBuffer(b))
	req.AddCookie(&http.Cookie{Name: "midi_panel", Value: token})
	req.Header.Set("X-Panel-CSRF", csrf)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("post code=%d body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(secret)) {
		t.Fatalf("token leaked in POST response")
	}
	h2 := p.requireSession(p.apiSysadminState)
	req2 := httptest.NewRequest(http.MethodGet, "/api/panel/sysadmin/state", nil)
	req2.AddCookie(&http.Cookie{Name: "midi_panel", Value: token})
	rr2 := httptest.NewRecorder()
	h2(rr2, req2)
	if bytes.Contains(rr2.Body.Bytes(), []byte(secret)) {
		t.Fatalf("token leaked in state")
	}
	raw, _ := os.ReadFile(dir + string(os.PathSeparator) + "sysadmin.json")
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("token leaked in sysadmin.json")
	}
	sec, _ := os.ReadFile(dir + string(os.PathSeparator) + "sysadmin_secrets.json")
	if !bytes.Contains(sec, []byte(secret)) {
		t.Fatalf("token not in secrets file")
	}
	aud, _ := os.ReadFile(dir + string(os.PathSeparator) + "panel_audit.jsonl")
	if bytes.Contains(aud, []byte(secret)) {
		t.Fatalf("token leaked in audit")
	}
}

package main

import (
	stdjson "encoding/json"
	"net/http"

	"quasarbridge/internal/sysadmin"
)

const panelSysadminBodyLimit = 64 << 10

// panelAuditAuditor adapts *panelAudit to sysadmin.Auditor.
type panelAuditAuditor struct{ a *panelAudit }

func (x panelAuditAuditor) Audit(event string, fields map[string]any) {
	if x.a == nil {
		return
	}
	x.a.record(event, nil, "ok", fields)
}

func (p *panelServer) apiSysadminState(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	hosts := p.sysHub.Hosts()
	// strip passwords: hosts config has no password field; secrets separate.
	state := map[string]any{
		"hosts":    hosts,
		"queue":    p.sysHub.Queue().Pending(),
		"recent":   p.sysHub.Queue().Recent(10),
		"policy":   p.sysHub.Policy(),
		"telegram": p.sysHub.Telegram(),
		"passport": p.sysHub.Memory().Passport(),
		"facts":    p.sysHub.Memory().Recall("", 50),
		"error":    errString(p.sysHub.Err()),
	}
	writePanelJSON(w, http.StatusOK, state)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (p *panelServer) apiSysadminApprove(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelSysadminBodyLimit)
	var body struct {
		ID    string `json:"id"`
		Allow bool   `json:"allow"`
		Scope string `json:"scope"`
	}
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	scope := sysadmin.ScopeDeny
	if body.Scope != "" {
		s, ok := sysadmin.ParseScope(body.Scope)
		if !ok {
			writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown scope"})
			return
		}
		scope = s
	} else if body.Allow {
		scope = sysadmin.ScopeOnce
	}
	item, err := p.sysHub.Queue().DecideScope(body.ID, scope, "panel")
	if err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p.sysHub.Audit("sysadmin_approve", map[string]any{"id": body.ID, "scope": string(scope)})
	writePanelJSON(w, http.StatusOK, map[string]any{"item": item})
}

func (p *panelServer) apiSysadminHost(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelSysadminBodyLimit)
	var body struct {
		Name          string `json:"name"`
		Addr          string `json:"addr"`
		Port          int    `json:"port"`
		User          string `json:"user"`
		KeyPath       string `json:"key_path"`
		MAC           string `json:"mac"`
		Password      string `json:"password"`
		ClearPassword bool   `json:"clear_password"`
	}
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	hc := sysadminHostCfg{
		Name:    body.Name,
		Addr:    body.Addr,
		Port:    body.Port,
		User:    body.User,
		KeyPath: body.KeyPath,
		MAC:     body.MAC,
	}
	if err := p.sysHub.SaveHost(hc); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if body.ClearPassword {
		if err := p.sysHub.SetHostPassword(body.Name, ""); err != nil {
			writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else if body.Password != "" {
		if err := p.sysHub.SetHostPassword(body.Name, body.Password); err != nil {
			writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	p.sysHub.Audit("sysadmin_host_save", map[string]any{"name": body.Name})
	writePanelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *panelServer) apiSysadminHostDelete(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelSysadminBodyLimit)
	var body struct {
		Name string `json:"name"`
	}
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := p.sysHub.DeleteHost(body.Name); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p.sysHub.Audit("sysadmin_host_delete", map[string]any{"name": body.Name})
	writePanelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *panelServer) apiSysadminPolicy(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelSysadminBodyLimit)
	var body struct {
		Default string `json:"default"`
		Rules   []struct {
			Tool  string `json:"tool"`
			Match string `json:"match"`
			Mode  string `json:"mode"`
		} `json:"rules"`
	}
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	pol := sysadmin.Policy{Default: sysadmin.Mode(body.Default)}
	for _, rr := range body.Rules {
		pol.Rules = append(pol.Rules, sysadmin.Rule{Tool: rr.Tool, Match: rr.Match, Mode: sysadmin.Mode(rr.Mode)})
	}
	if err := p.sysHub.SetPolicy(pol); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p.sysHub.Audit("sysadmin_policy", map[string]any{"default": body.Default})
	writePanelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *panelServer) apiSysadminMemoryForget(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelSysadminBodyLimit)
	var body struct {
		ID string `json:"id"`
	}
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if !p.sysHub.Memory().Forget(body.ID) {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown fact"})
		return
	}
	p.sysHub.Audit("sysadmin_memory_forget", map[string]any{"id": body.ID})
	writePanelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *panelServer) apiSysadminPassport(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelSysadminBodyLimit)
	var body struct {
		Section string `json:"section"`
		Text    string `json:"text"`
	}
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := p.sysHub.Memory().SetPassport(body.Section, body.Text); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p.sysHub.Audit("sysadmin_passport", map[string]any{"section": body.Section})
	writePanelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *panelServer) apiSysadminTelegram(w http.ResponseWriter, r *http.Request) {
	if p.sysHub == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sysadmin hub unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelSysadminBodyLimit)
	var body struct {
		Enabled    bool    `json:"enabled"`
		Users      []int64 `json:"users"`
		Agent      string  `json:"agent"`
		Token      string  `json:"token"`
		ClearToken bool    `json:"clear_token"`
	}
	dec := stdjson.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := p.sysHub.SetTelegram(body.Enabled, body.Users, body.Agent, body.Token, body.ClearToken); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p.sysHub.Audit("sysadmin_telegram", map[string]any{"enabled": body.Enabled, "users": body.Users, "agent": body.Agent})
	writePanelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

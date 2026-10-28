package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"quasarbridge/internal/bridge"
)

// settingsTestDeps wires the write path over a real voice state and records the
// configuration the live engine would have been given.
func settingsTestDeps(t *testing.T) (settingsDeps, *voiceState, *[]config) {
	t.Helper()
	voice := new(voiceState)
	applied := new([]config)
	deps := settingsDeps{
		Voice: voice,
		Live:  func() *bridge.Native { return nil },
		Apply: func(cfg config) error {
			*applied = append(*applied, cfg)
			return nil
		},
	}
	return deps, voice, applied
}

// TestSettingsPatchWritesBothSections walks the panel write path end to end: the
// sections are validated together, written in one atomic swap, folded into the
// live voice table and applied to the running engine.
func TestSettingsPatchWritesBothSections(t *testing.T) {
	path := configFile(t, `{"llm_model":"m","local_voice":{"voice":"omazh"}}`)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	deps, voice, applied := settingsTestDeps(t)

	patch := settingsPatch{
		Agents: &[]agentConfig{{
			Name:         "briefing",
			SystemPrompt: "Ты — утренний брифинг.",
			Voice:        "jane",
		}},
		WakeWords: &wakeWordsConfig{Words: []wakeWordConfig{{
			Word: "Брифинг.", Action: wakeAsk, Text: "Дай краткую сводку.", Agent: "briefing",
		}}},
	}
	status, payload := applySettingsPatch(path, deps, patch)
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}

	cfg, err := readConfig(path)
	if err != nil {
		t.Fatalf("written configuration is not readable: %v", err)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "briefing" {
		t.Fatalf("agents: %+v", cfg.Agents)
	}
	// The defaults a restart would fill are written as well, so the file says
	// what the process actually uses.
	if cfg.Agents[0].Model != "m" || cfg.Agents[0].API != "chat_completions" {
		t.Fatalf("agents not normalized: %+v", cfg.Agents[0])
	}
	if len(cfg.WakeWords.Words) != 1 || cfg.WakeWords.Words[0].Word != "брифинг" || cfg.WakeWords.Words[0].Agent != "briefing" {
		t.Fatalf("wake words: %+v", cfg.WakeWords)
	}
	if got := cfg.LocalVoice.Routes["agent:briefing"]; got != "jane" {
		t.Fatalf("role voice not folded into local_voice: %+v", cfg.LocalVoice)
	}
	if got := voice.Load().Routes["agent:briefing"]; got != "jane" {
		t.Fatalf("live voice not applied: %+v", voice.Load())
	}
	if len(*applied) != 1 || len((*applied)[0].WakeWords.Words) != 1 {
		t.Fatalf("live engine not applied: %+v", *applied)
	}

	snapshot, err := os.ReadFile(path + ".settings-prev")
	if err != nil {
		t.Fatalf("previous configuration not kept: %v", err)
	}
	if string(snapshot) != string(original) {
		t.Fatalf("snapshot is not the previous file:\n%s", snapshot)
	}
}

// TestSettingsPatchRejectsUnknownAgentKeepsFile pins the ordering: validation
// runs before the file is touched, so a word naming a removed role cannot leave
// a half-applied configuration behind.
func TestSettingsPatchRejectsUnknownAgentKeepsFile(t *testing.T) {
	path := configFile(t, `{"llm_model":"m"}`)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	deps, voice, applied := settingsTestDeps(t)

	status, payload := applySettingsPatch(path, deps, settingsPatch{
		WakeWords: &wakeWordsConfig{Words: []wakeWordConfig{{
			Word: "брифинг", Action: wakeAsk, Text: "Дай сводку.", Agent: "ghost",
		}}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	if message, _ := payload.(map[string]string)["error"]; !strings.Contains(message, "unknown agent") {
		t.Fatalf("error does not explain the role: %v", payload)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("rejected patch changed the file:\n%s", after)
	}
	if _, err := os.Stat(path + ".settings-prev"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected patch took a snapshot: %v", err)
	}
	if len(*applied) != 0 || voice.Revision() != 0 {
		t.Fatalf("rejected patch reached the live engine: %+v", *applied)
	}
}

// TestSettingsPatchRemovesAllSections checks that an empty list really clears
// the section: a removed role must lose its derived voice route too.
func TestSettingsPatchRemovesAllSections(t *testing.T) {
	path := configFile(t, `{"llm_model":"m","agents":[{"name":"briefing","voice":"jane"}],"wake_words":{"words":[{"word":"брифинг","action":"ask","text":"сводка","agent":"briefing"}]}}`)
	deps, voice, _ := settingsTestDeps(t)
	if _, err := readConfig(path); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}

	status, payload := applySettingsPatch(path, deps, settingsPatch{
		Agents:    &[]agentConfig{},
		WakeWords: &wakeWordsConfig{Words: []wakeWordConfig{}},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents != nil || cfg.WakeWords.Words != nil {
		t.Fatalf("sections not cleared: agents=%+v words=%+v", cfg.Agents, cfg.WakeWords.Words)
	}
	if _, ok := cfg.LocalVoice.Routes["agent:briefing"]; ok {
		t.Fatalf("derived route survived the role: %+v", cfg.LocalVoice.Routes)
	}
	if _, ok := voice.Load().Routes["agent:briefing"]; ok {
		t.Fatalf("live route survived the role: %+v", voice.Load().Routes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"agents": []`) {
		t.Fatalf("empty section not written: %s", raw)
	}
}

// TestSettingsPatchChangesRoleVoiceWithoutConflict: the route left by an earlier
// write is derived from the role, so it must not be read as an operator override
// that collides with the new voice.
func TestSettingsPatchChangesRoleVoiceWithoutConflict(t *testing.T) {
	path := configFile(t, `{"llm_model":"m","local_voice":{"routes":{"agent:briefing":"jane"}},"agents":[{"name":"briefing","voice":"jane"}]}`)
	deps, voice, _ := settingsTestDeps(t)

	status, payload := applySettingsPatch(path, deps, settingsPatch{
		Agents: &[]agentConfig{{Name: "briefing", Voice: "omazh"}},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.LocalVoice.Routes["agent:briefing"]; got != "omazh" {
		t.Fatalf("role voice not replaced: %+v", cfg.LocalVoice.Routes)
	}
	if got := voice.Load().Routes["agent:briefing"]; got != "omazh" {
		t.Fatalf("live voice not replaced: %+v", voice.Load().Routes)
	}
}

// TestSettingsPatchRollsBackWhenApplyFails keeps the file and the running bridge
// telling the same story: a live engine that refuses the new sections restores
// the previous file and is re-applied from it.
func TestSettingsPatchRollsBackWhenApplyFails(t *testing.T) {
	path := configFile(t, `{"llm_model":"m"}`)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	voice := new(voiceState)
	calls := 0
	deps := settingsDeps{
		Voice: voice,
		Live:  func() *bridge.Native { return nil },
		Apply: func(config) error {
			calls++
			if calls == 1 {
				return errors.New("engine refused the roles")
			}
			return nil
		},
	}

	status, payload := applySettingsPatch(path, deps, settingsPatch{
		Agents: &[]agentConfig{{Name: "briefing", Model: "m", SystemPrompt: "роль"}},
	})
	if status != http.StatusInternalServerError {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("file not restored:\n%s", after)
	}
	if calls != 2 {
		t.Fatalf("previous configuration not re-applied: %d applies", calls)
	}
	if revision := voice.Revision(); revision > 0 && len(voice.Load().Routes) != 0 {
		t.Fatalf("refused roles reached the live voice: %+v", voice.Load())
	}
}

// TestPanelSettingsRequiresSessionAndCSRF: the new endpoints live behind the
// same two gates as every other mutation.
func TestPanelSettingsRequiresSessionAndCSRF(t *testing.T) {
	panel, _, _ := panelTestServer(t)
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/panel/settings", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", recorder.Code)
	}

	cookie, csrf := panelLogin(t, panel)
	request := panelMutation(t, "/api/panel/settings", map[string]any{"wake_words": map[string]any{"dry_run": true, "words": []any{}}}, cookie, "")
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST without csrf = %d", recorder.Code)
	}

	request = panelMutation(t, "/api/panel/settings", map[string]any{"wake_words": map[string]any{"dry_run": true, "words": []any{}}}, cookie, csrf)
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST with csrf = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestPanelSettingsUpdateRoundTrip covers the JSON contract the page relies on:
// the GET hands back the vocabularies and the current sections, the POST stores
// them, and the audit log records the action without the prompt text.
func TestPanelSettingsUpdateRoundTrip(t *testing.T) {
	panel, _, dir := panelTestServer(t)
	cookie, csrf := panelLogin(t, panel)

	request := httptest.NewRequest(http.MethodGet, "/api/panel/settings", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"agents":[]`, `"wake_words"`, `"actions":["listen","ask","command"]`, `"voices":["jane"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET lacks %s: %s", want, body)
		}
	}

	request = panelMutation(t, "/api/panel/settings", map[string]any{
		"agents": []any{map[string]any{
			"name": "briefing", "model": "m", "system_prompt": "Ты — брифинг.", "voice": "jane",
		}},
		"wake_words": map[string]any{
			"dry_run": false,
			"words":   []any{map[string]any{"word": "брифинг", "action": "ask", "text": "Дай сводку.", "agent": "briefing"}},
		},
	}, cookie, csrf)
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", recorder.Code, recorder.Body.String())
	}
	cfg, err := readConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 1 || len(cfg.WakeWords.Words) != 1 {
		t.Fatalf("sections not written: %+v %+v", cfg.Agents, cfg.WakeWords)
	}

	audit, err := os.ReadFile(filepath.Join(dir, "panel_audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"action":"settings_update"`, `"result":"ok"`, `"wake_words":1`} {
		if !strings.Contains(string(audit), want) {
			t.Fatalf("audit lacks %s: %s", want, audit)
		}
	}
}

// TestWakeStateStatusDescribesTheTable: /healthz publishes the size of the word
// table, never the words themselves.
func TestWakeStateStatusDescribesTheTable(t *testing.T) {
	state := new(wakeState)
	if status := state.status(); status["words"] != 0 || status["dry_run"] != false {
		t.Fatalf("empty table: %v", status)
	}
	state.set(wakeWordsConfig{DryRun: true, Words: []wakeWordConfig{
		{Word: "брифинг", Action: wakeAsk, Text: "сводка"},
		{Word: "Егорка", Action: wakeListen},
	}})
	if status := state.status(); status["words"] != 2 || status["dry_run"] != true {
		t.Fatalf("table status: %v", status)
	}
}

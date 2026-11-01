package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"quasarbridge/internal/bridge"
)

// voiceConfigBody mirrors the deployed configuration: an unrelated member must
// survive a hot-apply byte for byte.
const voiceConfigBody = `{"llm_model":"deepseek-flash","listen":"127.0.0.1:8443","first_frame_ms":8000,"local_voice":{"voice":"omazh","candidates":["oksana","jane"],"volume":1}}`

func TestVoicePatchAppliesOnlyGivenFields(t *testing.T) {
	current := bridge.VoiceConfig{Voice: "omazh", Emotion: "neutral", Speed: 1.1, Volume: 1, Candidates: []string{"oksana"}, Routes: map[string]string{"home": "jane"}}
	volume := 1.4
	got := voicePatch{Volume: &volume}.apply(current)
	if got.Volume != 1.4 || got.Voice != "omazh" || got.Emotion != "neutral" || got.Speed != 1.1 || len(got.Candidates) != 1 || got.Routes["home"] != "jane" {
		t.Fatalf("partial patch changed other fields: %+v", got)
	}
	var none []string
	got = voicePatch{Candidates: &none}.apply(got)
	if got.Candidates != nil {
		t.Fatalf("cleared candidates = %v, want nil", got.Candidates)
	}
	if got.Volume != 1.4 {
		t.Fatalf("volume after candidate patch = %v", got.Volume)
	}
}

func TestWriteLocalVoiceKeepsOtherMembers(t *testing.T) {
	path := configFile(t, voiceConfigBody)
	before := readConfigMembers(t, path)
	if err := writeLocalVoice(path, bridge.VoiceConfig{Voice: "oksana", Candidates: []string{"jane"}, Volume: 1.2}); err != nil {
		t.Fatal(err)
	}
	after := readConfigMembers(t, path)
	for name, value := range before {
		if name == "local_voice" {
			continue
		}
		if string(after[name]) != string(value) {
			t.Fatalf("%s rewritten: %s -> %s", name, value, after[name])
		}
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalVoice.Voice != "oksana" || cfg.LocalVoice.Volume != 1.2 || len(cfg.LocalVoice.Candidates) != 1 || cfg.LocalVoice.Candidates[0] != "jane" {
		t.Fatalf("local_voice = %+v", cfg.LocalVoice)
	}
	backup, err := os.ReadFile(path + ".voice-prev")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != voiceConfigBody {
		t.Fatalf("backup = %s", backup)
	}
}

func TestVoiceConfigHandlerAppliesWithoutRestart(t *testing.T) {
	path := configFile(t, voiceConfigBody)
	state := new(voiceState)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	state.Store(initial.LocalVoice)
	native := bridge.NewNative(t.Context(), nil, nil, nil)
	defer native.Close()
	native.SetVoice(initial.LocalVoice)
	base := state.Revision()
	handler := voiceConfigHandler(&path, state, func() *bridge.Native { return native })

	response := serveVoice(handler, http.MethodPut, `{"volume":1.4}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if native.Voice().Volume != 1.4 || native.Voice().Voice != "omazh" {
		t.Fatalf("live voice = %+v", native.Voice())
	}
	if state.Load().Volume != 1.4 || state.Revision() != base+1 {
		t.Fatalf("state = %+v revision=%d", state.Load(), state.Revision())
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalVoice.Volume != 1.4 || cfg.LLMModel != "deepseek-flash" {
		t.Fatalf("config = %+v", cfg)
	}
	// The same request twice must not look like a change.
	response = serveVoice(handler, http.MethodPut, `{"volume":1.4}`)
	if response.Code != http.StatusOK || state.Revision() != base+1 {
		t.Fatalf("repeat status=%d revision=%d body=%s", response.Code, state.Revision(), response.Body.String())
	}

	response = serveVoice(handler, http.MethodGet, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get status=%d", response.Code)
	}
	var payload struct {
		LocalVoice bridge.VoiceConfig `json:"local_voice"`
		Revision   uint64             `json:"revision"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.LocalVoice.Volume != 1.4 || payload.Revision != base+1 {
		t.Fatalf("get payload = %+v", payload)
	}

	// An empty voice is the documented way back to the stock Alice voice, so it
	// must be accepted and reach the live session.
	response = serveVoice(handler, http.MethodPut, `{"voice":""}`)
	if response.Code != http.StatusOK || state.Revision() != base+2 {
		t.Fatalf("clear status=%d revision=%d body=%s", response.Code, state.Revision(), response.Body.String())
	}
	if native.Voice().Voice != "" || native.Voice().Volume != 1.4 {
		t.Fatalf("live voice after clear = %+v", native.Voice())
	}
}

func TestVoiceConfigHandlerRejectsInvalidAndForeignWrites(t *testing.T) {
	path := configFile(t, voiceConfigBody)
	state := new(voiceState)
	state.Store(bridge.VoiceConfig{Voice: "omazh"})
	base := state.Revision()
	native := bridge.NewNative(t.Context(), nil, nil, nil)
	defer native.Close()
	native.SetVoice(state.Load())
	handler := voiceConfigHandler(&path, state, func() *bridge.Native { return native })

	for _, body := range []string{
		`{"voice":"fil ipp"}`,
		`{"voice":"filipp"}`,
		`{"candidates":["jane","tanya"]}`,
		`{"routes":{"llm":"silaerkan"}}`,
		`{"speed":9}`,
		`{"volume":0.01}`,
		`{"routes":{"yandex":"jane"}}`,
		`not json`,
	} {
		if response := serveVoice(handler, http.MethodPut, body); response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", body, response.Code, response.Body.String())
		}
	}
	if state.Revision() != base || native.Voice().Voice != "omazh" {
		t.Fatalf("rejected write changed state: %+v", state.Load())
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != voiceConfigBody {
		t.Fatalf("rejected write changed the file: %s (%v)", data, err)
	}

	// A PUT that does not come from the station itself must never touch the
	// configuration, even when the body is valid.
	request := httptest.NewRequest(http.MethodPut, "/api/voice-config", bytes.NewReader([]byte(`{"voice":"jane"}`)))
	request.RemoteAddr = "192.168.1.50:4000"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign status=%d", response.Code)
	}
	if data, _ := os.ReadFile(path); string(data) != voiceConfigBody {
		t.Fatalf("foreign write reached the file: %s", data)
	}
}

// TestVoiceStateReloadVoiceUpdatesLiveSession covers SIGHUP: the reload path
// shares the apply lock and pushes the file value to the live session.
func TestVoiceStateReloadVoiceUpdatesLiveSession(t *testing.T) {
	path := configFile(t, voiceConfigBody)
	state := new(voiceState)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	state.Store(initial.LocalVoice)
	base := state.Revision()
	native := bridge.NewNative(t.Context(), nil, nil, nil)
	defer native.Close()
	native.SetVoice(initial.LocalVoice)
	live := func() *bridge.Native { return native }

	if revision, err := state.reloadVoice(path, live); err != nil || revision != base {
		t.Fatalf("unchanged reload revision=%d err=%v, want %d", revision, err, base)
	}
	if err := os.WriteFile(path, []byte(`{"llm_model":"deepseek-flash","local_voice":{"voice":"jane","volume":0.9}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	revision, err := state.reloadVoice(path, live)
	if err != nil {
		t.Fatal(err)
	}
	if revision != base+1 || native.Voice().Voice != "jane" || native.Voice().Volume != 0.9 {
		t.Fatalf("revision=%d voice=%+v", revision, native.Voice())
	}
}

func serveVoice(handler http.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/api/voice-config", bytes.NewReader([]byte(body)))
	request.RemoteAddr = "127.0.0.1:51234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func readConfigMembers(t *testing.T, path string) map[string]jsontext.Value {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	return members
}

func TestVoiceStateSessionsOutliveOverlap(t *testing.T) {
	state := new(voiceState)
	a, b := bridge.NewNative(t.Context(), nil, nil, nil), bridge.NewNative(t.Context(), nil, nil, nil)
	state.Attach(a)
	state.Attach(b)
	state.Detach(b)
	if state.Latest() != a {
		t.Fatal("older session must stay reachable after the newer one closes")
	}
	cfg := bridge.VoiceConfig{Voice: "omazh", Speed: 1}
	state.push(cfg, func() *bridge.Native { return nil })
	if a.Voice().Speed != 1 {
		t.Fatal("push must reach every open session")
	}
}

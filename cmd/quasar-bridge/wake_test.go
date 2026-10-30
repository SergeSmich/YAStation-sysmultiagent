package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"quasarbridge/internal/bridge"
)

// armedPreset is one prepared answer a start word handed to the session.
type armedPreset struct {
	text  string
	route string
	ttl   time.Duration
}

// pinnedInjector records the route each injected frame carried and the answers
// prepared for the next microphone window.
type pinnedInjector struct {
	mu      sync.Mutex
	frames  [][]byte
	routes  []string
	err     error
	presets []armedPreset
	armErr  error
}

func (p *pinnedInjector) Client(ctx context.Context, _ websocket.MessageType, data []byte) error {
	route, _ := bridge.PinnedRoute(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames = append(p.frames, append([]byte{}, data...))
	p.routes = append(p.routes, route)
	return p.err
}

func (p *pinnedInjector) ArmPreset(text, route string, ttl time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.presets = append(p.presets, armedPreset{text: text, route: route, ttl: ttl})
	return p.armErr
}

func (p *pinnedInjector) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.frames)
}

func (p *pinnedInjector) armed() []armedPreset {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]armedPreset{}, p.presets...)
}

// clientOnlyInjector can inject text but cannot prepare an answer, like a
// native session built before the prepared-answer mechanism existed.
type clientOnlyInjector struct {
	injector *pinnedInjector
}

func (c clientOnlyInjector) Client(ctx context.Context, k websocket.MessageType, data []byte) error {
	return c.injector.Client(ctx, k, data)
}

type wakeResponse struct {
	OK      bool   `json:"ok"`
	Known   bool   `json:"known"`
	Action  string `json:"action"`
	Listen  bool   `json:"listen"`
	DryRun  bool   `json:"dry_run"`
	Request string `json:"request"`
}

func wakePost(body, remote string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/wake", strings.NewReader(body))
	request.RemoteAddr = remote
	return request
}

func wakeCall(t *testing.T, words wakeWordsConfig, native func() textInjector, body, remote string) (int, wakeResponse, *pinnedInjector) {
	t.Helper()
	injector := &pinnedInjector{}
	if native == nil {
		native = func() textInjector { return injector }
	}
	handler := wakeHandler(func() *wakeTable { return newWakeTable(words) }, native)
	response := httptest.NewRecorder()
	handler(response, wakePost(body, remote))
	var parsed wakeResponse
	if response.Code == http.StatusOK {
		if err := stdjson.NewDecoder(response.Body).Decode(&parsed); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return response.Code, parsed, injector
}

func TestReadConfigWakeWords(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{
		"llm_model":"m",
		"agents":[{"name":"briefing"}],
		"wake_words":{"words":[
			{"word":" Егорка "},
			{"word":"Брифинг!","action":"ask","agent":"briefing","text":" дай сводку "},
			{"word":"Слушай","action":"ask","agent":"briefing"},
			{"word":"свет","action":"command","text":"включи свет"}
		]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []wakeWordConfig{
		{Word: "егорка", Action: wakeListen},
		{Word: "брифинг", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
		{Word: "слушай", Action: wakeAsk, Agent: "briefing"},
		{Word: "свет", Action: wakeCommand, Text: "включи свет"},
	}
	if len(cfg.WakeWords.Words) != len(want) {
		t.Fatalf("words: %+v", cfg.WakeWords.Words)
	}
	for i := range want {
		if cfg.WakeWords.Words[i] != want[i] {
			t.Fatalf("word %d = %+v, want %+v", i, cfg.WakeWords.Words[i], want[i])
		}
	}
	if cfg.WakeWords.DryRun {
		t.Fatal("dry_run enabled by default")
	}
}

func TestReadConfigWithoutWakeWordsIsUnchanged(t *testing.T) {
	for _, body := range []string{`{}`, `{"wake_words":{}}`, `{"wake_words":{"words":[]}}`} {
		cfg, err := readConfig(configFile(t, body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if cfg.WakeWords.Words != nil || cfg.WakeWords.DryRun {
			t.Fatalf("%s: %+v", body, cfg.WakeWords)
		}
	}
}

func TestReadConfigRejectsInvalidWakeWords(t *testing.T) {
	for _, body := range []string{
		`{"wake_words":{"words":[{"word":""}]}}`,
		`{"wake_words":{"words":[{"word":"..."}]}}`,
		`{"wake_words":{"words":[{"word":"` + strings.Repeat("я", maxWakeWordRunes+1) + `"}]}}`,
		`{"wake_words":{"words":[{"word":"егорка"},{"word":"Егорка!"}]}}`,
		`{"wake_words":{"words":[{"word":"егорка","action":"silent"}]}}`,
		`{"wake_words":{"words":[{"word":"егорка","text":"привет"}]}}`,
		`{"wake_words":{"words":[{"word":"егорка","agent":"briefing"}]}}`,
		`{"wake_words":{"words":[{"word":"брифинг","action":"ask"}]}}`,
		`{"wake_words":{"words":[{"word":"брифинг","action":"ask","text":"дай сводку","agent":"ghost"}]}}`,
		`{"llm_model":"m","agents":[{"name":"briefing"}],"wake_words":{"words":[{"word":"брифинг","action":"ask","text":"дай сводку","agent":"Briefing"}]}}`,
		`{"wake_words":{"words":[{"word":"свет","action":"command"}]}}`,
		`{"wake_words":{"words":[{"word":"свет","action":"command","text":"включи свет","agent":"briefing"}]}}`,
		`{"wake_words":{"words":[{"word":"длинно","action":"ask","text":"` + strings.Repeat("я", bridge.MaxSyntheticTextRunes+1) + `"}]}}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestWakeHandlerRejectsNonLoopback(t *testing.T) {
	status, _, injector := wakeCall(t, wakeWordsConfig{}, nil, `{"word":"егорка"}`, "192.168.1.186:41000")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if injector.count() != 0 {
		t.Fatal("frame was injected for a non-loopback caller")
	}
}

func TestWakeHandlerUnknownWordListens(t *testing.T) {
	status, body, injector := wakeCall(t, wakeWordsConfig{}, nil, `{"word":"неизвестное"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body.Known || !body.Listen || body.Action != wakeListen {
		t.Fatalf("response = %+v", body)
	}
	if injector.count() != 0 {
		t.Fatal("unknown word injected a frame")
	}
}

func TestWakeHandlerListenWordOpensMicrophone(t *testing.T) {
	status, body, injector := wakeCall(t, wakeWordsConfig{Words: []wakeWordConfig{{Word: "егорка", Action: wakeListen}}}, nil, `{"word":" Егорка! "}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !body.Known || !body.Listen || body.Action != wakeListen {
		t.Fatalf("response = %+v", body)
	}
	if injector.count() != 0 {
		t.Fatal("listen word injected a frame")
	}
}

func TestWakeHandlerAskArmsPresetForTheNextWindow(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "брифинг", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"}}}
	status, body, injector := wakeCall(t, words, nil, `{"word":"брифинг"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !body.Known || !body.Listen || body.Action != wakeAsk {
		t.Fatalf("response = %+v", body)
	}
	// The prepared answer is spoken as the reply to the microphone window the
	// word opens: nothing is injected, so the daemon must open that window.
	if injector.count() != 0 {
		t.Fatalf("ask injected %d frames", injector.count())
	}
	presets := injector.armed()
	if len(presets) != 1 {
		t.Fatalf("armed %d presets", len(presets))
	}
	if presets[0].text != "дай сводку" || presets[0].route != "agent:briefing" {
		t.Fatalf("preset = %+v", presets[0])
	}
	if presets[0].ttl != bridge.DefaultPresetTTL {
		t.Fatalf("ttl = %v, want %v", presets[0].ttl, bridge.DefaultPresetTTL)
	}
}

func TestWakeHandlerAskWithoutAgentUsesDefaultAssistant(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "скажи", Action: wakeAsk, Text: "который час"}}}
	status, body, injector := wakeCall(t, words, nil, `{"word":"скажи"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !body.Listen {
		t.Fatalf("response = %+v", body)
	}
	presets := injector.armed()
	if len(presets) != 1 || presets[0].route != "llm" {
		t.Fatalf("presets = %+v", presets)
	}
}

// TestWakeHandlerAskWithoutTextOnlyPinsTheRole: a word without a phrase opens
// the microphone and prepares nothing to say — the person's own words are the
// question, so only the route is armed.
func TestWakeHandlerAskWithoutTextOnlyPinsTheRole(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "егорка", Action: wakeAsk, Agent: "briefing"}}}
	status, body, injector := wakeCall(t, words, nil, `{"word":"егорка"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !body.Known || !body.Listen || body.Action != wakeAsk {
		t.Fatalf("response = %+v", body)
	}
	if injector.count() != 0 {
		t.Fatalf("ask injected %d frames", injector.count())
	}
	presets := injector.armed()
	if len(presets) != 1 || presets[0].text != "" || presets[0].route != "agent:briefing" {
		t.Fatalf("presets = %+v", presets)
	}
}

func TestWakeHandlerAskWithoutPresetSupportFallsBack(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "брифинг", Action: wakeAsk, Text: "дай сводку"}}}
	holder := &pinnedInjector{}
	status, _, _ := wakeCall(t, words, func() textInjector { return clientOnlyInjector{injector: holder} }, `{"word":"брифинг"}`, "127.0.0.1:41000")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if holder.count() != 0 {
		t.Fatal("a session without preset support still injected a frame")
	}
}

func TestWakeHandlerCommandInjectsWithoutPin(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "свет", Action: wakeCommand, Text: "включи свет в кабинете"}}}
	status, body, injector := wakeCall(t, words, nil, `{"word":"свет"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if body.Action != wakeCommand || body.Listen {
		t.Fatalf("response = %+v", body)
	}
	if injector.count() != 1 || injector.routes[0] != "" {
		t.Fatalf("routes = %v", injector.routes)
	}
}

func TestWakeHandlerDryRunOnlyListens(t *testing.T) {
	words := wakeWordsConfig{DryRun: true, Words: []wakeWordConfig{{Word: "брифинг", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"}}}
	status, body, injector := wakeCall(t, words, nil, `{"word":"брифинг"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !body.DryRun || !body.Listen || body.Action != wakeListen {
		t.Fatalf("response = %+v", body)
	}
	if injector.count() != 0 {
		t.Fatal("dry run injected a frame")
	}
}

func TestWakeHandlerWithoutSessionFallsBack(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "брифинг", Action: wakeAsk, Text: "дай сводку"}}}
	status, _, _ := wakeCall(t, words, func() textInjector { return nil }, `{"word":"брифинг"}`, "127.0.0.1:41000")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
}

func TestWakeHandlerRejectsBadBodies(t *testing.T) {
	for _, body := range []string{`{"word":`, `{}`, `{"word":"   "}`, `{"word":"..."}`} {
		status, _, injector := wakeCall(t, wakeWordsConfig{}, nil, body, "127.0.0.1:41000")
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, status)
		}
		if injector.count() != 0 {
			t.Fatalf("%s: frame injected", body)
		}
	}
}

func TestWakeHandlerReportsInjectionFailure(t *testing.T) {
	// Only a "command" word is injected: an "ask" word prepares its answer for
	// the microphone window and never reaches the injector.
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "свет", Action: wakeCommand, Text: "включи свет"}}}
	failing := &pinnedInjector{err: errors.New("too many pending native requests")}
	status, _, _ := wakeCall(t, words, func() textInjector { return failing }, `{"word":"свет"}`, "127.0.0.1:41000")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
}

func TestReloadWakeWordsIsHotApplied(t *testing.T) {
	path := configFile(t, `{"wake_words":{"words":[{"word":"егорка"}]}}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	wake := new(wakeState)
	apply := func(next config) error {
		wake.set(next.WakeWords)
		return nil
	}
	if err := apply(initial); err != nil {
		t.Fatal(err)
	}
	if _, ok := wake.load().lookup("егорка"); !ok {
		t.Fatal("initial word missing")
	}
	if err := os.WriteFile(path, []byte(`{"wake_words":{"words":[{"word":"брифинг","action":"ask","text":"дай сводку"}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, engine, reloadHooks{applyWake: apply}); err != nil {
		t.Fatalf("wake words required a restart: %v", err)
	}
	if _, ok := wake.load().lookup("егорка"); ok {
		t.Fatal("removed word survived the reload")
	}
	entry, ok := wake.load().lookup("Брифинг")
	if !ok || entry.Action != wakeAsk || entry.Text != "дай сводку" {
		t.Fatalf("entry: %+v %v", entry, ok)
	}
}

func TestReloadWakeWordsRemovesAllWords(t *testing.T) {
	path := configFile(t, `{"wake_words":{"words":[{"word":"егорка"},{"word":"брифинг","action":"ask","text":"дай сводку"}]}}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	wake := new(wakeState)
	apply := func(next config) error {
		wake.set(next.WakeWords)
		return nil
	}
	if err := apply(initial); err != nil {
		t.Fatal(err)
	}
	if got := wake.load().size(); got != 2 {
		t.Fatalf("words = %d", got)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, engine, reloadHooks{applyWake: apply}); err != nil {
		t.Fatalf("removing every word required a restart: %v", err)
	}
	if got := wake.load().size(); got != 0 {
		t.Fatalf("words after reload = %d", got)
	}
	// A word that was just removed must fall back to listening, not to a cached
	// reaction.
	injected := &pinnedInjector{}
	handler := wakeHandler(wake.load, func() textInjector { return injected })
	recorder := httptest.NewRecorder()
	handler(recorder, wakePost(`{"word":"брифинг"}`, "127.0.0.1:41000"))
	var parsed wakeResponse
	if err := stdjson.NewDecoder(recorder.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if parsed.Known || !parsed.Listen || parsed.Action != wakeListen {
		t.Fatalf("response = %+v", parsed)
	}
	if injected.count() != 0 {
		t.Fatal("a removed word still injected a frame")
	}
}

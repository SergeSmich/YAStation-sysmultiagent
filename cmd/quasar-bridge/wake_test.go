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

	"github.com/coder/websocket"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/wire"
)

// pinnedInjector records the route each injected frame carried.
type pinnedInjector struct {
	mu     sync.Mutex
	frames [][]byte
	routes []string
	err    error
}

func (p *pinnedInjector) Client(ctx context.Context, _ websocket.MessageType, data []byte) error {
	route, _ := bridge.PinnedRoute(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames = append(p.frames, append([]byte{}, data...))
	p.routes = append(p.routes, route)
	return p.err
}

func (p *pinnedInjector) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.frames)
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
			{"word":"свет","action":"command","text":"включи свет"}
		]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []wakeWordConfig{
		{Word: "егорка", Action: wakeListen},
		{Word: "брифинг", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
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

func TestWakeHandlerAskInjectsPinnedPreset(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "брифинг", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"}}}
	status, body, injector := wakeCall(t, words, nil, `{"word":"брифинг"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !body.Known || body.Listen || body.Action != wakeAsk || body.Request == "" {
		t.Fatalf("response = %+v", body)
	}
	if injector.count() != 1 {
		t.Fatalf("injected %d frames", injector.count())
	}
	if got := injector.routes[0]; got != "agent:briefing" {
		t.Fatalf("pinned route = %q", got)
	}
	decoded := wire.Decode(injector.frames[0], wire.ClientToServer)
	if got := decoded.Kind(); got != "TextInput" {
		t.Fatalf("frame kind = %q", got)
	}
	if got := decoded.Client.GetEvent().GetTextInput().GetRequest().GetEvent().GetText(); got != "дай сводку" {
		t.Fatalf("text = %q", got)
	}
	if got := decoded.Client.GetEvent().GetHeader().GetMessageId(); got != body.Request {
		t.Fatalf("message id = %q, want %q", got, body.Request)
	}
}

func TestWakeHandlerAskWithoutAgentUsesDefaultAssistant(t *testing.T) {
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "скажи", Action: wakeAsk, Text: "который час"}}}
	status, _, injector := wakeCall(t, words, nil, `{"word":"скажи"}`, "127.0.0.1:41000")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if injector.count() != 1 || injector.routes[0] != "llm" {
		t.Fatalf("routes = %v", injector.routes)
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
	words := wakeWordsConfig{Words: []wakeWordConfig{{Word: "брифинг", Action: wakeAsk, Text: "дай сводку"}}}
	failing := &pinnedInjector{err: errors.New("too many pending native requests")}
	status, _, _ := wakeCall(t, words, func() textInjector { return failing }, `{"word":"брифинг"}`, "127.0.0.1:41000")
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

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGuessLLMProvider pins the key shapes the panel recognises. A wrong guess
// would send a real key to a provider it does not belong to, so the ambiguous
// "sk-" family is separated by length and nothing else is guessed at all.
func TestGuessLLMProvider(t *testing.T) {
	// "sk-" plus 32 characters: a DeepSeek key.
	deepseek := "sk-" + strings.Repeat("a", 32)
	// "sk-" plus 48: a legacy OpenAI key.
	legacyOpenAI := "sk-" + strings.Repeat("b", 48)
	for _, testCase := range []struct {
		name     string
		key      string
		wantName string
		wantOK   bool
	}{
		{name: "openrouter", key: "sk-or-v1-" + strings.Repeat("c", 64), wantName: "openrouter", wantOK: true},
		{name: "openai project", key: "sk-proj-" + strings.Repeat("d", 40), wantName: "openai", wantOK: true},
		{name: "groq", key: "gsk_" + strings.Repeat("e", 48), wantName: "groq", wantOK: true},
		{name: "xai", key: "xai-" + strings.Repeat("f", 40), wantName: "xai", wantOK: true},
		{name: "google", key: "AIza" + strings.Repeat("g", 35), wantName: "google", wantOK: true},
		{name: "deepseek by length", key: deepseek, wantName: "deepseek", wantOK: true},
		{name: "legacy openai by length", key: legacyOpenAI, wantName: "openai", wantOK: true},
		{name: "unknown shape", key: "zzz-not-a-provider-key", wantOK: false},
		{name: "ambiguous sk- length", key: "sk-" + strings.Repeat("h", 20), wantOK: false},
		{name: "anthropic is not openai-compatible", key: "sk-ant-" + strings.Repeat("i", 40), wantOK: false},
		{name: "empty", key: "   ", wantOK: false},
	} {
		guess, ok := guessLLMProvider(testCase.key)
		if ok != testCase.wantOK {
			t.Fatalf("%s: ok = %v, want %v", testCase.name, ok, testCase.wantOK)
		}
		if ok && guess.name != testCase.wantName {
			t.Fatalf("%s: provider = %q, want %q", testCase.name, guess.name, testCase.wantName)
		}
	}
}

// TestPanelLLMDetectFillsTheRowFromTheKey walks the endpoint the page calls when
// the operator pastes a key: the answer carries the address, the wire format and
// the model list, and the key itself never comes back.
func TestPanelLLMDetectFillsTheRowFromTheKey(t *testing.T) {
	panel, _, dir := panelTestServer(t)
	const key = "sk-" + "0123456789abcdef0123456789abcdef" // 32 characters after "sk-"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
	}))
	defer server.Close()

	// The table is a variable so the guess can point at the fake provider.
	original := llmGuesses
	llmGuesses = []llmGuess{{
		name: "deepseek", baseURL: server.URL + "/v1", api: "chat_completions",
		prefixes: []string{"sk-"}, generic: true, lengths: []int{len(key)},
	}}
	defer func() { llmGuesses = original }()

	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/panel/llm/detect",
		strings.NewReader(`{"api_key":"`+key+`"}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous detect = %d", recorder.Code)
	}

	cookie, csrf := panelLogin(t, panel)
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/detect", map[string]any{"api_key": key}, cookie, csrf))
	if recorder.Code != http.StatusOK {
		t.Fatalf("detect = %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"detected":true`, `"verified":true`, `"provider":"deepseek"`, "deepseek-reasoner", `"api":"chat_completions"`, server.URL + "/v1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detect body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, key) {
		t.Fatal("the key leaked into the detect answer")
	}

	// An unknown key is an answer, not an error: the page fills the row by hand.
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/detect", map[string]any{"api_key": "zzz-not-a-key"}, cookie, csrf))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"detected":false`) {
		t.Fatalf("unknown key = %d %s", recorder.Code, recorder.Body.String())
	}

	// An empty key is a bad request, not a guess.
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/detect", map[string]any{"api_key": "  "}, cookie, csrf))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty key = %d %s", recorder.Code, recorder.Body.String())
	}

	audit, err := os.ReadFile(filepath.Join(dir, "panel_audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"action":"llm_detect"`} {
		if !strings.Contains(string(audit), want) {
			t.Fatalf("audit lacks %s: %s", want, audit)
		}
	}
	if strings.Contains(string(audit), key) {
		t.Fatal("the key reached the audit log")
	}
}

// TestPanelLLMDetectKeepsTheAddressWhenTheProviderIsUnreachable: the key shape is
// still the right answer, so the page gets the address plus the reason instead of
// an empty row.
func TestPanelLLMDetectKeepsTheAddressWhenTheProviderIsUnreachable(t *testing.T) {
	panel, _, _ := panelTestServer(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer server.Close()

	const key = "gsk_" + "0123456789"
	original := llmGuesses
	llmGuesses = []llmGuess{{
		name: "groq", baseURL: server.URL + "/v1", api: "chat_completions",
		prefixes: []string{"gsk_"},
	}}
	defer func() { llmGuesses = original }()

	cookie, csrf := panelLogin(t, panel)
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/detect", map[string]any{"api_key": key}, cookie, csrf))
	if recorder.Code != http.StatusOK {
		t.Fatalf("detect = %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"detected":true`, `"verified":false`, `"provider":"groq"`, "не принял ключ"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detect body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, key) {
		t.Fatal("the key leaked into the detect answer")
	}
}

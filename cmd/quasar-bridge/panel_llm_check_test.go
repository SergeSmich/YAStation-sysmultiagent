package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPanelLLMCheckStoresModelsAndNeverEchoesTheKey: one «Проверить» proves the
// key, asks the model, and leaves the model list in llm_models.json for the next
// page load. The key must not come back in the answer or land in the file.
func TestPanelLLMCheckStoresModelsAndNeverEchoesTheKey(t *testing.T) {
	panel, _, dir := panelTestServer(t)
	const key = "secret-check-key-123456"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"alpha-free"},{"id":"beta"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cookie, csrf := panelLogin(t, panel)
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/check", map[string]any{
		"provider": "demo", "base_url": server.URL + "/v1", "api": "chat_completions",
		"model": "alpha-free", "api_key": key,
	}, cookie, csrf))
	if recorder.Code != http.StatusOK {
		t.Fatalf("check = %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"ok":true`, `"chat_ok":true`, "alpha-free", "pong"} {
		if !strings.Contains(body, want) {
			t.Fatalf("answer lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, key) {
		t.Fatal("the key came back in the answer")
	}
	models := readLLMModels(dir + "/config.json")
	if got := models["demo"]; len(got) != 2 || got[0] != "alpha-free" {
		t.Fatalf("stored models = %v", models)
	}

	// A wrong key is reported, not stored over the good list.
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/check", map[string]any{
		"provider": "demo", "base_url": server.URL + "/v1", "api": "chat_completions",
		"api_key": "wrong",
	}, cookie, csrf))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok":false`) {
		t.Fatalf("bad key: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := readLLMModels(dir + "/config.json")["demo"]; len(got) != 2 {
		t.Fatalf("a failed check changed the stored list: %v", got)
	}
}

func TestStoreLLMModelsSkipsEmptyList(t *testing.T) {
	path := t.TempDir() + "/config.json"
	if err := storeLLMModels(path, "demo", []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := storeLLMModels(path, "demo", nil); err != nil {
		t.Fatal(err)
	}
	if got := readLLMModels(path)["demo"]; len(got) != 2 {
		t.Fatalf("empty list overwrote the stored one: %v", got)
	}
	if _, ok := llmPresetByID("groq"); !ok {
		t.Fatal("groq preset missing")
	}
	if _, ok := llmPresetByID("gigachat"); !ok {
		t.Fatal("gigachat preset missing")
	}
}

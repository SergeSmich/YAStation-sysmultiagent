package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseModelListAcceptsBothShapes: the page offers the model list as a hint,
// so the two documented wire shapes and a bare string array must all work.
func TestParseModelListAcceptsBothShapes(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "openai data[].id, deduplicated and sorted",
			body: `{"object":"list","data":[{"id":"b-model"},{"id":"a-model"},{"id":"a-model"}]}`,
			want: []string{"a-model", "b-model"},
		},
		{
			name: "ollama models[].name",
			body: `{"models":[{"name":"qwen2.5:7b","size":1},{"name":"llama3:8b"}]}`,
			want: []string{"llama3:8b", "qwen2.5:7b"},
		},
		{
			name: "plain string array",
			body: `{"models":["x","y"]}`,
			want: []string{"x", "y"},
		},
		{
			name: "blank ids are dropped",
			body: `{"data":[{"id":"  "},{"id":"kept"}]}`,
			want: []string{"kept"},
		},
	} {
		models, err := parseModelList([]byte(testCase.body))
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if strings.Join(models, ",") != strings.Join(testCase.want, ",") {
			t.Fatalf("%s: got %v want %v", testCase.name, models, testCase.want)
		}
	}
}

// TestParseModelListRejectsGarbage keeps "not a model list" distinct from "no
// models": an empty answer would be rendered as a provider without models.
func TestParseModelListRejectsGarbage(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"data":[]}`, `{"models":[{"id":""}]}`} {
		if _, err := parseModelList([]byte(body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

// TestPanelLLMProbeNeedsASavedProvider pins both gates of the probe endpoints:
// the session/CSRF pair, and a provider name that the file actually knows, since
// the key is looked up by that name.
func TestPanelLLMProbeNeedsASavedProvider(t *testing.T) {
	panel, _, _ := panelTestServer(t)

	for _, path := range []string{"/api/panel/llm/test", "/api/panel/llm/models"} {
		recorder := httptest.NewRecorder()
		panel.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"provider":"ghost"}`)))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous POST %s = %d", path, recorder.Code)
		}
	}

	cookie, csrf := panelLogin(t, panel)
	for _, path := range []string{"/api/panel/llm/test", "/api/panel/llm/models"} {
		recorder := httptest.NewRecorder()
		panel.ServeHTTP(recorder, panelMutation(t, path, map[string]any{"provider": "ghost"}, cookie, csrf))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("unknown provider %s = %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestPanelLLMProbeUsesTheStoredKey walks the whole probe path against a fake
// provider: the key comes from secrets.json, reaches the Authorization header,
// and never appears in the answer to the page.
func TestPanelLLMProbeUsesTheStoredKey(t *testing.T) {
	panel, _, dir := panelTestServer(t)
	const key = "secret-provider-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	body := `{"llm_providers":[{"name":"deepseek","base_url":"` + server.URL + `/v1","model":"deepseek-chat"}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"providers":{"deepseek":"`+key+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cookie, csrf := panelLogin(t, panel)

	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/models", map[string]any{"provider": "deepseek"}, cookie, csrf))
	if recorder.Code != http.StatusOK {
		t.Fatalf("models = %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "deepseek-reasoner") {
		t.Fatalf("models body: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), key) {
		t.Fatal("the key leaked into the model list answer")
	}

	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/test", map[string]any{"provider": "deepseek"}, cookie, csrf))
	if recorder.Code != http.StatusOK {
		t.Fatalf("test = %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"ok":true`) || !strings.Contains(recorder.Body.String(), "pong") {
		t.Fatalf("test body: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), key) {
		t.Fatal("the key leaked into the probe answer")
	}

	audit, err := os.ReadFile(filepath.Join(dir, "panel_audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"action":"llm_test"`, `"action":"llm_models"`} {
		if !strings.Contains(string(audit), want) {
			t.Fatalf("audit lacks %s: %s", want, audit)
		}
	}
	if strings.Contains(string(audit), key) {
		t.Fatal("the key reached the audit log")
	}
}

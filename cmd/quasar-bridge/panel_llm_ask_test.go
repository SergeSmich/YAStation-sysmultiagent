package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPanelLLMAskExplainsTheOutcome walks the role test against a fake provider:
// a good answer, a wrong answer, and a rejected key each get their own verdict,
// and the key never comes back.
func TestPanelLLMAskExplainsTheOutcome(t *testing.T) {
	panel, _, dir := panelTestServer(t)
	const key = "secret-ask-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		content := "Четыре."
		body, _ := readAllString(r)
		if strings.Contains(body, "wrong-model") {
			content = "Не знаю."
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` + content + `"}}]}`))
	}))
	defer server.Close()
	cfg := `{"llm_providers":[{"name":"demo","base_url":"` + server.URL + `/v1","model":"good-model"}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"providers":{"demo":"`+key+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := panelLogin(t, panel)
	ask := func(payload map[string]any) (int, string) {
		recorder := httptest.NewRecorder()
		panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/ask", payload, cookie, csrf))
		return recorder.Code, recorder.Body.String()
	}

	code, body := ask(map[string]any{"provider": "demo", "model": ""})
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, "всё хорошо") {
		t.Fatalf("good model: %d %s", code, body)
	}
	if strings.Contains(body, key) {
		t.Fatal("the key came back in the answer")
	}

	code, body = ask(map[string]any{"provider": "demo", "model": "wrong-model"})
	if code != http.StatusOK || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "не то, что ждали") {
		t.Fatalf("wrong answer: %d %s", code, body)
	}

	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"providers":{"demo":"bad"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = ask(map[string]any{"provider": "demo"})
	if code != http.StatusOK || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "не принял ключ") {
		t.Fatalf("bad key: %d %s", code, body)
	}

	code, _ = ask(map[string]any{"provider": "ghost"})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d", code)
	}
}

func readAllString(r *http.Request) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}

func TestExplainLLMError(t *testing.T) {
	for text, want := range map[string]string{
		"LLM returned HTTP 429":                    "лимит",
		"LLM returned HTTP 404":                    "не нашёл такой модели",
		"LLM returned HTTP 503":                    "перегружен",
		"provider returned HTTP 401":               "не принял ключ",
		"provider is unreachable":                  "достучаться",
		"LLM transport: context deadline exceeded": "медленная",
		"LLM answer exceeded token budget":         "думала",
	} {
		if got := explainLLMError(errors.New(text)); !strings.Contains(got, want) {
			t.Fatalf("%q -> %q, want %q", text, got, want)
		}
	}
}

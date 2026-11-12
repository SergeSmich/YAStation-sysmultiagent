package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPanelLLMChatAnswersAQuestion walks the mini-dialog against a fake
// provider: the operator's own question goes out with the role's saved
// prompt, and the answer, latency and verdict come back without the key.
func TestPanelLLMChatAnswersAQuestion(t *testing.T) {
	panel, _, dir := panelTestServer(t)
	const key = "secret-chat-key"
	var systemSeen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		body, _ := readAllString(r)
		_ = json.Unmarshal([]byte(body), &request)
		if len(request.Messages) > 0 && request.Messages[0].Role == "system" {
			systemSeen = request.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		content := "Пять."
		if strings.Contains(body, "нет") {
			content = "Не понял вопроса."
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"` + content + `"}}]}`))
	}))
	defer server.Close()
	cfg := `{"llm_providers":[{"name":"demo","base_url":"` + server.URL + `/v1","model":"good-model"}],` +
		`"agents":[{"name":"assistant","provider":"demo","max_output_tokens":512,"system_prompt":"Ты дворецкий."},{"name":"quiet","provider":"demo","max_output_tokens":512}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"providers":{"demo":"`+key+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := panelLogin(t, panel)
	chat := func(payload map[string]any) (int, string) {
		recorder := httptest.NewRecorder()
		panel.ServeHTTP(recorder, panelMutation(t, "/api/panel/llm/chat", payload, cookie, csrf))
		return recorder.Code, recorder.Body.String()
	}

	code, body := chat(map[string]any{"agent": "assistant", "provider": "demo", "model": "", "question": "который час?"})
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) || !strings.Contains(body, "Пять.") {
		t.Fatalf("good answer: %d %s", code, body)
	}
	if !strings.Contains(body, "ответ получен") {
		t.Fatalf("verdict missing: %s", body)
	}
	if strings.Contains(body, key) {
		t.Fatal("the key came back in the answer")
	}
	if systemSeen != "Ты дворецкий." {
		t.Fatalf("role system prompt not sent: %q", systemSeen)
	}

	// A saved role without its own prompt falls back to the shared voice
	// assistant prompt of the model probe.
	code, body = chat(map[string]any{"agent": "quiet", "provider": "demo", "model": "", "question": "который час?"})
	if code != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("role without prompt: %d %s", code, body)
	}
	if systemSeen != panelLLMAskSystem {
		t.Fatalf("expected the shared prompt, got %q", systemSeen)
	}

	// A role that answers something else is still a real answer here: the
	// mini-dialog shows whatever the model said, the verdict stays polite.
	code, body = chat(map[string]any{"agent": "assistant", "provider": "demo", "model": "", "question": "нет"})
	if code != http.StatusOK || !strings.Contains(body, "Не понял вопроса.") {
		t.Fatalf("any answer passes: %d %s", code, body)
	}

	code, _ = chat(map[string]any{"agent": "assistant", "provider": "demo", "model": "", "question": "  "})
	if code != http.StatusBadRequest {
		t.Fatalf("empty question = %d", code)
	}

	code, body = chat(map[string]any{"agent": "assistant", "provider": "demo", "model": "", "question": "х", "unknown": 1})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", code)
	}

	code, _ = chat(map[string]any{"agent": "assistant", "provider": "ghost", "model": "", "question": "х"})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d", code)
	}

	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"providers":{"demo":"bad"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = chat(map[string]any{"agent": "assistant", "provider": "demo", "model": "", "question": "х"})
	if code != http.StatusOK || !strings.Contains(body, `"ok":false`) || !strings.Contains(body, "не принял ключ") {
		t.Fatalf("bad key: %d %s", code, body)
	}
}

// TestPanelLLMChatAnswerCap keeps one question from turning into a wall of
// text: long answers are cut and marked, short ones pass unchanged.
func TestPanelLLMChatAnswerCap(t *testing.T) {
	if got := panelLLMChatAnswer("  скобочка   и\nновая строка"); got != "скобочка и новая строка" {
		t.Fatalf("expected collapsed whitespace, got %q", got)
	}
	long := strings.Repeat("а", panelLLMChatRuneLimit+100)
	got := panelLLMChatAnswer(long)
	if len([]rune(got)) != panelLLMChatRuneLimit+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("answer not capped: length %d", len([]rune(got)))
	}
}

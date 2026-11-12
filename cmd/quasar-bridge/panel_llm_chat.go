package main

import (
	"context"
	stdjson "encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"quasarbridge/internal/llm"
)

// The role mini-dialog: the panel asks a saved role a free-form question and
// reads the answer in the card. Unlike the fixed «Проверить модель» probe this
// endpoint carries the operator's own question, so it is a separate handler —
// llm/ask keeps its hard-coded question and answer-length verdict.
const (
	panelLLMChatQuestionRunes = 500
	panelLLMChatRuneLimit     = 4000
)

// apiLLMChat answers one question with the role's saved prompt and the
// connection plus model the role would really use. The key never leaves the
// station: it is looked up in secrets.json and only the outcome comes back.
func (p *panelServer) apiLLMChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent    string `json:"agent"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Question string `json:"question"`
	}
	decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, panelLLMBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		p.audit.record("llm_chat", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "запрос не разобран: обновите страницу (Ctrl+F5)"})
		return
	}
	question := strings.TrimSpace(body.Question)
	if question == "" {
		p.audit.record("llm_chat", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "вопрос пустой"})
		return
	}
	if utf8.RuneCountInString(question) > panelLLMChatQuestionRunes {
		p.audit.record("llm_chat", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "вопрос длиннее 500 символов"})
		return
	}
	cfg, err := readConfig(p.configPath)
	if err != nil {
		p.audit.record("llm_chat", r, "failed", nil)
		writePanelJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось прочитать настройки"})
		return
	}
	target, err := cfg.resolveLLM(agentConfig{
		Provider: normalizeProviderName(body.Provider),
		Model:    strings.TrimSpace(body.Model),
	})
	if err != nil {
		p.audit.record("llm_chat", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "нейросеть не сохранена: сначала сохраните её"})
		return
	}
	if target.BaseURL == "" || target.Model == "" {
		p.audit.record("llm_chat", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{
			"error": "не задана модель: выберите её в карточке или у нейросети",
		})
		return
	}
	// The role's own prompt when the role is saved and has one; otherwise the
	// shared voice-assistant prompt of the probe.
	system := panelLLMAskSystem
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == strings.TrimSpace(body.Agent) {
			if prompt := strings.TrimSpace(cfg.Agents[i].SystemPrompt); prompt != "" {
				system = prompt
			}
			break
		}
	}
	stored := map[string]string{}
	if p.settings.Secrets != nil {
		if file, loadErr := p.settings.Secrets.load(); loadErr == nil {
			stored = file.Providers
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelLLMAskTimeout)
	defer cancel()
	// A saved role's own token budget wins; otherwise the probe's default.
	tokens := panelLLMAskTokens
	if target.MaxOutputTokens > 0 {
		tokens = target.MaxOutputTokens
	}
	// The same chat tuning a role would use in a real turn.
	var chatParameters map[string]any
	if target.API == "chat_completions" {
		chatParameters = cfg.LLMChatParameters
		if target.ChatParameters != nil {
			chatParameters = target.ChatParameters
		}
	}
	client := &llm.Client{
		BaseURL: target.BaseURL, Model: target.Model, APIKey: apiKey(stored, target),
		API: target.API, ReasoningEffort: target.ReasoningEffort,
		MaxOutputTokens: tokens,
		ChatParameters:  chatParameters,
		HTTP:            &http.Client{Timeout: panelLLMAskTimeout},
	}
	start := time.Now()
	reply, chatErr := client.Complete(ctx, []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: question},
	}, false)
	elapsed := time.Since(start).Milliseconds()
	answer := map[string]any{
		"provider": target.Provider, "model": target.Model,
		"latency_ms": elapsed, "question": question,
	}
	audit := map[string]any{"provider": target.Provider, "latency_ms": elapsed}
	if chatErr != nil {
		answer["ok"] = false
		answer["verdict"] = explainLLMError(chatErr)
		answer["error"] = chatErr.Error()
		p.audit.record("llm_chat", r, "failed", audit)
		writePanelJSON(w, http.StatusOK, answer)
		return
	}
	answer["ok"] = true
	answer["answer"] = panelLLMChatAnswer(reply)
	answer["verdict"] = fmt.Sprintf("ответ получен за %d мс", elapsed)
	p.audit.record("llm_chat", r, "ok", audit)
	writePanelJSON(w, http.StatusOK, answer)
}

// panelLLMChatAnswer keeps the full answer but caps it for the page: a model
// that answers in paragraphs must not turn one question into a wall of text.
func panelLLMChatAnswer(answer string) string {
	answer = strings.Join(strings.Fields(answer), " ")
	if utf8.RuneCountInString(answer) <= panelLLMChatRuneLimit {
		return answer
	}
	runes := []rune(answer)
	return string(runes[:panelLLMChatRuneLimit]) + "…"
}

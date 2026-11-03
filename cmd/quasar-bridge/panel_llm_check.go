package main

import (
	"context"
	stdjson "encoding/json"
	"net/http"
	"strings"
	"time"

	"quasarbridge/internal/llm"
)

// apiLLMCheck is the one «Проверить» button: it proves the key and the address
// work, refreshes the model list (stored for the next page load) and, when a
// model is set, asks it one tiny question.
//
// The row may be unsaved. A key typed into the row is used as is; with an empty
// field the saved key of the provider is used, but only for the address the file
// holds — otherwise an edited address would receive a key that was issued for
// another one.
func (p *panelServer) apiLLMCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"base_url"`
		API      string `json:"api"`
		Model    string `json:"model"`
		APIKey   string `json:"api_key"`
	}
	decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, panelLLMBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		p.audit.record("llm_check", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "запрос не разобран: обновите страницу (Ctrl+F5)"})
		return
	}
	name := normalizeProviderName(body.Provider)
	baseURL := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	api := strings.TrimSpace(body.API)
	if api == "" {
		api = "chat_completions"
	}
	model := strings.TrimSpace(body.Model)
	if baseURL == "" || !validEndpoint(baseURL, "http", "https") {
		p.audit.record("llm_check", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "укажите корректный адрес API"})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" && name != "" {
		saved, storedKey, err := p.panelProvider(name)
		if err == nil {
			if strings.TrimRight(saved.BaseURL, "/") != baseURL || saved.API != api {
				p.audit.record("llm_check", r, "address_changed", map[string]any{"provider": name})
				writePanelJSON(w, http.StatusBadRequest, map[string]string{
					"error": "адрес изменён: введите ключ заново или сначала сохраните",
				})
				return
			}
			key = storedKey
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*panelLLMProbeTimeout)
	defer cancel()
	answer := map[string]any{"provider": name, "model": model}
	models, err := fetchLLMModelsAPI(ctx, baseURL, key, api)
	if err != nil {
		answer["models_error"] = explainLLMError(err)
	} else {
		answer["models"] = models
		if name != "" {
			_ = storeLLMModels(p.configPath, name, models)
		}
	}
	if model != "" {
		start := time.Now()
		client := &llm.Client{
			BaseURL: baseURL, Model: model, APIKey: key, API: api,
			MaxOutputTokens: panelLLMProbeTokens,
			HTTP:            &http.Client{Timeout: panelLLMProbeTimeout},
		}
		reply, chatErr := client.Complete(ctx, []llm.Message{
			{Role: "system", Content: "Reply with a single short word."},
			{Role: "user", Content: "ping"},
		}, false)
		answer["latency_ms"] = time.Since(start).Milliseconds()
		if chatErr != nil {
			answer["chat_error"] = explainLLMError(chatErr)
		} else {
			answer["chat_ok"] = true
			answer["sample"] = panelLLMSample(reply)
		}
	}
	_, hasModels := answer["models"]
	chatOK, _ := answer["chat_ok"].(bool)
	answer["ok"] = hasModels || chatOK
	result := "ok"
	if answer["ok"] != true {
		result = "failed"
	}
	p.audit.record("llm_check", r, result, map[string]any{"provider": name})
	writePanelJSON(w, http.StatusOK, answer)
}

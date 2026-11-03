package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"quasarbridge/internal/llm"
)

// The panel probes a provider through the bridge, never from the browser: the
// key must not reach the page and the station, not the operator's laptop, is
// the host that will talk to the API.
const (
	panelLLMBodyLimit    = 4 << 10
	panelLLMProbeTimeout = 10 * time.Second
	panelLLMProbeTokens  = 32
	panelLLMModelsBytes  = 2 << 20
	panelLLMModelsLimit  = 500
	panelLLMSampleRunes  = 200
)

// panelProvider resolves one saved provider and its credential. An unknown name
// is a bad request, not a server error: the page may still hold a list the file
// no longer has.
func (p *panelServer) panelProvider(name string) (llmProviderConfig, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return llmProviderConfig{}, "", errors.New("provider is required")
	}
	cfg, err := readConfig(p.configPath)
	if err != nil {
		return llmProviderConfig{}, "", err
	}
	provider, ok := cfg.providerByName(name)
	if !ok {
		return llmProviderConfig{}, "", errors.New("provider is not configured")
	}
	stored := map[string]string{}
	if p.settings.Secrets != nil {
		if file, loadErr := p.settings.Secrets.load(); loadErr == nil {
			stored = file.Providers
		}
	}
	// Same order as a role build: the stored key first, then the deployment
	// environment. A provider without a key is legal (a local server).
	key := apiKey(stored, llmTarget{Provider: provider.Name})
	return provider, key, nil
}

// decodePanelLLMRequest reads the one-field body of both probe endpoints. The
// decoder rejects unknown members, so a page that still sends an old field is
// told instead of silently probing the wrong provider.
func decodePanelLLMRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	var body struct {
		Provider string `json:"provider"`
	}
	decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, panelLLMBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return "", errors.New("invalid request body")
	}
	return strings.TrimSpace(body.Provider), nil
}

// apiLLMTest asks the provider one tiny question. It is a real round trip: the
// point is to prove the key, the address and the model agree, which no static
// validation can do. The key and the request body are never logged or audited.
func (p *panelServer) apiLLMTest(w http.ResponseWriter, r *http.Request) {
	name, err := decodePanelLLMRequest(w, r)
	if err != nil {
		p.audit.record("llm_test", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	provider, key, err := p.panelProvider(name)
	if err != nil {
		p.audit.record("llm_test", r, "bad_request", map[string]any{"provider": normalizeProviderName(name)})
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelLLMProbeTimeout)
	defer cancel()
	client := &llm.Client{
		BaseURL: provider.BaseURL, Model: provider.Model, APIKey: key,
		API: provider.API, ReasoningEffort: provider.ReasoningEffort,
		MaxOutputTokens: panelLLMProbeTokens,
		HTTP:            &http.Client{Timeout: panelLLMProbeTimeout},
	}
	start := time.Now()
	answer, err := client.Complete(ctx, []llm.Message{
		{Role: "system", Content: "Reply with a single short word."},
		{Role: "user", Content: "ping"},
	}, false)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		// llm.Client errors carry an endpoint or a status code, never the key.
		p.audit.record("llm_test", r, "failed", map[string]any{"provider": provider.Name, "latency_ms": elapsed})
		writePanelJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "provider": provider.Name, "model": provider.Model,
			"latency_ms": elapsed, "error": err.Error(),
		})
		return
	}
	p.audit.record("llm_test", r, "ok", map[string]any{"provider": provider.Name, "latency_ms": elapsed})
	writePanelJSON(w, http.StatusOK, map[string]any{
		"ok": true, "provider": provider.Name, "model": provider.Model,
		"latency_ms": elapsed, "sample": panelLLMSample(answer),
	})
}

// panelLLMSample keeps a short, single-line preview of the answer. A provider
// that echoes a long text must not turn the audit-free response into a dump.
func panelLLMSample(answer string) string {
	answer = strings.Join(strings.Fields(answer), " ")
	if utf8.RuneCountInString(answer) <= panelLLMSampleRunes {
		return answer
	}
	runes := []rune(answer)
	return string(runes[:panelLLMSampleRunes]) + "…"
}

// apiLLMModels lists the models the provider offers, so the operator picks one
// instead of typing it. Every provider shape seen in practice is accepted:
// OpenAI's {"data":[{"id":…}]} and Ollama's {"models":[{"name":…}]}. A provider
// that refuses the list is not an error the page cannot handle: the model field
// stays a free-text input.
func (p *panelServer) apiLLMModels(w http.ResponseWriter, r *http.Request) {
	name, err := decodePanelLLMRequest(w, r)
	if err != nil {
		p.audit.record("llm_models", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	provider, key, err := p.panelProvider(name)
	if err != nil {
		p.audit.record("llm_models", r, "bad_request", map[string]any{"provider": normalizeProviderName(name)})
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelLLMProbeTimeout)
	defer cancel()
	models, err := fetchLLMModelsAPI(ctx, provider.BaseURL, key, provider.API)
	if err != nil {
		p.audit.record("llm_models", r, "failed", map[string]any{"provider": provider.Name})
		writePanelJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	_ = storeLLMModels(p.configPath, provider.Name, models)
	p.audit.record("llm_models", r, "ok", map[string]any{"provider": provider.Name, "models": len(models)})
	writePanelJSON(w, http.StatusOK, map[string]any{"provider": provider.Name, "models": models})
}

// parseModelList reads the two documented shapes and returns unique, sorted ids.
// Anything else is reported as "not a model list" instead of an empty answer,
// which the page would otherwise render as "no models".
func parseModelList(data []byte) ([]string, error) {
	var payload struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
		Models []stdjson.RawMessage `json:"models"`
	}
	if err := stdjson.Unmarshal(data, &payload); err != nil {
		return nil, errors.New("provider response is not a model list")
	}
	ids := make(map[string]bool, len(payload.Data)+len(payload.Models))
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			ids[id] = true
		} else if name := strings.TrimSpace(item.Name); name != "" {
			ids[name] = true
		}
	}
	for _, raw := range payload.Models {
		var object struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := stdjson.Unmarshal(raw, &object); err == nil {
			if id := strings.TrimSpace(object.ID); id != "" {
				ids[id] = true
			} else if name := strings.TrimSpace(object.Name); name != "" {
				ids[name] = true
			}
			continue
		}
		var plain string
		if err := stdjson.Unmarshal(raw, &plain); err == nil {
			if id := strings.TrimSpace(plain); id != "" {
				ids[id] = true
			}
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("provider response is not a model list")
	}
	models := make([]string, 0, len(ids))
	for id := range ids {
		models = append(models, id)
	}
	sort.Strings(models)
	if len(models) > panelLLMModelsLimit {
		models = models[:panelLLMModelsLimit]
	}
	return models, nil
}

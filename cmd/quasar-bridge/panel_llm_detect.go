package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"quasarbridge/internal/llm"
)

// The panel fills the endpoint fields from the key the operator pastes. The
// station, not the browser, does the request: the key never leaves the box and
// never reaches the page again.
//
// Only providers with an OpenAI-compatible /models endpoint are listed. Sending
// a key to a provider it does not belong to would disclose it, so the guess is
// made from the key itself and never by probing a list of candidates.
type llmGuess struct {
	name    string
	baseURL string
	// api is the wire format the provider speaks; every entry below is
	// OpenAI-compatible, which is the format a role uses by default.
	api string
	// prefixes are distinctive key prefixes.
	prefixes []string
	// generic marks the bare "sk-" family, where only the key length separates
	// DeepSeek from a legacy OpenAI key. A generic guess is matched last, so a
	// distinctive prefix always wins.
	generic bool
	lengths []int
}

// llmGuesses is a variable, not a constant, so a test can point a provider at a
// local server instead of the real API.
var llmGuesses = []llmGuess{
	{name: "openrouter", baseURL: "https://openrouter.ai/api/v1", api: "chat_completions", prefixes: []string{"sk-or-v1-", "sk-or-"}},
	{name: "openai", baseURL: "https://api.openai.com/v1", api: "chat_completions", prefixes: []string{"sk-proj-", "sk-svcacct-", "sk-admin-", "sk-None-"}},
	{name: "groq", baseURL: "https://api.groq.com/openai/v1", api: "chat_completions", prefixes: []string{"gsk_"}},
	{name: "xai", baseURL: "https://api.x.ai/v1", api: "chat_completions", prefixes: []string{"xai-"}},
	{name: "google", baseURL: "https://generativelanguage.googleapis.com/v1beta/openai", api: "chat_completions", prefixes: []string{"AIza"}},
	// A DeepSeek key is "sk-" plus 32 characters, a legacy OpenAI key is "sk-"
	// plus 48. Anything else in this family stays unrecognised on purpose.
	{name: "deepseek", baseURL: "https://api.deepseek.com/v1", api: "chat_completions", prefixes: []string{"sk-"}, generic: true, lengths: []int{35}},
	{name: "openai", baseURL: "https://api.openai.com/v1", api: "chat_completions", prefixes: []string{"sk-"}, generic: true, lengths: []int{51}},
}

// guessLLMProvider recognises the provider from the key shape alone.
func guessLLMProvider(key string) (llmGuess, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return llmGuess{}, false
	}
	for _, guess := range llmGuesses {
		if guess.generic {
			continue
		}
		for _, prefix := range guess.prefixes {
			if strings.HasPrefix(key, prefix) {
				return guess, true
			}
		}
	}
	for _, guess := range llmGuesses {
		if !guess.generic {
			continue
		}
		matched := false
		for _, prefix := range guess.prefixes {
			if strings.HasPrefix(key, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		for _, length := range guess.lengths {
			if len(key) == length {
				return guess, true
			}
		}
	}
	return llmGuess{}, false
}

// fetchLLMModels asks a provider for its model list. The "Модели" button and the
// key autodetect share it, so both accept the two documented wire shapes.
func fetchLLMModels(ctx context.Context, baseURL, key string) ([]string, error) {
	return fetchLLMModelsAPI(ctx, baseURL, key, "")
}

// fetchLLMModelsAPI is fetchLLMModels for a provider with its own wire format:
// GigaChat exchanges the key for a token and trusts the Russian CA.
func fetchLLMModelsAPI(ctx context.Context, baseURL, key, api string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, errors.New("invalid provider endpoint")
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: panelLLMProbeTimeout}
	if api == llm.APIGigaChat {
		token, err := llm.GigaAccessToken(ctx, key)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		client = llm.GigaHTTPClient(panelLLMProbeTimeout)
	} else if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("provider is unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("provider returned HTTP " + strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, panelLLMModelsBytes+1))
	if err != nil {
		return nil, errors.New("cannot read provider response")
	}
	if len(data) > panelLLMModelsBytes {
		return nil, errors.New("provider response is too large")
	}
	return parseModelList(data)
}

// apiLLMDetect turns a pasted key into the fields a provider row needs: name,
// address, wire format and the model list. A key that is not recognised is not
// an error: the answer says so and the operator fills the row by hand.
func (p *panelServer) apiLLMDetect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"api_key"`
		// Preset is the id of an entry of llmPresets the operator picked in the
		// «Сервис» list. A named service is not guessed from the key.
		Preset string `json:"preset"`
		// Provider is the row name; the model list found is stored under it.
		Provider string `json:"provider"`
	}
	decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, panelLLMBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		p.audit.record("llm_detect", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "запрос не разобран: обновите страницу (Ctrl+F5)"})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		p.audit.record("llm_detect", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "вставьте ключ"})
		return
	}
	guess, ok := llmGuess{}, false
	if preset, found := llmPresetByID(strings.TrimSpace(body.Preset)); found {
		guess, ok = llmGuess{name: preset.Name, baseURL: preset.BaseURL, api: preset.API}, true
	} else {
		guess, ok = guessLLMProvider(key)
	}
	if !ok {
		// The key is never logged, and the answer is not an error the page has to
		// shout about: an unknown key simply means "fill the row yourself".
		p.audit.record("llm_detect", r, "unknown", nil)
		writePanelJSON(w, http.StatusOK, map[string]any{
			"detected": false,
			"note":     "провайдер не распознан по ключу — впишите адрес API вручную",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelLLMProbeTimeout)
	defer cancel()
	models, err := fetchLLMModelsAPI(ctx, guess.baseURL, key, guess.api)
	if err != nil {
		// The address is still right for the recognised key, so the page fills the
		// row and shows the reason instead of refusing the whole guess.
		p.audit.record("llm_detect", r, "failed", map[string]any{"provider": guess.name})
		writePanelJSON(w, http.StatusOK, map[string]any{
			"detected": true, "verified": false,
			"provider": guess.name, "base_url": guess.baseURL, "api": guess.api,
			"note": explainLLMError(err),
		})
		return
	}
	storeName := strings.TrimSpace(body.Provider)
	if storeName == "" {
		storeName = guess.name
	}
	_ = storeLLMModels(p.configPath, storeName, models)
	p.audit.record("llm_detect", r, "ok", map[string]any{"provider": guess.name, "models": len(models)})
	writePanelJSON(w, http.StatusOK, map[string]any{
		"detected": true, "verified": true,
		"provider": guess.name, "base_url": guess.baseURL, "api": guess.api,
		"models": models,
	})
}

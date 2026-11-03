package main

import (
	stdjson "encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// llmModelsFile keeps the last model list each provider returned, so the panel
// shows the list after a reload instead of an empty field. It holds model ids
// only: no key, no address.
const llmModelsFile = "llm_models.json"

var llmModelsMu sync.Mutex

func llmModelsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), llmModelsFile)
}

func readLLMModels(configPath string) map[string][]string {
	data, err := os.ReadFile(llmModelsPath(configPath))
	if err != nil {
		return map[string][]string{}
	}
	var out map[string][]string
	if stdjson.Unmarshal(data, &out) != nil || out == nil {
		return map[string][]string{}
	}
	return out
}

// storeLLMModels replaces the list of one provider. An empty list is not stored:
// a provider that refused the request must not wipe the list the page has.
func storeLLMModels(configPath, provider string, models []string) error {
	provider = normalizeProviderName(provider)
	if provider == "" || len(models) == 0 {
		return nil
	}
	llmModelsMu.Lock()
	defer llmModelsMu.Unlock()
	all := readLLMModels(configPath)
	all[provider] = append([]string(nil), models...)
	data, err := stdjson.Marshal(all)
	if err != nil {
		return err
	}
	return writeFileAtomic(llmModelsPath(configPath), data)
}

// providerModelsFor returns the stored lists of the configured providers only.
func providerModelsFor(configPath string, cfg config) map[string][]string {
	all := readLLMModels(configPath)
	out := make(map[string][]string, len(cfg.LLMProviders))
	for _, p := range cfg.LLMProviders {
		if list := all[p.Name]; len(list) > 0 {
			out[p.Name] = list
		}
	}
	return out
}

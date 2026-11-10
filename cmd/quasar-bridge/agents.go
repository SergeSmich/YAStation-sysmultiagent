package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
)

// defaultLLMAPIKeyEnv is the credential source the deployment already uses. A
// role may name its own variable when it talks to a different provider.
const defaultLLMAPIKeyEnv = "QUASAR_LLM_API_KEY"

func apiKeyEnv(name string) string {
	if name == "" {
		return defaultLLMAPIKeyEnv
	}
	return name
}

// apiKey resolves the credential of one role: the key the panel stored for its
// provider, then the environment variable the role names, then the deployment
// default. The value is never logged and never returned in an error.
func apiKey(stored map[string]string, target llmTarget) string {
	if target.Provider != "" {
		if key := strings.TrimSpace(stored[target.Provider]); key != "" {
			return key
		}
	}
	return os.Getenv(apiKeyEnv(target.APIKeyEnv))
}

// providerNames lists the configured endpoints for the startup log. Names only:
// a key must never reach a log line, and a URL may carry userinfo.
func providerNames(cfg config) []string {
	names := make([]string, 0, len(cfg.LLMProviders))
	for _, p := range cfg.LLMProviders {
		names = append(names, p.Name)
	}
	return names
}

// secretKeys reads the provider credentials once per build. A broken file is
// reported and treated as empty: every role falls back to its environment
// variable, exactly as before the file existed.
func secretKeys(secrets *secretStore) map[string]string {
	if secrets == nil {
		return map[string]string{}
	}
	file, err := secrets.load()
	if err != nil {
		slog.Warn("secrets_unreadable", "error", err)
		return map[string]string{}
	}
	return file.Providers
}

// chatParametersFor picks the chat tuning of one target: a role that speaks a
// different API gets no chat-only parameters, and a role that brings its own
// table replaces the shared one.
func chatParametersFor(cfg config, target llmTarget) map[string]any {
	if target.API != "chat_completions" {
		return nil
	}
	if target.ChatParameters != nil {
		return target.ChatParameters
	}
	return cfg.LLMChatParameters
}

// mainAnswerer is the pair that answers route llm.
type mainAnswerer struct {
	// LLM thinks: the classifier and the replay path.
	LLM bridge.Completer
	// Answerer speaks to the person; it may carry the assistant tools.
	Answerer bridge.Completer
	// Prompt is the role prompt, empty for the shared assistant prompt.
	Prompt string
	// Role is the configured role name, empty when llm_* answers.
	Role     string
	Provider string
	Model    string
}

// buildMainAnswerer resolves main_agent. An empty value keeps the deployment
// client; a role contributes its provider, model, tuning and prompt. A role
// never contributes its sysadmin or peers toolset: route llm catches every
// phrase the rules did not, so it must not be able to act. The assistant tools
// stay a deployment switch, so they wrap the role's client when they are on.
func buildMainAnswerer(cfg config, tools *assistant.Tools, secrets *secretStore, root *llm.Client, rootAnswerer bridge.Completer) (mainAnswerer, error) {
	name := strings.TrimSpace(cfg.MainAgent)
	if name == "" {
		main := mainAnswerer{LLM: root, Answerer: rootAnswerer, Model: cfg.LLMModel}
		if main.Answerer == nil {
			main.Answerer = root
		}
		return main, nil
	}
	for i := range cfg.Agents {
		a := cfg.Agents[i]
		if a.Name != name {
			continue
		}
		target, err := cfg.resolveLLM(a)
		if err != nil {
			return mainAnswerer{}, err
		}
		client := &llm.Client{
			BaseURL: target.BaseURL, Model: target.Model, APIKey: apiKey(secretKeys(secrets), target),
			API: target.API, ReasoningEffort: target.ReasoningEffort,
			MaxOutputTokens: target.MaxOutputTokens, HostedTools: target.HostedTools,
			ChatParameters: chatParametersFor(cfg, target),
		}
		main := mainAnswerer{
			LLM: client, Answerer: client, Prompt: a.SystemPrompt,
			Role: a.Name, Provider: target.Provider, Model: target.Model,
		}
		if cfg.AssistantTools.Enabled && tools != nil {
			main.Answerer = &assistant.Agent{Client: client, Tools: tools,
				MaxRounds: cfg.AssistantTools.MaxRounds, MaxCalls: cfg.AssistantTools.MaxCalls}
		}
		return main, nil
	}
	return mainAnswerer{}, fmt.Errorf("main_agent %q is not a configured role", name)
}

// buildAgents creates the configured roles. Every role owns its model client, so
// a reload replaces the whole registry instead of mutating a shared client. The
// endpoint comes from the provider the role names, from the role's own fields
// or from the top-level llm_* values, in that order.
func buildAgents(cfg config, tools *assistant.Tools, secrets *secretStore, sysHub *sysadminHub) (map[string]*bridge.Agent, error) {
	if len(cfg.Agents) == 0 {
		return nil, nil
	}
	stored := secretKeys(secrets)
	agents := make(map[string]*bridge.Agent, len(cfg.Agents))
	hub := &peerHub{}
	for _, a := range cfg.Agents {
		target, err := cfg.resolveLLM(a)
		if err != nil {
			return nil, err
		}
		// Chat tuning is shared with the default client unless the role speaks a
		// different API, where the bridge refuses chat-only parameters.
		chatParameters := chatParametersFor(cfg, target)
		client := &llm.Client{
			BaseURL: target.BaseURL, Model: target.Model, APIKey: apiKey(stored, target),
			API: target.API, ReasoningEffort: target.ReasoningEffort,
			MaxOutputTokens: target.MaxOutputTokens, HostedTools: target.HostedTools,
			ChatParameters: chatParameters,
		}
		var answerer bridge.Completer = client
		var toolset assistant.Toolset
		if a.AssistantTools {
			if tools == nil {
				return nil, fmt.Errorf("agent %q needs assistant_tools.enabled", a.Name)
			}
			toolset = tools
		}
		prompt := a.SystemPrompt
		if a.Sysadmin {
			if sysHub == nil {
				return nil, fmt.Errorf("agent %q: sysadmin is unavailable", a.Name)
			}
			toolset = &sysadminToolset{base: toolset, self: a.Name, hub: sysHub}
			prompt += sysadminPrompt
		}
		if a.Peers {
			toolset = &peerToolset{base: toolset, self: a.Name, hub: hub}
		}
		if toolset != nil {
			answerer = &assistant.Agent{Client: client, Tools: toolset, MaxRounds: cfg.AssistantTools.MaxRounds, MaxCalls: cfg.AssistantTools.MaxCalls}
		}
		ba := &bridge.Agent{Name: a.Name, Answerer: answerer, Prompt: prompt, Description: a.Description, Peers: a.Peers, Sysadmin: a.Sysadmin}
		if a.Sysadmin {
			ba.PromptExtra = sysHub.CharterPrompt
		}
		agents[a.Name] = ba
	}
	hub.agents.Store(&agents)
	return agents, nil
}

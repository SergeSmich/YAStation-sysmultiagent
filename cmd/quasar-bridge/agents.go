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

// buildAgents creates the configured roles. Every role owns its model client, so
// a reload replaces the whole registry instead of mutating a shared client. The
// endpoint comes from the provider the role names, from the role's own fields
// or from the top-level llm_* values, in that order.
func buildAgents(cfg config, tools *assistant.Tools, secrets *secretStore, sysHub *sysadminHub) (map[string]*bridge.Agent, error) {
	if len(cfg.Agents) == 0 {
		return nil, nil
	}
	stored := map[string]string{}
	if secrets != nil {
		file, err := secrets.load()
		if err != nil {
			// A broken secrets file must not take the bridge down: every role
			// falls back to its environment variable, exactly as before the file
			// existed, and the panel still reports the key as unset.
			slog.Warn("secrets_unreadable", "error", err)
		}
		stored = file.Providers
	}
	agents := make(map[string]*bridge.Agent, len(cfg.Agents))
	hub := &peerHub{}
	for _, a := range cfg.Agents {
		target, err := cfg.resolveLLM(a)
		if err != nil {
			return nil, err
		}
		// Chat tuning is shared with the default client unless the role speaks a
		// different API, where the bridge refuses chat-only parameters.
		var chatParameters map[string]any
		if target.API == "chat_completions" {
			chatParameters = cfg.LLMChatParameters
		}
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
		agents[a.Name] = &bridge.Agent{Name: a.Name, Answerer: answerer, Prompt: prompt, Description: a.Description, Peers: a.Peers, Sysadmin: a.Sysadmin}
	}
	hub.agents.Store(&agents)
	return agents, nil
}

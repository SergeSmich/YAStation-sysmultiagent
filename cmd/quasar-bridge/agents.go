package main

import (
	"fmt"
	"os"

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

// buildAgents creates the configured roles. Every role owns its model client, so
// a reload replaces the whole registry instead of mutating a shared client.
func buildAgents(cfg config, tools *assistant.Tools) (map[string]*bridge.Agent, error) {
	if len(cfg.Agents) == 0 {
		return nil, nil
	}
	agents := make(map[string]*bridge.Agent, len(cfg.Agents))
	for _, a := range cfg.Agents {
		// Chat tuning is shared with the default client unless the role speaks a
		// different API, where the bridge refuses chat-only parameters.
		var chatParameters map[string]any
		if a.API == "chat_completions" {
			chatParameters = cfg.LLMChatParameters
		}
		client := &llm.Client{
			BaseURL: a.BaseURL, Model: a.Model, APIKey: os.Getenv(apiKeyEnv(a.APIKeyEnv)),
			API: a.API, ReasoningEffort: a.ReasoningEffort,
			MaxOutputTokens: a.MaxOutputTokens, HostedTools: a.HostedTools,
			ChatParameters: chatParameters,
		}
		var answerer bridge.Completer = client
		if a.AssistantTools {
			if tools == nil {
				return nil, fmt.Errorf("agent %q needs assistant_tools.enabled", a.Name)
			}
			answerer = &assistant.Agent{Client: client, Tools: tools, MaxRounds: cfg.AssistantTools.MaxRounds, MaxCalls: cfg.AssistantTools.MaxCalls}
		}
		agents[a.Name] = &bridge.Agent{Name: a.Name, Answerer: answerer, Prompt: a.SystemPrompt}
	}
	return agents, nil
}

package main

import (
	"testing"

	"quasarbridge/internal/llm"
)

const providerWithParams = `{
	"llm_chat_parameters":{"temperature":0.3},
	"llm_providers":[
		{"name":"groq","api":"chat_completions","base_url":"https://api.groq.com/openai/v1","model":"openai/gpt-oss-120b",
		 "chat_parameters":{"reasoning_effort":"low"}},
		{"name":"deepseek","api":"chat_completions","base_url":"https://api.deepseek.com/v1","model":"deepseek-chat"}],
	"agents":[{"name":"fast","provider":"groq"},{"name":"plain","provider":"deepseek"}]
}`

// A provider's own chat parameters replace the shared ones for its roles only.
func TestProviderChatParametersReachItsRoles(t *testing.T) {
	cfg, err := readConfig(configFile(t, providerWithParams))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := buildAgents(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fast := agents["fast"].Answerer.(*llm.Client)
	if fast.ChatParameters["reasoning_effort"] != "low" {
		t.Fatalf("provider parameters lost: %+v", fast.ChatParameters)
	}
	if _, shared := fast.ChatParameters["temperature"]; shared {
		t.Fatalf("shared parameters leaked into the provider's own set: %+v", fast.ChatParameters)
	}
	plain := agents["plain"].Answerer.(*llm.Client)
	if plain.ChatParameters["temperature"] != 0.3 || plain.ChatParameters["reasoning_effort"] != nil {
		t.Fatalf("role without own parameters must inherit the shared ones: %+v", plain.ChatParameters)
	}
}

func TestProviderChatParametersAreValidated(t *testing.T) {
	for _, body := range []string{
		`{"llm_providers":[{"name":"a","base_url":"https://a.example/v1","model":"m","chat_parameters":{"stream":true}}]}`,
		`{"llm_providers":[{"name":"a","base_url":"https://a.example/v1","model":"m","chat_parameters":{"reasoning_effort":"fast"}}]}`,
		`{"llm_providers":[{"name":"a","base_url":"https://a.example/v1","model":"m","chat_parameters":{"reasoning_effort":5}}]}`,
		`{"llm_providers":[{"name":"a","api":"responses","base_url":"https://a.example/v1","model":"m","chat_parameters":{"temperature":0}}]}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	cfg, err := readConfig(configFile(t, `{"llm_providers":[{"name":"a","base_url":"https://a.example/v1","model":"m","chat_parameters":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMProviders[0].ChatParameters != nil {
		t.Fatalf("an empty set must normalise to nil: %+v", cfg.LLMProviders[0].ChatParameters)
	}
}

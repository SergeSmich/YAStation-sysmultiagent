package main

import (
	"os"
	"strings"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
)

func TestReadConfigAgentsInheritLLMSettings(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{
		"llm_base_url":"https://llm.example.invalid/v1",
		"llm_model":"your-model",
		"llm_max_output_tokens":2048,
		"agents":[
			{"name":"briefing","voice":"omazh","system_prompt":"Ты роль briefing."},
			{"name":"house","model":"other-model","max_output_tokens":512}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("agents: %+v", cfg.Agents)
	}
	first := cfg.Agents[0]
	if first.Model != "your-model" || first.BaseURL != "https://llm.example.invalid/v1" || first.API != "chat_completions" || first.MaxOutputTokens != 2048 || first.APIKeyEnv != "" {
		t.Fatalf("inherited fields: %+v", first)
	}
	if cfg.Agents[1].Model != "other-model" || cfg.Agents[1].MaxOutputTokens != 512 || cfg.Agents[1].BaseURL != "https://llm.example.invalid/v1" {
		t.Fatalf("explicit fields: %+v", cfg.Agents[1])
	}
	if got := cfg.LocalVoice.Routes["agent:briefing"]; got != "omazh" {
		t.Fatalf("voice folded into routes: %+v", cfg.LocalVoice.Routes)
	}
	if _, ok := cfg.LocalVoice.Routes["agent:house"]; ok {
		t.Fatal("role without a voice gained a route")
	}
}

func TestReadConfigWithoutAgentsIsUnchanged(t *testing.T) {
	for _, body := range []string{`{}`, `{"agents":[]}`} {
		cfg, err := readConfig(configFile(t, body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if cfg.Agents != nil || cfg.LocalVoice.Routes != nil {
			t.Fatalf("%s: agents=%+v routes=%+v", body, cfg.Agents, cfg.LocalVoice.Routes)
		}
	}
}

func TestReadConfigRejectsInvalidAgents(t *testing.T) {
	for _, body := range []string{
		`{"llm_model":"m","agents":[{"name":"Briefing"}]}`,
		`{"llm_model":"m","agents":[{"name":""}]}`,
		`{"llm_model":"m","agents":[{"name":"a b"}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing"},{"name":"briefing"}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","api":"grpc"}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","base_url":"file:///tmp/socket"}]}`,
		`{"agents":[{"name":"briefing"}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","max_output_tokens":-1}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","reasoning_effort":"low"}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","hosted_tools":["web_search"]}]}`,
		`{"llm_api":"responses","llm_model":"m","agents":[{"name":"briefing","hosted_tools":["browser"]}]}`,
		`{"llm_api":"responses","llm_model":"m","agents":[{"name":"briefing","hosted_tools":["web_search","web_search"]}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","voice":"unknown-voice"}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","assistant_tools":true}]}`,
		`{"llm_model":"m","agents":[{"name":"briefing","system_prompt":"` + strings.Repeat("я", bridge.MaxAgentPromptRunes+1) + `"}]}`,
		`{"llm_model":"m","local_voice":{"routes":{"agent:briefing":"jane"}},"agents":[{"name":"briefing","voice":"omazh"}]}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestBuildAgentsUsesOwnClientAndKey(t *testing.T) {
	t.Setenv("BRIEFING_KEY", "secret-briefing")
	cfg, err := readConfig(configFile(t, `{
		"llm_base_url":"https://llm.example.invalid/v1",
		"llm_model":"your-model",
		"llm_chat_parameters":{"temperature":null},
		"agents":[{"name":"briefing","api_key_env":"BRIEFING_KEY","system_prompt":"Ты роль briefing."}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := buildAgents(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	role, ok := agents["briefing"]
	if !ok || role.Prompt != "Ты роль briefing." {
		t.Fatalf("agents: %+v", agents)
	}
	client, ok := role.Answerer.(*llm.Client)
	if !ok {
		t.Fatalf("answerer: %T", role.Answerer)
	}
	if client.BaseURL != "https://llm.example.invalid/v1" || client.Model != "your-model" || client.APIKey != "secret-briefing" {
		t.Fatalf("client: %+v", client)
	}
	if _, ok := client.ChatParameters["temperature"]; !ok {
		t.Fatalf("chat parameters not inherited: %+v", client.ChatParameters)
	}
	if agents, err := buildAgents(config{}, nil, nil); err != nil || agents != nil {
		t.Fatalf("empty registry: %+v %v", agents, err)
	}
}

func TestBuildAgentsWrapsAssistantTools(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{
		"llm_model":"your-model",
		"assistant_tools":{"enabled":true},
		"agents":[{"name":"briefing","assistant_tools":true}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	tools := &assistant.Tools{Config: cfg.AssistantTools}
	agents, err := buildAgents(cfg, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agents["briefing"].Answerer.(*assistant.Agent); !ok {
		t.Fatalf("answerer: %T", agents["briefing"].Answerer)
	}
}

func TestBuildAgentsFailsWithoutTools(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"llm_model":"your-model","assistant_tools":{"enabled":true},"agents":[{"name":"briefing","assistant_tools":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildAgents(cfg, nil, nil); err == nil {
		t.Fatal("role with tools accepted without a tools instance")
	}
}

func TestReloadAgentsRejectsUnrelatedChanges(t *testing.T) {
	path := configFile(t, `{"llm_model":"your-model","agents":[{"name":"briefing"}]}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	applied := 0
	apply := func(config) error {
		applied++
		return nil
	}
	if err := os.WriteFile(path, []byte(`{"llm_model":"other-model","agents":[{"name":"briefing"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, engine, reloadHooks{applyAgents: apply}); err == nil || !strings.Contains(err.Error(), "restart required") {
		t.Fatalf("unrelated change accepted: %v", err)
	}
	if applied != 0 {
		t.Fatal("roles applied on a rejected reload")
	}
}

func TestReloadAgentsIsHotApplied(t *testing.T) {
	path := configFile(t, `{"llm_model":"your-model","agents":[{"name":"briefing","system_prompt":"старый"}]}`)
	initial, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := new(bridge.Engine)
	if err := engine.SetRouting(initial.Routing); err != nil {
		t.Fatal(err)
	}
	applied := 0
	apply := func(next config) error {
		applied++
		agents, err := buildAgents(next, nil, nil)
		if err != nil {
			return err
		}
		return engine.SetAgents(agents)
	}
	if err := apply(initial); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"llm_model":"your-model","agents":[{"name":"briefing","system_prompt":"новый"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reloadRouting(path, initial, engine, reloadHooks{applyAgents: apply}); err != nil {
		t.Fatalf("agent change required a restart: %v", err)
	}
	if applied != 2 {
		t.Fatalf("apply called %d times", applied)
	}
	role, ok := engine.Agent("briefing")
	if !ok || role.Prompt != "новый" {
		t.Fatalf("role: %+v %v", role, ok)
	}
}

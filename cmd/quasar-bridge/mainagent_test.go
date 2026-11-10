package main

import (
	"net/http"
	"strings"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
)

// main_agent points route llm at a role. The tests below cover the three ways it
// can be wrong (unknown role, no model, empty value) and the two ways it must not
// grow: tools from the role, and a silent fall back to llm_*.

func mainAgentConfig() config {
	return config{
		LLMModel: "root-model",
		LLMAPI:   "chat_completions",
		LLMProviders: []llmProviderConfig{{
			Name: "main", API: "chat_completions", BaseURL: "https://llm.example.invalid/v1", Model: "role-model",
		}},
		Agents: []agentConfig{
			{Name: "helper", Provider: "main", SystemPrompt: "Ты — хелпер."},
			{Name: "rooted", SystemPrompt: "Ты — сисадмин.", Sysadmin: true, Peers: true},
			{Name: "bare", SystemPrompt: "Ты — без модели."},
		},
	}
}

func TestValidateMainAgent(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"empty keeps llm_*", "", ""},
		{"known role", "helper", ""},
		{"unknown role", "ghost", "not a configured role"},
		{"role without a model", "bare", "has no model"},
		{"uppercase name", "Helper", "main_agent must be"},
		{"spaces around the name", "  helper  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mainAgentConfig()
			cfg.MainAgent = tc.value
			// validateAgents runs first in readConfig and would already reject a
			// role without a model, so the check is exercised on its own: it is the
			// last line of defence for a hand-edited file.
			err := cfg.validateMainAgent()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("rejected a valid main_agent: %v", err)
				}
				if cfg.MainAgent != strings.TrimSpace(tc.value) {
					t.Fatalf("main_agent stored as %q", cfg.MainAgent)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestReadConfigRejectsUnknownMainAgent: a start must refuse the file instead of
// answering with another model than the one the operator chose.
func TestReadConfigRejectsUnknownMainAgent(t *testing.T) {
	path := configFile(t, `{
		"llm_model":"root-model",
		"main_agent":"ghost",
		"agents":[{"name":"helper","system_prompt":"Ты — хелпер."}]
	}`)
	if _, err := readConfig(path); err == nil {
		t.Fatal("a configuration with an unknown main_agent was accepted")
	}
}

func TestBuildMainAnswererKeepsTheDeploymentClient(t *testing.T) {
	cfg := mainAgentConfig()
	root := &llm.Client{Model: "root-model"}
	rootAnswerer := &assistant.Agent{Client: root}
	built, err := buildMainAnswerer(cfg, nil, nil, root, rootAnswerer)
	if err != nil {
		t.Fatal(err)
	}
	if built.Role != "" {
		t.Fatalf("role=%q for an empty main_agent", built.Role)
	}
	if built.LLM != bridge.Completer(root) || built.Answerer != bridge.Completer(rootAnswerer) {
		t.Fatal("an empty main_agent must keep the deployment clients")
	}
}

func TestBuildMainAnswererUsesTheRole(t *testing.T) {
	cfg := mainAgentConfig()
	cfg.MainAgent = "helper"
	cfg.AssistantTools = assistant.Config{Enabled: true, MaxRounds: 3, MaxCalls: 5}
	root := &llm.Client{Model: "root-model"}
	tools := &assistant.Tools{}
	built, err := buildMainAnswerer(cfg, tools, nil, root, root)
	if err != nil {
		t.Fatal(err)
	}
	if built.Role != "helper" || built.Provider != "main" || built.Model != "role-model" {
		t.Fatalf("resolved %+v", built)
	}
	if built.Prompt != "Ты — хелпер." {
		t.Fatalf("prompt=%q", built.Prompt)
	}
	client, ok := built.LLM.(*llm.Client)
	if !ok || client == root {
		t.Fatalf("the classifier client is not the role's own: %T", built.LLM)
	}
	// The assistant tools belong to the deployment, so the role's client is
	// wrapped with them: same toolset, different model.
	wrapped, ok := built.Answerer.(*assistant.Agent)
	if !ok {
		t.Fatalf("answerer=%T, want the tool-carrying assistant", built.Answerer)
	}
	if wrapped.Client != assistant.Chatter(client) {
		t.Fatal("the wrapper does not use the role client")
	}
	if wrapped.Tools != assistant.Toolset(tools) {
		t.Fatal("the wrapper replaced the deployment toolset")
	}
}

// TestBuildMainAnswererNeverBringsRoleTools: route llm catches every phrase the
// rules did not, so a sysadmin role must contribute its model and prompt only.
func TestBuildMainAnswererNeverBringsRoleTools(t *testing.T) {
	cfg := mainAgentConfig()
	cfg.MainAgent = "rooted"
	root := &llm.Client{Model: "root-model"}
	built, err := buildMainAnswerer(cfg, nil, nil, root, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := built.Answerer.(*llm.Client); !ok {
		t.Fatalf("answerer=%T, want a plain client without a toolset", built.Answerer)
	}
	if built.Model != "root-model" {
		t.Fatalf("model=%q, the role inherits llm_*", built.Model)
	}
}

func TestBuildMainAnswererRejectsUnknownRole(t *testing.T) {
	cfg := mainAgentConfig()
	cfg.MainAgent = "ghost"
	if _, err := buildMainAnswerer(cfg, nil, nil, &llm.Client{}, &llm.Client{}); err == nil {
		t.Fatal("a missing role was resolved")
	}
}

// TestMainAgentSettingsRoundTrip walks the panel write path: the file gets
// main_agent, the live engine gets the new answerer, and the snapshot describes
// the resolved target without a credential.
func TestMainAgentSettingsRoundTrip(t *testing.T) {
	path := configFile(t, `{
		"llm_model":"root-model",
		"llm_providers":[{"name":"main","api":"chat_completions","base_url":"https://llm.example.invalid/v1","model":"role-model"}],
		"agents":[{"name":"helper","provider":"main","system_prompt":"Ты — хелпер."}]
	}`)
	deps, _, applied := settingsTestDeps(t)
	value := "helper"
	status, payload := applySettingsPatch(path, deps, settingsPatch{MainAgent: &value})
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatalf("written configuration is not readable: %v", err)
	}
	if cfg.MainAgent != "helper" {
		t.Fatalf("main_agent=%q in the file", cfg.MainAgent)
	}
	if len(*applied) == 0 || (*applied)[len(*applied)-1].MainAgent != "helper" {
		t.Fatal("the live engine was not given the new configuration")
	}
	snapshot, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload=%T", payload)
	}
	if snapshot["main_agent"] != "helper" {
		t.Fatalf("snapshot main_agent=%v", snapshot["main_agent"])
	}
	target, ok := snapshot["main_target"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot main_target=%T", snapshot["main_target"])
	}
	if target["role"] != "helper" || target["model"] != "role-model" || target["provider"] != "main" {
		t.Fatalf("main_target=%v", target)
	}
	if _, ok := target["key_set"].(bool); !ok {
		t.Fatalf("main_target.key_set must be a boolean, got %T", target["key_set"])
	}
}

// TestMainAgentClears: an empty string goes back to llm_* and is written as such.
func TestMainAgentClears(t *testing.T) {
	path := configFile(t, `{"llm_model":"root-model","main_agent":"helper","agents":[{"name":"helper","system_prompt":"x"}]}`)
	deps, _, _ := settingsTestDeps(t)
	empty := ""
	if status, payload := applySettingsPatch(path, deps, settingsPatch{MainAgent: &empty}); status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MainAgent != "" {
		t.Fatalf("main_agent=%q after clearing", cfg.MainAgent)
	}
}

func TestMainAgentUnknownRoleIsRejected(t *testing.T) {
	path := configFile(t, `{"llm_model":"root-model","agents":[{"name":"helper","system_prompt":"x"}]}`)
	deps, _, applied := settingsTestDeps(t)
	ghost := "ghost"
	status, payload := applySettingsPatch(path, deps, settingsPatch{MainAgent: &ghost})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	if len(*applied) != 0 {
		t.Fatal("a rejected main_agent reached the live engine")
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MainAgent != "" {
		t.Fatalf("the rejected value was written: %q", cfg.MainAgent)
	}
}

// TestMainTargetViewNeverCarriesTheKey: the panel learns that a credential is
// set, never the credential.
func TestMainTargetViewNeverCarriesTheKey(t *testing.T) {
	t.Setenv("QUASAR_LLM_API_KEY", "fixture-key-value")
	cfg := mainAgentConfig()
	cfg.MainAgent = "rooted"
	view := mainTargetView(cfg, nil)
	if view["key_set"] != true {
		t.Fatalf("key_set=%v, want a boolean true", view["key_set"])
	}
	for field, value := range view {
		if text, ok := value.(string); ok && strings.Contains(text, "fixture-key-value") {
			t.Fatalf("%s leaks the credential", field)
		}
	}
}

// TestSettingsWarningsOnProviderModelMismatch: an explicit role model that its
// connection does not serve is a warning, not a silent clear and not an error.
func TestSettingsWarningsOnProviderModelMismatch(t *testing.T) {
	cfg := mainAgentConfig()
	cfg.Agents[0].Model = "another-model"
	warnings := settingsWarnings(cfg)
	if len(warnings) != 1 {
		t.Fatalf("warnings=%v", warnings)
	}
	if warnings[0]["agent"] != "helper" || warnings[0]["model"] != "another-model" || warnings[0]["provider_model"] != "role-model" {
		t.Fatalf("warning=%v", warnings[0])
	}
	cfg.Agents[0].Model = "role-model"
	if len(settingsWarnings(cfg)) != 0 {
		t.Fatal("a matching model produced a warning")
	}
}

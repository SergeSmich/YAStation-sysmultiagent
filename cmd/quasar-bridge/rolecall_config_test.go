package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"quasarbridge/internal/bridge"
)

// The spoken call is a switch with a small, strictly validated grammar: an
// upgrade must never start answering names nobody announced, and a malformed
// alias is refused instead of being trimmed.

func TestRoleCallSectionDefaultsOff(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"llm_model":"root-model","agents":[{"name":"helper","system_prompt":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RoleCall.Enabled || cfg.RoleCall.DryRun {
		t.Fatalf("role_call defaults to %+v", cfg.RoleCall)
	}
	if cfg.Agents[0].Callable {
		t.Fatal("a role is callable without being asked to be")
	}
}

func TestRoleCallSectionParses(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{
		"llm_model":"root-model",
		"role_call":{"enabled":true,"dry_run":true},
		"agents":[{"name":"helper","system_prompt":"x","callable":true,"greeting":"Я здесь."}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RoleCall.Enabled || !cfg.RoleCall.DryRun {
		t.Fatalf("role_call=%+v", cfg.RoleCall)
	}
	if !cfg.Agents[0].Callable || cfg.Agents[0].Greeting != "Я здесь." {
		t.Fatalf("agent=%+v", cfg.Agents[0])
	}
}

func TestRoleCallSectionRejectsUnknownMember(t *testing.T) {
	path := configFile(t, `{"llm_model":"root-model","role_call":{"enabled":true,"mode":"x"}}`)
	if _, err := readConfig(path); err == nil {
		t.Fatal("an unknown member inside role_call was accepted")
	}
}

func TestAgentGreetingValidation(t *testing.T) {
	cases := []struct {
		name     string
		greeting string
		wantErr  bool
	}{
		{name: "short line", greeting: "Я здесь. Слушаю."},
		{name: "empty", greeting: ""},
		{name: "too long", greeting: strings.Repeat("я", 301), wantErr: true},
		{name: "link", greeting: "Смотри https://example.invalid", wantErr: true},
		{name: "control character", greeting: "Я здесь\nСлушаю", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// %q escapes the newline exactly as JSON expects it.
			path := configFile(t, `{"llm_model":"root-model","agents":[{"name":"helper","system_prompt":"x","greeting":`+
				fmt.Sprintf("%q", tc.greeting)+`}]}`)
			_, err := readConfig(path)
			if tc.wantErr && err == nil {
				t.Fatal("the greeting was accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("the greeting was rejected: %v", err)
			}
		})
	}
}

func TestAgentAliasValidation(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		match string
	}{
		{
			name:  "too many aliases",
			body:  `[{"name":"helper","system_prompt":"x","aliases":["a","b","c","d","e"]}]`,
			match: "no more than",
		},
		{
			name:  "invalid alias alphabet",
			body:  `[{"name":"helper","system_prompt":"x","aliases":["Помощник"]}]`,
			match: "each alias",
		},
		{
			name:  "alias repeats the name",
			body:  `[{"name":"helper","system_prompt":"x","aliases":["helper"]}]`,
			match: "repeats the role name",
		},
		{
			name:  "two roles share a spoken name",
			body:  `[{"name":"helper","system_prompt":"x","aliases":["urka"]},{"name":"urka","system_prompt":"y"}]`,
			match: "already used",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, `{"llm_model":"root-model","agents":`+tc.body+`}`)
			_, err := readConfig(path)
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("err=%v, want %q", err, tc.match)
			}
		})
	}
}

// TestSettingsRoleCallRoundTrip walks the panel write path of the switch: the
// section is written, pushed into the live configuration and returned in the
// snapshot, and the roles are left alone.
func TestSettingsRoleCallRoundTrip(t *testing.T) {
	path := configFile(t, `{"llm_model":"root-model","agents":[{"name":"helper","system_prompt":"x","callable":true}]}`)
	deps, _, applied := settingsTestDeps(t)
	patch := roleCallConfig{Enabled: true, DryRun: true}
	status, payload := applySettingsPatch(path, deps, settingsPatch{RoleCall: &patch})
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RoleCall.Enabled || !cfg.RoleCall.DryRun {
		t.Fatalf("role_call=%+v in the file", cfg.RoleCall)
	}
	if len(*applied) == 0 || !(*applied)[len(*applied)-1].RoleCall.Enabled {
		t.Fatal("the live configuration was not given the switch")
	}
	snapshot, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload=%T", payload)
	}
	call, ok := snapshot["role_call"].(roleCallConfig)
	if !ok || !call.Enabled || !call.DryRun {
		t.Fatalf("snapshot role_call=%v", snapshot["role_call"])
	}
	if !cfg.Agents[0].Callable {
		t.Fatal("the switch rewrote the roles")
	}
}

// TestSettingsRoleCallOffTurnsItBack: the switch is reversible without touching
// the role list.
func TestSettingsRoleCallOffTurnsItBack(t *testing.T) {
	path := configFile(t, `{"llm_model":"root-model","role_call":{"enabled":true},"agents":[{"name":"helper","system_prompt":"x","callable":true}]}`)
	deps, _, _ := settingsTestDeps(t)
	patch := roleCallConfig{}
	if status, payload := applySettingsPatch(path, deps, settingsPatch{RoleCall: &patch}); status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RoleCall.Enabled {
		t.Fatal("role_call is still enabled")
	}
	if !cfg.Agents[0].Callable {
		t.Fatal("the roles were reset with the switch")
	}
}

// TestSettingsAgentSpokenCallFields: the three fields are part of the role, so a
// card save carries them and the snapshot returns them.
func TestSettingsAgentSpokenCallFields(t *testing.T) {
	path := configFile(t, `{"llm_model":"root-model","agents":[{"name":"helper","system_prompt":"x"}]}`)
	deps, _, _ := settingsTestDeps(t)
	agents := []agentConfig{{
		Name: "helper", SystemPrompt: "x", Callable: true,
		Aliases: []string{"помощник"}, Greeting: "Я здесь.",
	}}
	status, payload := applySettingsPatch(path, deps, settingsPatch{Agents: &agents})
	if status != http.StatusOK {
		t.Fatalf("status=%d payload=%v", status, payload)
	}
	cfg, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Agents[0].Callable || cfg.Agents[0].Greeting != "Я здесь." || len(cfg.Agents[0].Aliases) != 1 {
		t.Fatalf("agent=%+v", cfg.Agents[0])
	}
	snapshot, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload=%T", payload)
	}
	saved, ok := snapshot["agents"].([]agentConfig)
	if !ok || len(saved) != 1 || !saved[0].Callable || saved[0].Aliases[0] != "помощник" {
		t.Fatalf("snapshot agents=%v", snapshot["agents"])
	}
}

// TestRoleCallThroughConfig is the whole path the station runs: validation, the
// agent registry, the directory and the decision. A bare call must answer with
// the configured greeting and never reach a model.
func TestRoleCallThroughConfig(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{
		"llm_model":"root-model",
		"role_call":{"enabled":true},
		"agents":[{"name":"helper","system_prompt":"Ты — хелпер.","callable":true,"greeting":"Я здесь.","aliases":["помощник"]}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	agents, err := buildAgents(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine := &bridge.Engine{}
	engine.SetRoleCall(bridge.RoleCallConfig{Enabled: cfg.RoleCall.Enabled, DryRun: cfg.RoleCall.DryRun})
	if err := engine.SetAgents(agents); err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"Алиса, позови helper", "helper, ты здесь?", "позови помощника"} {
		decision := engine.Decide(context.Background(), phrase)
		if decision.Source != "role_call" || decision.Route != "agent:helper" {
			t.Fatalf("%q: decision=%+v", phrase, decision)
		}
		if decision.Answer != "Я здесь." {
			t.Fatalf("%q: answer=%q", phrase, decision.Answer)
		}
	}
	// A name inside a sentence is not a call, whatever the switch says.
	if decision := engine.Decide(context.Background(), "включи свет, helper"); decision.Source == "role_call" {
		t.Fatalf("a phrase with the name in the middle resolved: %+v", decision)
	}
}

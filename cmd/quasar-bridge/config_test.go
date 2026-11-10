package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/wire"
)

func TestReadConfigLLMAPISettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		api    string
		effort string
		tokens int
		tools  int
	}{
		{"defaults", `{}`, "chat_completions", "", 4096, 0},
		{"responses", `{"llm_api":"responses","llm_reasoning_effort":"low","llm_max_output_tokens":8000,"llm_hosted_tools":["web_search","code_interpreter"]}`, "responses", "low", 8000, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := readConfig(configFile(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LLMAPI != tc.api || cfg.LLMReasoningEffort != tc.effort || cfg.LLMMaxOutputTokens != tc.tokens || len(cfg.LLMHostedTools) != tc.tools {
				t.Fatalf("LLM settings: %+v", cfg)
			}
		})
	}
}

func TestDeviceContextConfig(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceContext.Enabled || cfg.DeviceContext.Timezone != "Europe/Moscow" || cfg.DeviceContext.wire() != (wire.DeviceContext{}) {
		t.Fatalf("defaults: %+v", cfg.DeviceContext)
	}
	cfg, err = readConfig(configFile(t, `{"device_context":{"enabled":true,"lat":55.7558,"lon":37.6173,"accuracy":1000,"timezone":"Europe/Moscow"}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.DeviceContext.wire()
	if got.Timezone != "Europe/Moscow" || got.Lat != 55.7558 || got.Accuracy != 1000 {
		t.Fatalf("wire context: %+v", got)
	}
	for _, body := range []string{
		`{"device_context":{"enabled":true,"lat":91,"lon":37.6173}}`,
		`{"device_context":{"enabled":true,"lat":55.7558,"lon":181}}`,
		`{"device_context":{"enabled":true,"lat":0,"lon":0}}`,
		`{"device_context":{"enabled":true,"lat":55.7558,"lon":37.6173,"accuracy":-1}}`,
		`{"device_context":{"enabled":true,"lat":55.7558,"lon":37.6173,"timezone":"Europe/Moscow 1"}}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestReadConfigLLMAPIEnvironmentOverrides(t *testing.T) {
	t.Setenv("QUASAR_LLM_API", "responses")
	t.Setenv("QUASAR_LLM_REASONING_EFFORT", "low")
	cfg, err := readConfig(configFile(t, `{"llm_api":"chat_completions"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMAPI != "responses" || cfg.LLMReasoningEffort != "low" {
		t.Fatalf("env overrides: %+v", cfg)
	}
	t.Setenv("QUASAR_LLM_API", "")
	t.Setenv("QUASAR_LLM_REASONING_EFFORT", "")
	if _, err := readConfig(configFile(t, `{}`)); err != nil {
		t.Fatalf("empty API override should use default: %v", err)
	}
}

func TestReadConfigRejectsInvalidLLMAPISettings(t *testing.T) {
	for _, body := range []string{
		`{"llm_api":"completions"}`,
		`{"llm_api":"responses","llm_reasoning_effort":"fast"}`,
		`{"llm_reasoning_effort":"low"}`,
		`{"llm_api":"responses","llm_max_output_tokens":0}`,
		`{"llm_api":"responses","llm_max_output_tokens":-1}`,
		`{"llm_api":"responses","llm_hosted_tools":["unknown"]}`,
		`{"llm_api":"responses","llm_hosted_tools":["web_search","web_search"]}`,
		`{"llm_hosted_tools":["web_search"]}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := readConfig(configFile(t, body)); err == nil {
				t.Fatal("invalid LLM settings accepted")
			}
		})
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if _, err := readConfig(configFile(t, `{"llm_api":"responses","llm_reasoning_effort":"`+effort+`"}`)); err != nil {
			t.Fatalf("effort %q rejected: %v", effort, err)
		}
	}
}

func configFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProviderNamesAreCaseInsensitive: the panel folds what the operator types,
// but a hand-edited file may still say "DeepSeek" twice in two spellings. One
// canonical name has to reach config.json, secrets.json and the role that points
// at the provider, so both sides are normalised and the lookup ignores case.
func TestProviderNamesAreCaseInsensitive(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{
		"mode":"observe",
		"llm_providers":[{"name":"  DeepSeek ","api":"chat_completions",
			"base_url":"https://api.deepseek.com/v1","model":"deepseek-chat"}],
		"agents":[{"name":"briefing","provider":"DEEPSEEK","system_prompt":"role"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.LLMProviders[0].Name; got != "deepseek" {
		t.Fatalf("provider name = %q, want deepseek", got)
	}
	if got := cfg.Agents[0].Provider; got != "deepseek" {
		t.Fatalf("role provider = %q, want deepseek", got)
	}
	target, err := cfg.resolveLLM(cfg.Agents[0])
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider != "deepseek" || target.Model != "deepseek-chat" ||
		target.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("resolved target = %+v", target)
	}
	// Two spellings of one endpoint are one provider, not two credential keys.
	if _, err := readConfig(configFile(t, `{
		"mode":"observe",
		"llm_providers":[
			{"name":"deepseek","base_url":"https://api.deepseek.com/v1","model":"a"},
			{"name":"DeepSeek","base_url":"https://api.deepseek.com/v1","model":"b"}
		]
	}`)); err == nil {
		t.Fatal("two spellings of one provider name accepted")
	}
}

// TestValidateAgentsDropsStaleAddressOfLinkedRole: an earlier build baked the
// address and the wire format into every role. A role that names a connection
// must not keep such a pair: resolveLLM ignores it, and the write path drops it
// so config.json stops carrying a value the panel does not show.
func TestValidateAgentsDropsStaleAddressOfLinkedRole(t *testing.T) {
	cfg := config{
		LLMModel: "root-model",
		LLMProviders: []llmProviderConfig{{
			Name: "groq", API: "chat_completions", BaseURL: "https://api.groq.com/openai/v1",
			Model: "openai/gpt-oss-20b", MaxOutputTokens: 512,
		}},
		Agents: []agentConfig{{
			Name: "helper", Provider: "groq", SystemPrompt: "роль",
			API: "responses", BaseURL: "https://api.deepseek.com/v1",
			Model: "own-model", MaxOutputTokens: 256,
		}},
	}
	if err := cfg.validateProviders(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateAgents(); err != nil {
		t.Fatal(err)
	}
	role := cfg.Agents[0]
	if role.API != "" || role.BaseURL != "" {
		t.Fatalf("a linked role kept %q %q", role.API, role.BaseURL)
	}
	// The model is still the role's own choice.
	if role.Model != "own-model" {
		t.Fatalf("role model = %q", role.Model)
	}
	// The panel probe of the main-model card takes the published address and the
	// saved key together, so they have to be the connection's pair.
	cfg.MainAgent = "helper"
	if view := mainTargetView(cfg, nil); view["base_url"] != "https://api.groq.com/openai/v1" {
		t.Fatalf("main target published %v", view["base_url"])
	}
}

func TestReadConfigAcceptsListenAfterReply(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{}`, true},
		{`{"listen_after_reply":true}`, true},
		{`{"listen_after_reply":false}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cfg, err := readConfig(configFile(t, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ListenAfterReply != tc.want {
				t.Fatalf("listen_after_reply=%v, want %v", cfg.ListenAfterReply, tc.want)
			}
		})
	}
}
func TestReadConfigEnvironmentOverridesDeploymentSettings(t *testing.T) {
	t.Setenv("QUASAR_MODE", "hybrid")
	t.Setenv("QUASAR_LISTEN", "localhost:9443")
	t.Setenv("QUASAR_UPSTREAM", "wss://upstream.example.invalid/uni.ws")
	t.Setenv("QUASAR_TLS_CERT", "runtime/public.pem")
	t.Setenv("QUASAR_TLS_KEY", "runtime/private.key")
	t.Setenv("QUASAR_ALLOWED_IPS", "192.0.2.10, 2001:db8::10")
	t.Setenv("QUASAR_LLM_BASE_URL", "https://llm.example.invalid/v1")
	t.Setenv("QUASAR_LLM_MODEL", "configured-model")
	t.Setenv("QUASAR_HOME_MCP_URL", "https://home.example.invalid/mcp")
	t.Setenv("QUASAR_HOME_WARM_AREA", "Office")
	cfg, err := readConfig(configFile(t, `{"mode":"observe","llm_model":"previous"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "localhost:9443" || cfg.Upstream != "wss://upstream.example.invalid/uni.ws" || cfg.TLSCert != "runtime/public.pem" || cfg.TLSKey != "runtime/private.key" || cfg.Mode != "hybrid" || cfg.LLMModel != "configured-model" || cfg.HomeWarmArea != "Office" || len(cfg.AllowedIPs) != 2 {
		t.Fatalf("configuration override failed: %+v", cfg)
	}
}
func TestReadConfigRejectsInvalidAndUnknownSettings(t *testing.T) {
	for _, body := range []string{
		`{"unexpected":"value"}`, `{"mode":"unknown"}`, `{"tls_cert":"cert.pem"}`,
		`{"allowed_ips":["not-an-address"]}`, `{"upstream":"file:///tmp/socket"}`,
		`{"llm_base_url":"https://name:password@example.invalid/v1"}`,
		`{"mode":"hybrid"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := readConfig(configFile(t, body)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
func TestReadConfigHasGenericDefaults(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "observe" || !strings.HasPrefix(cfg.Listen, "localhost:") {
		t.Fatalf("defaults=%+v", cfg)
	}
}

func TestHybridHomeAssistantIsOptional(t *testing.T) {
	t.Setenv("QUASAR_HOME_MCP_URL", "")
	body := `{"mode":"hybrid","llm_base_url":"https://llm.example.invalid/v1","llm_model":"test"}`
	if _, err := readConfig(configFile(t, body)); err != nil {
		t.Fatalf("LLM-only hybrid rejected: %v", err)
	}
	for _, endpoint := range []string{"not-a-url", "file:///tmp/mcp", "https://user:secret@example.invalid/mcp"} {
		t.Setenv("QUASAR_HOME_MCP_URL", endpoint)
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatal("invalid configured HA endpoint accepted")
		}
	}
}

func TestRoutingDefaultsAndPartialOverlay(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"routing":{"classifier":{"on_error":"llm"},"refusal":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routing.Rules) != len(bridge.DefaultRoutingConfig().Rules) || cfg.Routing.Classifier.TimeoutMS != 8000 || cfg.Routing.Classifier.OnError != "llm" || cfg.Routing.Refusal.Enabled {
		t.Fatalf("partial defaults lost: %+v", cfg.Routing)
	}
	empty, err := readConfig(configFile(t, `{"routing":{"rules":[],"default_route":"yandex"}}`))
	if err != nil || len(empty.Routing.Rules) != 0 {
		t.Fatalf("empty rules: %v", err)
	}
	for _, body := range []string{
		`{"routing":{"default_route":"invalid"}}`,
		`{"routing":{"rules":[{"name":"bad","route":"llm","match":{"regex":"["}}]}}`,
		`{"routing":{"rules":[{"name":"bad","route":"llm","match":{"typo":["x"]}}]}}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatal("invalid routing accepted")
		}
	}
}

func TestRuleArrayReplacesDefaultElements(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"routing":{"rules":[{"name":"custom","route":"llm","match":{"contains_any":["ping"]}}],"default_route":"yandex"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routing.Rules) != 1 || len(cfg.Routing.Rules[0].Match.ContainsAllGroups) != 0 {
		t.Fatal("custom rule inherited built-in conditions")
	}
	e := &bridge.Engine{}
	if err := e.SetRouting(cfg.Routing); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(t.Context(), "PING"); got.Route != "llm" || got.Rule != "custom" {
		t.Fatal(got)
	}
}

func TestExampleRoutingEqualsBackwardCompatibleDefaults(t *testing.T) {
	t.Setenv("QUASAR_HOME_MCP_URL", "https://home.example.invalid/mcp")
	cfg, err := readConfig("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Routing, bridge.DefaultRoutingConfig()) {
		t.Fatal("example defaults drifted from legacy policy")
	}
}

func TestLocalProgressConfiguration(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"mode":"observe"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LocalProgress.Enabled || cfg.LocalProgress.AfterMS != 2000 || cfg.LocalProgress.IntervalMS != 4000 {
		t.Fatal("unexpected progress defaults")
	}
	for _, body := range []string{`{"mode":"observe","local_progress":{"enabled":true,"after_ms":99}}`, `{"mode":"observe","local_progress":{"enabled":true,"interval_ms":999}}`, `{"mode":"observe","local_progress":{"enabled":true,"after_ms":10001}}`} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatal("invalid progress timing accepted")
		}
	}
}

func TestUpstreamWatchdogConfiguration(t *testing.T) {
	for _, tc := range []struct {
		body       string
		firstFrame int
		interval   int
		timeout    int
	}{
		{`{"mode":"observe"}`, 0, 0, 0},
		{`{"mode":"observe","first_frame_ms":8000,"upstream_ping":{}}`, 8000, 0, 0},
		{`{"mode":"observe","first_frame_ms":5000,"upstream_ping":{"interval_ms":30000,"timeout_ms":10000}}`, 5000, 30000, 10000},
	} {
		cfg, err := readConfig(configFile(t, tc.body))
		if err != nil {
			t.Fatalf("configuration rejected: %v", err)
		}
		if cfg.FirstFrameMS != tc.firstFrame || cfg.UpstreamPing.IntervalMS != tc.interval || cfg.UpstreamPing.TimeoutMS != tc.timeout {
			t.Fatalf("unexpected upstream watchdog settings: %+v", cfg)
		}
	}
	for _, body := range []string{
		`{"mode":"observe","first_frame_ms":-1}`,
		`{"mode":"observe","first_frame_ms":60001}`,
		`{"mode":"observe","upstream_ping":{"interval_ms":999,"timeout_ms":100}}`,
		`{"mode":"observe","upstream_ping":{"interval_ms":1000,"timeout_ms":99}}`,
		`{"mode":"observe","upstream_ping":{"interval_ms":1000,"timeout_ms":1000}}`,
		`{"mode":"observe","upstream_ping":{"timeout_ms":100}}`,
	} {
		if _, err := readConfig(configFile(t, body)); err == nil {
			t.Fatalf("invalid watchdog configuration accepted: %s", body)
		}
	}
}

func TestDialogueContextDefaultsAndValidation(t *testing.T) {
	cfg, err := readConfig(configFile(t, `{"mode":"observe"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DialogueContext.Enabled || cfg.DialogueContext.TTLSeconds != 300 || cfg.DialogueContext.MaxTurns != 6 {
		t.Fatal("wrong dialogue defaults")
	}
	if _, err := readConfig(configFile(t, `{"mode":"observe","dialogue_context":{"max_turns":0}}`)); err == nil {
		t.Fatal("invalid dialogue limit accepted")
	}
}

func TestMCPExampleAndUnknownConfiguration(t *testing.T) {
	t.Setenv("QUASAR_HOME_MCP_URL", "")
	cfg, err := readConfig("../../config.mcp.example.json")
	if err != nil || len(cfg.AssistantTools.MCPServers) != 1 || cfg.AssistantTools.MCPServers[0].Name != "parallel" {
		t.Fatal("invalid MCP example", err)
	}
	if got := (&bridge.Engine{}); got.SetRouting(cfg.Routing) != nil || got.Decide(t.Context(), "найди в интернете документацию").Route != "llm" {
		t.Fatal("MCP search phrase did not reach LLM")
	}
	if _, err := readConfig(configFile(t, `{"assistant_tools":{"enabled":true,"mcp_servers":[{"name":"x","url":"https://mcp.example.invalid","typo":true}]}}`)); err == nil {
		t.Fatal("unknown MCP option accepted")
	}
}

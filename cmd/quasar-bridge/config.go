package main

import (
	"encoding/json/v2"
	"errors"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

type progressConfig struct {
	Enabled    bool `json:"enabled"`
	AfterMS    int  `json:"after_ms"`
	IntervalMS int  `json:"interval_ms"`
}

type upstreamPingConfig struct {
	IntervalMS int `json:"interval_ms"`
	TimeoutMS  int `json:"timeout_ms"`
}

// deviceContextConfig is the place the bridge reports on behalf of the station.
// Local patch (not upstream): the station ignores the set_location and
// set_timezone directives, so local skills receive requests without a place.
// Disabled by default; the values are the Moscow defaults the station needs.
type deviceContextConfig struct {
	Enabled  bool    `json:"enabled"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Accuracy float64 `json:"accuracy"`
	Timezone string  `json:"timezone"`
}

func defaultDeviceContext() deviceContextConfig {
	return deviceContextConfig{Lat: 55.7558, Lon: 37.6173, Accuracy: 1000, Timezone: "Europe/Moscow"}
}

func (c deviceContextConfig) wire() wire.DeviceContext {
	if !c.Enabled {
		return wire.DeviceContext{}
	}
	return wire.DeviceContext{Lat: c.Lat, Lon: c.Lon, Accuracy: c.Accuracy, Timezone: c.Timezone}
}

type config struct {
	DialogueContext    dialogue.Config      `json:"dialogue_context"`
	LocalProgress      progressConfig       `json:"local_progress"`
	AssistantTools     assistant.Config     `json:"assistant_tools"`
	ConversationLog    journal.Config       `json:"conversation_log"`
	DeviceContext      deviceContextConfig  `json:"device_context"`
	Listen             string               `json:"listen"`
	Upstream           string               `json:"upstream"`
	TLSCert            string               `json:"tls_cert"`
	TLSKey             string               `json:"tls_key"`
	AllowedIPs         []string             `json:"allowed_ips"`
	Mode               string               `json:"mode"`
	LLMBaseURL         string               `json:"llm_base_url"`
	LLMModel           string               `json:"llm_model"`
	LLMAPI             string               `json:"llm_api"`
	LLMReasoningEffort string               `json:"llm_reasoning_effort"`
	LLMMaxOutputTokens int                  `json:"llm_max_output_tokens"`
	LLMHostedTools     []string             `json:"llm_hosted_tools"`
	LLMChatParameters  map[string]any       `json:"llm_chat_parameters"`
	HomeWarmArea       string               `json:"home_warm_area"`
	ListenAfterReply   bool                 `json:"listen_after_reply"`
	Routing            bridge.RoutingConfig `json:"routing"`
	LocalVoice         bridge.VoiceConfig   `json:"local_voice"`
	FirstFrameMS       int                  `json:"first_frame_ms"`
	UpstreamPing       upstreamPingConfig   `json:"upstream_ping"`
	Panel              panelConfig          `json:"panel"`
}

func readConfig(path string) (config, error) {
	cfg := config{DialogueContext: dialogue.DefaultConfig(), Routing: bridge.DefaultRoutingConfig(), ConversationLog: journal.DefaultConfig(), DeviceContext: defaultDeviceContext(), AssistantTools: assistant.DefaultConfig(), LocalProgress: progressConfig{AfterMS: 2000, IntervalMS: 4000}, LLMAPI: "chat_completions", LLMMaxOutputTokens: 4096, ListenAfterReply: true, Panel: defaultPanelConfig()}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, errors.New("cannot read configuration file")
	}
	if json.Unmarshal(raw, &cfg, json.RejectUnknownMembers(true)) != nil {
		return cfg, errors.New("invalid configuration JSON")
	}
	for name, target := range map[string]*string{
		"QUASAR_LISTEN": &cfg.Listen, "QUASAR_UPSTREAM": &cfg.Upstream,
		"QUASAR_TLS_CERT": &cfg.TLSCert, "QUASAR_TLS_KEY": &cfg.TLSKey,
		"QUASAR_MODE": &cfg.Mode, "QUASAR_LLM_BASE_URL": &cfg.LLMBaseURL,
		"QUASAR_LLM_MODEL": &cfg.LLMModel, "QUASAR_HOME_WARM_AREA": &cfg.HomeWarmArea,
		"QUASAR_LLM_API": &cfg.LLMAPI, "QUASAR_LLM_REASONING_EFFORT": &cfg.LLMReasoningEffort,
	} {
		if value, ok := os.LookupEnv(name); ok {
			*target = value
		}
	}
	if value, ok := os.LookupEnv("QUASAR_ALLOWED_IPS"); ok {
		cfg.AllowedIPs = nil
		for item := range strings.SplitSeq(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				cfg.AllowedIPs = append(cfg.AllowedIPs, item)
			}
		}
	}
	if len(cfg.AllowedIPs) == 0 {
		cfg.AllowedIPs = nil
	}
	// json/v2 turns an absent slice into an empty one; the reload comparison
	// must see the same value for a file that was not edited.
	if len(cfg.Panel.Hosts) == 0 {
		cfg.Panel.Hosts = nil
	}
	if cfg.Listen == "" {
		cfg.Listen = "localhost:8443"
	}
	if cfg.Upstream == "" {
		cfg.Upstream = "wss://uniproxy.alice.yandex.net/uni.ws"
	}
	if cfg.Mode == "" {
		cfg.Mode = "observe"
	}
	if cfg.LLMAPI == "" {
		cfg.LLMAPI = "chat_completions"
	}
	if cfg.LLMAPI != "chat_completions" && cfg.LLMAPI != "responses" {
		return cfg, errors.New("llm_api must be chat_completions or responses")
	}
	if len(cfg.LLMChatParameters) == 0 {
		cfg.LLMChatParameters = nil
	} else {
		if cfg.LLMAPI != "chat_completions" {
			return cfg, errors.New("llm_chat_parameters requires llm_api chat_completions")
		}
		if err := llm.ValidateChatParameters(cfg.LLMChatParameters); err != nil {
			return cfg, err
		}
	}
	// encoding/json/v2 round-trips an absent map as an empty one; normalize it
	// so a configuration reload sees identical values.
	if len(cfg.LocalVoice.Routes) == 0 {
		cfg.LocalVoice.Routes = nil
	}
	if len(cfg.LocalVoice.Candidates) == 0 {
		cfg.LocalVoice.Candidates = nil
	}
	if cfg.LLMMaxOutputTokens <= 0 {
		return cfg, errors.New("llm_max_output_tokens must be positive")
	}
	if cfg.LLMReasoningEffort != "" {
		if cfg.LLMAPI != "responses" {
			return cfg, errors.New("llm_reasoning_effort requires llm_api responses")
		}
		if !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, cfg.LLMReasoningEffort) {
			return cfg, errors.New("invalid llm_reasoning_effort")
		}
	}
	if len(cfg.LLMHostedTools) == 0 {
		cfg.LLMHostedTools = nil
	} else {
		if cfg.LLMAPI != "responses" {
			return cfg, errors.New("llm_hosted_tools requires llm_api responses")
		}
		seen := make(map[string]bool, len(cfg.LLMHostedTools))
		for _, name := range cfg.LLMHostedTools {
			if name != "web_search" && name != "code_interpreter" {
				return cfg, errors.New("llm_hosted_tools supports web_search and code_interpreter")
			}
			if seen[name] {
				return cfg, errors.New("duplicate llm_hosted_tools entry")
			}
			seen[name] = true
		}
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return cfg, errors.New("listen must include host and port")
	}
	if cfg.Mode != "observe" && cfg.Mode != "hybrid" {
		return cfg, errors.New("mode must be observe or hybrid")
	}
	if !validEndpoint(cfg.Upstream, "ws", "wss") {
		return cfg, errors.New("invalid upstream URL")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return cfg, errors.New("TLS certificate and key must be set together")
	}
	for _, ip := range cfg.AllowedIPs {
		if _, err := netip.ParseAddr(ip); err != nil {
			return cfg, errors.New("allowed_ips must contain IP addresses")
		}
	}
	if cfg.LLMBaseURL != "" && !validEndpoint(cfg.LLMBaseURL, "http", "https") {
		return cfg, errors.New("invalid LLM URL")
	}
	if cfg.Mode == "hybrid" {
		if cfg.LLMBaseURL == "" || cfg.LLMModel == "" {
			return cfg, errors.New("hybrid mode requires LLM URL and model")
		}
		if endpoint := os.Getenv("QUASAR_HOME_MCP_URL"); endpoint != "" && !validEndpoint(endpoint, "http", "https") {
			return cfg, errors.New("invalid QUASAR_HOME_MCP_URL")
		}
	}
	if cfg.LocalProgress.AfterMS < 100 || cfg.LocalProgress.AfterMS > 10000 || cfg.LocalProgress.IntervalMS < 1000 || cfg.LocalProgress.IntervalMS > 10000 {
		return cfg, errors.New("invalid local_progress timing")
	}
	if err := cfg.DialogueContext.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.AssistantTools.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.ConversationLog.Validate(); err != nil {
		return cfg, err
	}
	if _, err := bridge.CompileRouting(cfg.Routing); err != nil {
		return cfg, err
	}
	if err := cfg.LocalVoice.Validate(); err != nil {
		return cfg, err
	}
	if c := cfg.DeviceContext; c.Enabled {
		if math.Abs(c.Lat) > 90 || math.Abs(c.Lon) > 180 {
			return cfg, errors.New("device_context lat/lon out of range")
		}
		if c.Lat == 0 && c.Lon == 0 {
			return cfg, errors.New("device_context requires lat and lon")
		}
		if c.Accuracy < 0 || c.Accuracy > 100000 {
			return cfg, errors.New("device_context accuracy must be 0..100000")
		}
		if c.Timezone != "" && !validTimezone(c.Timezone) {
			return cfg, errors.New("invalid device_context timezone")
		}
	}
	if cfg.FirstFrameMS < 0 || cfg.FirstFrameMS > 60000 {
		return cfg, errors.New("first_frame_ms must be 0..60000")
	}
	if cfg.UpstreamPing.IntervalMS != 0 || cfg.UpstreamPing.TimeoutMS != 0 {
		if cfg.UpstreamPing.IntervalMS < 1000 || cfg.UpstreamPing.IntervalMS > 60000 {
			return cfg, errors.New("upstream_ping.interval_ms must be 1000..60000")
		}
		if cfg.UpstreamPing.TimeoutMS < 100 || cfg.UpstreamPing.TimeoutMS >= cfg.UpstreamPing.IntervalMS {
			return cfg, errors.New("upstream_ping.timeout_ms must be 100..interval_ms")
		}
	}
	if err := cfg.Panel.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func validEndpoint(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, scheme := range schemes {
		if u.Scheme == scheme {
			return true
		}
	}
	return false
}

// validTimezone accepts an IANA name such as "Europe/Moscow". The station has
// no tzdata, and the cloud expects the raw identifier, so the value is only
// checked for shape: spaces and control characters would corrupt the frame or
// the log.
func validTimezone(name string) bool {
	if len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '/', r == '_', r == '+', r == '-':
		default:
			return false
		}
	}
	return true
}

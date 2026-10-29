package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
)

func TestHealthzReportsUpstreamCounters(t *testing.T) {
	proxy := new(bridge.Proxy)
	proxy.RecordUpstreamStall()
	proxy.RecordUpstreamFrame()
	voice := new(voiceState)
	voice.Store(bridge.VoiceConfig{Voice: "omazh", Volume: 1.2})
	handler := healthzHandler(config{Mode: "observe"}, new(bridge.Engine), new(journal.Writer), nil, 0, nil, proxy, voice, new(wakeState), new(atomic.Int64), new(atomic.Int64))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if response.Code != 200 {
		t.Fatalf("status=%d", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["upstream_stalls_total"] != float64(1) || payload["upstream_ping_failures_total"] != float64(0) || payload["upstream_last_frame_ms"].(float64) <= 0 {
		t.Fatalf("upstream health fields: %v", payload)
	}
	voicePayload, ok := payload["local_voice"].(map[string]any)
	if !ok || voicePayload["voice"] != "omazh" || voicePayload["volume"] != 1.2 || payload["local_voice_revision"] != float64(1) {
		t.Fatalf("local voice fields: %v", payload)
	}
}

// TestHealthzCountsProviders: /healthz publishes how many endpoints are
// configured, never their names or credentials.
func TestHealthzCountsProviders(t *testing.T) {
	cfg := config{Mode: "observe", LLMProviders: []llmProviderConfig{
		{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
		{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Model: "openrouter/auto"},
	}}
	handler := healthzHandler(cfg, new(bridge.Engine), new(journal.Writer), nil, 0, nil, new(bridge.Proxy), new(voiceState), new(wakeState), new(atomic.Int64), new(atomic.Int64))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if response.Code != 200 {
		t.Fatalf("status=%d", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["llm_providers"] != float64(2) {
		t.Fatalf("llm_providers = %v", payload["llm_providers"])
	}
	if strings.Contains(response.Body.String(), "deepseek") {
		t.Fatalf("a provider name reached /healthz: %s", response.Body.String())
	}
}

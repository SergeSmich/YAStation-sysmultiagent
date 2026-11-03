package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

func TestExplainLLMErrorShowsServiceMessage(t *testing.T) {
	serve := func(status int, body string) error {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		defer s.Close()
		_, err := (&llm.Client{BaseURL: s.URL}).Complete(t.Context(), nil, false)
		return err
	}
	got := explainLLMError(serve(400, `{"error":{"message":"model does not support chat"}}`))
	if !strings.Contains(got, "model does not support chat") || strings.Contains(got, "Google") {
		t.Fatalf("400: %q", got)
	}
	got = explainLLMError(serve(403, `{"error":{"message":"This request requires the interactive Claude Code CLI, OpenCode, or Codex"}}`))
	if !strings.Contains(got, "только в свои программы") {
		t.Fatalf("403 restricted: %q", got)
	}
	if got := explainLLMError(serve(429, "")); strings.Contains(got, "Сервис пишет") {
		t.Fatalf("empty detail still shown: %q", got)
	}
}

func TestModelListHidesNonChatModels(t *testing.T) {
	got, err := parseModelList([]byte(`{"data":[{"id":"openai/gpt-oss-safeguard-20b"},{"id":"whisper-large-v3"},{"id":"openai/gpt-oss-120b"},{"id":"openai/gpt-oss-20b"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "openai/gpt-oss-120b,openai/gpt-oss-20b" {
		t.Fatalf("models=%v", got)
	}
	only, err := parseModelList([]byte(`{"data":[{"id":"whisper-large-v3"}]}`))
	if err != nil || len(only) != 1 {
		t.Fatalf("service-only list: %v %v", only, err)
	}
}

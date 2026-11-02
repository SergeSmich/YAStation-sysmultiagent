package main

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
)

type answerFunc func(context.Context, []llm.Message, bool) (string, error)

func (f answerFunc) Complete(ctx context.Context, m []llm.Message, s bool) (string, error) {
	return f(ctx, m, s)
}

type fakeBase struct{}

func (fakeBase) Definitions() []llm.Tool {
	return []llm.Tool{{Type: "function", Function: llm.Function{Name: "current_time"}}}
}
func (fakeBase) Call(context.Context, string, string) (any, error) { return "base", nil }

func newPeerFixture(t *testing.T, captured *[]llm.Message) (*peerToolset, *peerHub) {
	t.Helper()
	hub := &peerHub{}
	peer := &bridge.Agent{Name: "юля", Description: "погода", Prompt: "Ты Юля.", Answerer: answerFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
		*captured = append([]llm.Message(nil), m...)
		return " дождь ", nil
	})}
	self := &bridge.Agent{Name: "захар", Answerer: answerFunc(func(context.Context, []llm.Message, bool) (string, error) { return "", nil })}
	agents := map[string]*bridge.Agent{"юля": peer, "захар": self}
	hub.agents.Store(&agents)
	return &peerToolset{base: fakeBase{}, self: "захар", hub: hub}, hub
}

func TestPeerToolsetListAndDefinitions(t *testing.T) {
	var got []llm.Message
	tools, hub := newPeerFixture(t, &got)
	names := map[string]bool{}
	for _, d := range tools.Definitions() {
		names[d.Function.Name] = true
	}
	for _, want := range []string{"current_time", toolListAgents, toolAskAgent, toolHandoff} {
		if !names[want] {
			t.Fatalf("missing tool %s: %v", want, names)
		}
	}
	out, err := tools.Call(t.Context(), toolListAgents, "{}")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "юля") || strings.Contains(string(raw), `"name":"захар"`) {
		t.Fatalf("list: %s", raw)
	}
	if res, err := tools.Call(t.Context(), "current_time", "{}"); err != nil || res != "base" {
		t.Fatalf("base call: %v %v", res, err)
	}
	// With only one role there is nobody to talk to: the peer tools disappear.
	alone := map[string]*bridge.Agent{"захар": {Name: "захар"}}
	hub.agents.Store(&alone)
	if n := len(tools.Definitions()); n != 1 {
		t.Fatalf("definitions for a lone role: %d", n)
	}
}

func TestPeerToolsetAsk(t *testing.T) {
	var got []llm.Message
	tools, _ := newPeerFixture(t, &got)
	out, err := tools.Call(t.Context(), toolAskAgent, `{"agent":"юля","question":"какая погода?"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]string)
	if m["answer"] != "дождь" || m["agent"] != "юля" {
		t.Fatalf("answer: %v", m)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0].Content, "Ты Юля.") || !strings.Contains(got[0].Content, "«захар»") || got[1].Content != "какая погода?" {
		t.Fatalf("messages: %+v", got)
	}
	for _, bad := range []string{
		`{"agent":"захар","question":"я сам"}`,
		`{"agent":"нет","question":"x"}`,
		`{"agent":"юля","question":""}`,
		`{"agent":"юля","question":"x","extra":1}`,
	} {
		if _, err := tools.Call(t.Context(), toolAskAgent, bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	nested := bridge.WithPeerDepth(t.Context())
	if _, err := tools.Call(nested, toolAskAgent, `{"agent":"юля","question":"x"}`); err == nil {
		t.Fatal("nested ask allowed")
	}
}

func TestPeerToolsetHandoffNeedsDialogue(t *testing.T) {
	var got []llm.Message
	tools, _ := newPeerFixture(t, &got)
	if _, err := tools.Call(t.Context(), toolHandoff, `{"agent":"юля"}`); err != bridge.ErrNoHandoff {
		t.Fatalf("handoff outside a dialogue: %v", err)
	}
	if _, err := tools.Call(t.Context(), toolHandoff, `{"agent":"нет"}`); err == nil {
		t.Fatal("handoff to an unknown role accepted")
	}
}

func TestPeerToolsetBlockedAfterHistory(t *testing.T) {
	var got []llm.Message
	tools, _ := newPeerFixture(t, &got)
	for _, name := range []string{toolListAgents, toolAskAgent, toolHandoff, "read_webpage", "mcp__x"} {
		if tools.AllowedAfterHistory(name) {
			t.Fatalf("%s allowed after history", name)
		}
	}
	if !tools.AllowedAfterHistory("current_time") {
		t.Fatal("current_time blocked after history")
	}
	var _ assistant.Toolset = tools
}

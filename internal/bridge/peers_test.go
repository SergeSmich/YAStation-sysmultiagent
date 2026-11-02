package bridge

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

func TestRosterListsOtherRolesWithDescriptions(t *testing.T) {
	var seen string
	capture := completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		seen = messages[0].Content
		return "ok", nil
	})
	quiet := completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "ok", nil })
	e := &Engine{}
	agents := map[string]*Agent{
		"захар":  {Name: "захар", Answerer: capture, Prompt: "Ты Захар.", Peers: true},
		"юля":    {Name: "юля", Answerer: quiet, Description: "помогает с погодой"},
		"сисадм": {Name: "сисадм", Answerer: quiet},
	}
	if err := e.SetAgents(agents); err != nil {
		t.Fatal(err)
	}
	if _, err := e.answerAs(t.Context(), AgentRoute("захар"), "привет", "захар"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Ты Захар.", "— юля: помогает с погодой", "— сисадм", "ask_agent", roleTail} {
		if !strings.Contains(seen, want) {
			t.Fatalf("prompt lacks %q: %q", want, seen)
		}
	}
	if strings.Contains(seen, "— захар") {
		t.Fatalf("role listed itself: %q", seen)
	}
	// A role added later is known from the next turn without any restart.
	agents["новый"] = &Agent{Name: "новый", Answerer: quiet, Description: "только что создан"}
	if err := e.SetAgents(agents); err != nil {
		t.Fatal(err)
	}
	if _, err := e.answerAs(t.Context(), AgentRoute("захар"), "привет", "захар"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "— новый: только что создан") {
		t.Fatalf("new role is not in the roster: %q", seen)
	}
}

func TestRosterAbsentForSingleRoleAndKeepsEmptyPromptDefault(t *testing.T) {
	var seen string
	capture := completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		seen = messages[0].Content
		return "ok", nil
	})
	e := &Engine{}
	if err := e.SetAgents(map[string]*Agent{"один": {Name: "один", Answerer: capture, Prompt: "Ты один."}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.answerAs(t.Context(), AgentRoute("один"), "привет", "один"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(seen, "других ролей") {
		t.Fatalf("roster for a single role: %q", seen)
	}
}

func TestHandoffMovesConversationToAnotherRole(t *testing.T) {
	var target string
	asker := completionFunc(func(ctx context.Context, _ []llm.Message, _ bool) (string, error) {
		if err := RequestHandoff(ctx, target); err != nil {
			t.Errorf("handoff: %v", err)
		}
		return "передаю", nil
	})
	quiet := completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "ok", nil })
	e := &Engine{}
	if err := e.SetAgents(map[string]*Agent{
		"а": {Name: "а", Answerer: asker, Peers: true},
		"б": {Name: "б", Answerer: quiet},
	}); err != nil {
		t.Fatal(err)
	}
	target = "б"
	res, err := e.answerAs(t.Context(), AgentRoute("а"), "поговорим с б", "а")
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "agent:а" || res.Handoff != "agent:б" || res.Answer != "передаю" {
		t.Fatalf("result: %+v", res)
	}
	// A role without the peer flag has no slot: the request is refused.
	if err := RequestHandoff(t.Context(), "б"); err != ErrNoHandoff {
		t.Fatalf("handoff without a slot: %v", err)
	}
	// Handing over to oneself changes nothing.
	target = "а"
	res, err = e.answerAs(t.Context(), AgentRoute("а"), "x", "а")
	if err != nil || res.Handoff != "" {
		t.Fatalf("self handoff: %+v %v", res, err)
	}
}

func TestPeerDepthBlocksNestedCalls(t *testing.T) {
	ctx := t.Context()
	if !PeerCallAllowed(ctx) {
		t.Fatal("first call refused")
	}
	if PeerCallAllowed(WithPeerDepth(ctx)) {
		t.Fatal("nested call allowed")
	}
}

func TestOneLineFoldsDescription(t *testing.T) {
	if got := OneLine("  а\nб\r\n— в\tг\x00 "); got != "а б — в г" {
		t.Fatalf("OneLine: %q", got)
	}
}

func TestHandoffToVanishedRoleIgnored(t *testing.T) {
	asker := completionFunc(func(ctx context.Context, _ []llm.Message, _ bool) (string, error) {
		_ = RequestHandoff(ctx, "нет")
		return "ок", nil
	})
	e := &Engine{}
	if err := e.SetAgents(map[string]*Agent{"а": {Name: "а", Answerer: asker, Peers: true}}); err != nil {
		t.Fatal(err)
	}
	res, err := e.answerAs(t.Context(), AgentRoute("а"), "x", "а")
	if err != nil || res.Handoff != "" {
		t.Fatalf("vanished handoff: %+v %v", res, err)
	}
}

func TestAgentDescriptionLimit(t *testing.T) {
	quiet := completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "ok", nil })
	e := &Engine{}
	long := strings.Repeat("я", MaxAgentDescriptionRunes+1)
	if err := e.SetAgents(map[string]*Agent{"а": {Name: "а", Answerer: quiet, Description: long}}); err == nil {
		t.Fatal("oversized description accepted")
	}
}

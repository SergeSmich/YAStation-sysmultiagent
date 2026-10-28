package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"

	"quasarbridge/internal/llm"
)

func TestAgentNameAndRouteRoundTrip(t *testing.T) {
	for _, name := range []string{"briefing", "house", "a", "agent-2", "agent_2", strings.Repeat("b", 32)} {
		if !ValidAgentName(name) {
			t.Fatalf("rejected valid name %q", name)
		}
		got, ok := ParseAgentRoute(AgentRoute(name))
		if !ok || got != name {
			t.Fatalf("round trip %q: %q %v", name, got, ok)
		}
	}
	for _, name := range []string{"", "Briefing", "brief ing", "-briefing", "_briefing", "briefing!", "агент", strings.Repeat("b", 33)} {
		if ValidAgentName(name) {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
	for _, route := range []string{"llm", "yandex", "home", "", "agent:", "agent:Briefing", "music"} {
		if _, ok := ParseAgentRoute(route); ok {
			t.Fatalf("route %q parsed as an agent", route)
		}
	}
}

func TestSetAgentsValidatesRegistry(t *testing.T) {
	answerer := completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "ok", nil })
	var e Engine
	if err := e.SetAgents(map[string]*Agent{"Briefing": {Name: "Briefing", Answerer: answerer}}); err == nil {
		t.Fatal("invalid name accepted")
	}
	if err := e.SetAgents(map[string]*Agent{"briefing": {Name: "briefing"}}); err == nil {
		t.Fatal("agent without a model client accepted")
	}
	if err := e.SetAgents(map[string]*Agent{"briefing": nil}); err == nil {
		t.Fatal("nil agent accepted")
	}
	long := strings.Repeat("я", MaxAgentPromptRunes+1)
	if err := e.SetAgents(map[string]*Agent{"briefing": {Name: "briefing", Answerer: answerer, Prompt: long}}); err == nil {
		t.Fatal("oversized prompt accepted")
	}
	if err := e.SetAgents(map[string]*Agent{"briefing": {Name: "briefing", Answerer: answerer, Prompt: "Отвечай как аналитик."}}); err != nil {
		t.Fatal(err)
	}
	if names := e.AgentNames(); len(names) != 1 || names[0] != "briefing" {
		t.Fatalf("names: %v", names)
	}
	if _, ok := e.Agent("house"); ok {
		t.Fatal("unknown agent resolved")
	}
	// An empty registry must fail loudly instead of borrowing the default model.
	if err := e.SetAgents(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Agent("briefing"); ok {
		t.Fatal("cleared registry kept an agent")
	}
}

func TestAgentRouteAnswersWithItsOwnModelAndPrompt(t *testing.T) {
	var seen []llm.Message
	role := completionFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		if structured {
			t.Error("role answer requested structured output")
		}
		seen = append([]llm.Message(nil), messages...)
		return "Роль: сводка готова", nil
	})
	defaultAnswers := 0
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		defaultAnswers++
		return "Общий ответ", nil
	})}
	if err := e.SetAgents(map[string]*Agent{"briefing": {Name: "briefing", Answerer: role, Prompt: "Ты роль briefing."}}); err != nil {
		t.Fatal(err)
	}
	policy := DefaultRoutingConfig()
	policy.Rules = []RoutingRule{{Name: "briefing", Route: AgentRoute("briefing"), Match: RoutingMatch{ContainsAny: []string{"сводк"}}}}
	policy.DefaultRoute = "llm"
	if err := e.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	decision := e.Decide(t.Context(), "дай сводку")
	if decision.Route != "agent:briefing" {
		t.Fatalf("decision: %+v", decision)
	}
	result, err := e.ResolveDecision(t.Context(), "дай сводку", decision)
	if err != nil {
		t.Fatal(err)
	}
	if result.Route != "agent:briefing" || result.Answer != "Роль: сводка готова" {
		t.Fatalf("result: %+v", result)
	}
	if defaultAnswers != 0 {
		t.Fatalf("default model answered %d times", defaultAnswers)
	}
	if len(seen) < 2 || seen[0].Role != "system" || !strings.HasPrefix(seen[0].Content, "Ты роль briefing.") || !strings.Contains(seen[0].Content, roleTail) {
		t.Fatalf("prompt: %+v", seen)
	}
	if seen[len(seen)-1].Content != "дай сводку" {
		t.Fatalf("turn text: %+v", seen)
	}
}

func TestAgentRouteKeepsDialogueAndFallsBackToDefaultModel(t *testing.T) {
	role := completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		return "ответ роли", nil
	})
	var defaultMessages []llm.Message
	e := &Engine{LLM: completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		defaultMessages = append([]llm.Message(nil), messages...)
		return "общий ответ", nil
	})}
	if err := e.SetAgents(map[string]*Agent{"briefing": {Name: "briefing", Answerer: role, Prompt: "Ты роль briefing."}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ResolveDecision(t.Context(), "первый ход", RouteDecision{Route: "agent:briefing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resolve(t.Context(), "второй ход"); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, m := range defaultMessages {
		joined += m.Role + ":" + m.Content + "\n"
	}
	if !strings.Contains(joined, "первый ход") || !strings.Contains(joined, "ответ роли") {
		t.Fatalf("role turn did not reach the shared dialogue: %s", joined)
	}
}

func TestUnknownAgentRouteFailsLoudly(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("unknown agent fell back to the default model")
		return "", nil
	})}
	result, err := e.ResolveDecision(t.Context(), "дай сводку", RouteDecision{Route: "agent:ghost"})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err: %v", err)
	}
	if result.Route != "agent:ghost" {
		t.Fatalf("route lost: %+v", result)
	}
}

func TestMalformedAgentRouteIsReported(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("malformed agent route fell back to the default model")
		return "", nil
	})}
	for _, route := range []string{"agent:", "agent:Briefing", "agent:briefing!"} {
		result, err := e.ResolveDecision(t.Context(), "дай сводку", RouteDecision{Route: route})
		if err == nil || !strings.Contains(err.Error(), "invalid agent route") {
			t.Fatalf("route %q: %v", route, err)
		}
		if result.Route != route {
			t.Fatalf("route %q lost: %+v", route, result)
		}
	}
}

func TestSetAgentsIsSafeDuringAnswers(t *testing.T) {
	answerer := completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "ответ роли", nil })
	var e Engine
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 200; n++ {
				if err := e.SetAgents(map[string]*Agent{"briefing": {Name: "briefing", Answerer: answerer, Prompt: "Ты роль briefing."}}); err != nil {
					t.Error(err)
					return
				}
				if _, ok := e.Agent("briefing"); !ok {
					t.Error("agent disappeared during replacement")
					return
				}
				if names := e.AgentNames(); len(names) != 1 {
					t.Errorf("names: %v", names)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestAnswerPromptKeepsFormattingRules(t *testing.T) {
	if got := answerPrompt(""); got != defaultAnswerPrompt {
		t.Fatal("empty role replaced the shared prompt")
	}
	if got := answerPrompt("   "); got != defaultAnswerPrompt {
		t.Fatal("blank role replaced the shared prompt")
	}
	got := answerPrompt("Ты роль briefing.")
	if !strings.HasPrefix(got, "Ты роль briefing.") || !strings.HasSuffix(got, roleTail) {
		t.Fatalf("role prompt: %q", got)
	}
}

func TestRoutingAcceptsAgentRoutes(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.Rules = []RoutingRule{{Name: "briefing", Route: AgentRoute("briefing"), Match: RoutingMatch{ContainsAny: []string{"сводка"}}}}
	cfg.DefaultRoute = AgentRoute("briefing")
	cfg.Classifier.OnError = AgentRoute("briefing")
	if _, err := CompileRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"agent:", "agent:Briefing", "agent", "agent:briefing!"} {
		cfg := DefaultRoutingConfig()
		cfg.DefaultRoute = route
		if _, err := CompileRouting(cfg); err == nil {
			t.Fatalf("accepted default_route %q", route)
		}
	}
}

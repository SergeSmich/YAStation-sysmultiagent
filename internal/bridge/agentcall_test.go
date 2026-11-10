package bridge

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

// A spoken call must be cheap and predictable: a bare call answers from the
// configuration without touching a model, a question goes to the role's own
// client, and a switch that is off changes nothing at all.

func callableEngine(t *testing.T, answerer Completer, callable bool, greeting string) *Engine {
	t.Helper()
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "корневая модель", nil
	})}
	// The default route without classification keeps these tests about the call,
	// not about the classifier.
	if err := e.SetRouting(RoutingConfig{DefaultRoute: "yandex"}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetAgents(map[string]*Agent{"helper": {
		Name: "helper", Answerer: answerer, Prompt: "Ты — хелпер.",
		Callable: callable, Greeting: greeting,
	}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestBareRoleCallAnswersWithoutTheModel(t *testing.T) {
	var modelCalls int32
	role := completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		modelCalls++
		return "роль ответила", nil
	})
	e := callableEngine(t, role, true, "Я здесь. Слушаю.")
	e.SetRoleCall(RoleCallConfig{Enabled: true})

	decision := e.Decide(context.Background(), "Алиса, позови helper")
	if decision.Route != "agent:helper" || decision.Source != "role_call" {
		t.Fatalf("decision=%+v", decision)
	}
	if decision.Answer != "Я здесь. Слушаю." {
		t.Fatalf("answer=%q", decision.Answer)
	}
	resolution, err := e.ResolveDecision(context.Background(), "Алиса, позови helper", decision)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Route != "agent:helper" || resolution.Answer != "Я здесь. Слушаю." {
		t.Fatalf("resolution=%+v", resolution)
	}
	if modelCalls != 0 {
		t.Fatalf("a bare call asked the model %d times", modelCalls)
	}
}

// TestBareRoleCallWithoutGreeting: a role that never wrote a greeting still says
// something, and still without a model.
func TestBareRoleCallWithoutGreeting(t *testing.T) {
	e := callableEngine(t, completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "", nil
	}), true, "")
	e.SetRoleCall(RoleCallConfig{Enabled: true})
	decision := e.Decide(context.Background(), "helper, ты здесь?")
	if decision.Answer != DefaultRoleGreeting {
		t.Fatalf("answer=%q", decision.Answer)
	}
}

func TestRoleCallQuestionUsesTheRoleClient(t *testing.T) {
	var roleCalls int32
	var seen []llm.Message
	role := completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		roleCalls++
		seen = messages
		return "половина второго", nil
	})
	// No deployment client at all: the question must not need one.
	e := &Engine{}
	if err := e.SetRouting(RoutingConfig{DefaultRoute: "yandex"}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetAgents(map[string]*Agent{"helper": {
		Name: "helper", Answerer: role, Prompt: "Ты — хелпер.", Callable: true,
	}}); err != nil {
		t.Fatal(err)
	}
	e.SetRoleCall(RoleCallConfig{Enabled: true})

	decision := e.Decide(context.Background(), "спроси helper, который час")
	if decision.Route != "agent:helper" || decision.Source != "role_call" {
		t.Fatalf("decision=%+v", decision)
	}
	if decision.Answer != "" {
		t.Fatalf("a question must not answer itself: %q", decision.Answer)
	}
	resolution, err := e.ResolveDecision(context.Background(), "спроси helper, который час", decision)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Answer != "половина второго" {
		t.Fatalf("answer=%q", resolution.Answer)
	}
	if roleCalls != 1 {
		t.Fatalf("role calls=%d", roleCalls)
	}
	if len(seen) == 0 || !strings.Contains(seen[0].Content, "хелпер") {
		t.Fatalf("the role prompt did not reach the role: %v", seen)
	}
}

func TestRoleCallDryRunDoesNotAct(t *testing.T) {
	var modelCalls int32
	e := callableEngine(t, completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		modelCalls++
		return "", nil
	}), true, "Я здесь.")
	e.SetRoleCall(RoleCallConfig{Enabled: true, DryRun: true})

	decision := e.Decide(context.Background(), "позови helper")
	if decision.Source == "role_call" {
		t.Fatalf("a dry run acted: %+v", decision)
	}
	if decision.Route != "yandex" {
		t.Fatalf("a dry run changed routing: %+v", decision)
	}
	if modelCalls != 0 {
		t.Fatalf("a dry run asked the model %d times", modelCalls)
	}
}

func TestRoleCallSwitchOff(t *testing.T) {
	e := callableEngine(t, completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "", nil
	}), true, "Я здесь.")
	// Default: the feature is off even when a role is callable.
	decision := e.Decide(context.Background(), "позови helper")
	if decision.Route == "agent:helper" || decision.Source == "role_call" {
		t.Fatalf("the call fired while the feature is off: %+v", decision)
	}
}

func TestNonCallableRoleStaysSilent(t *testing.T) {
	e := callableEngine(t, completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "", nil
	}), false, "Я здесь.")
	e.SetRoleCall(RoleCallConfig{Enabled: true})
	if _, ok := e.MatchRoleCall("позови helper"); ok {
		t.Fatal("a role that is not callable was resolved")
	}
	decision := e.Decide(context.Background(), "позови helper")
	if decision.Route == "agent:helper" {
		t.Fatalf("a silent role was called: %+v", decision)
	}
}

func TestSetAgentsRejectsSpokenNameClashes(t *testing.T) {
	answerer := completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "", nil
	})
	cases := []struct {
		name   string
		agents map[string]*Agent
	}{
		{name: "alias equals another role name", agents: map[string]*Agent{
			"helper": {Name: "helper", Answerer: answerer, Aliases: []string{"urka"}},
			"urka":   {Name: "urka", Answerer: answerer},
		}},
		{name: "two roles share an alias", agents: map[string]*Agent{
			"helper": {Name: "helper", Answerer: answerer, Aliases: []string{"друг"}},
			"urka":   {Name: "urka", Answerer: answerer, Aliases: []string{"друг"}},
		}},
		{name: "invalid alias", agents: map[string]*Agent{
			"helper": {Name: "helper", Answerer: answerer, Aliases: []string{"Помощник"}},
		}},
		{name: "too many aliases", agents: map[string]*Agent{
			"helper": {Name: "helper", Answerer: answerer, Aliases: []string{"a", "b", "c", "d", "e"}},
		}},
		{name: "greeting too long", agents: map[string]*Agent{
			"helper": {Name: "helper", Answerer: answerer, Greeting: strings.Repeat("я", MaxAgentGreetingRunes+1)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{}
			if err := e.SetAgents(tc.agents); err == nil {
				t.Fatal("the registry was accepted")
			}
			if names := e.AgentNames(); len(names) != 0 {
				t.Fatalf("a rejected registry stayed live: %v", names)
			}
		})
	}
}

// TestSetAgentsAcceptsDistinctAliases: the same word twice is fine, two roles
// sharing a word is not.
func TestSetAgentsAcceptsDistinctAliases(t *testing.T) {
	answerer := completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "", nil
	})
	e := &Engine{}
	err := e.SetAgents(map[string]*Agent{
		"helper": {Name: "helper", Answerer: answerer, Callable: true, Aliases: []string{"помощник", "helper"}},
		"urka":   {Name: "urka", Answerer: answerer, Callable: true, Aliases: []string{"урка"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"позови помощника", "позови урку", "урка, ты здесь?"} {
		if _, ok := e.MatchRoleCall(phrase); !ok {
			t.Fatalf("%q did not resolve", phrase)
		}
	}
	if _, ok := e.MatchRoleCall("позови helper"); !ok {
		t.Fatal("the role name stopped resolving when it was also an alias")
	}
}

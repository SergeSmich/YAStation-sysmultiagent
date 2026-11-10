package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"

	"quasarbridge/internal/llm"
)

// The llm route answers with the main pair; everything else keeps the deployment
// client. These tests pin the three seams the panel depends on.

func recordingCompleter(answer string, seen *[]llm.Message) completionFunc {
	return completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		if seen != nil {
			*seen = messages
		}
		return answer, nil
	})
}

func TestSetMainSwitchesAnswerer(t *testing.T) {
	root := recordingCompleter("root", nil)
	var seen []llm.Message
	role := recordingCompleter("role", &seen)
	e := &Engine{LLM: root}

	before, err := e.Fallback(context.Background(), "привет")
	if err != nil {
		t.Fatal(err)
	}
	if before.Answer != "root" {
		t.Fatalf("empty main answered %q, want the deployment client", before.Answer)
	}
	e.SetMain(role, role, "Ты — хелпер.")
	after, err := e.Fallback(context.Background(), "привет")
	if err != nil {
		t.Fatal(err)
	}
	if after.Answer != "role" {
		t.Fatalf("main_agent answered %q, want the role client", after.Answer)
	}
	if len(seen) == 0 || !strings.Contains(seen[0].Content, "хелпер") {
		t.Fatalf("role prompt did not reach the model: %v", seen)
	}
	// A role prompt keeps the spoken-answer tail, exactly like any role.
	if !strings.Contains(seen[0].Content, "прочитан вслух") {
		t.Fatalf("role prompt lost the speech constraint: %q", seen[0].Content)
	}
	if got := e.mainPrompt(); got != "Ты — хелпер." {
		t.Fatalf("mainPrompt=%q", got)
	}
}

func TestClassifierUsesMainClientWithItsOwnPrompt(t *testing.T) {
	root := completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("the deployment client must not classify when a role answers")
		return "", nil
	})
	var seen []llm.Message
	role := completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		seen = messages
		return `{"route":"llm"}`, nil
	})
	e := &Engine{LLM: root}
	if err := e.SetRouting(DefaultRoutingConfig()); err != nil {
		t.Fatal(err)
	}
	e.SetMain(role, role, "Ты — хелпер.")

	decision := e.Decide(context.Background(), "запрос для классификации")
	if decision.Source != "classifier" {
		t.Fatalf("source=%q, want the classifier path", decision.Source)
	}
	if decision.Route != "llm" {
		t.Fatalf("route=%q", decision.Route)
	}
	if len(seen) == 0 || seen[0].Content != DefaultRoutingConfig().Classifier.Prompt {
		t.Fatalf("the classifier must keep its own system prompt, got %v", seen)
	}
}

func TestGuardStaysOnTheDeploymentClient(t *testing.T) {
	var rootCalls, roleCalls int32
	root := completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		rootCalls++
		return "root", nil
	})
	role := completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		roleCalls++
		return "role", nil
	})
	e := &Engine{LLM: root, Guard: root}
	e.SetMain(role, role, "")
	if _, err := e.guard().Complete(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if rootCalls != 1 || roleCalls != 0 {
		t.Fatalf("guard used root=%d role=%d, want the deployment client only", rootCalls, roleCalls)
	}
}

func TestGuardFallsBackToTheDeploymentClient(t *testing.T) {
	var rootCalls int32
	root := completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		rootCalls++
		return "root", nil
	})
	role := completionFunc(func(context.Context, []llm.Message, bool) (string, error) { return "role", nil })
	e := &Engine{LLM: root}
	e.SetMain(role, role, "")
	if _, err := e.guard().Complete(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if rootCalls != 1 {
		t.Fatalf("guard calls=%d, want the deployment client", rootCalls)
	}
}

func TestSetMainIgnoresNil(t *testing.T) {
	root := recordingCompleter("root", nil)
	e := &Engine{LLM: root}
	e.SetMain(nil, nil, "")
	if got := e.answerer(); got == nil {
		t.Fatal("a nil pair must leave the deployment client in place")
	}
	e.SetMain(nil, root, "")
	if got := e.answerer(); got == nil {
		t.Fatal("an answerer without a thinking client must still answer")
	}
}

// TestSetMainIsSafeUnderLoad is the -race case: the panel saves while turns are
// in flight, and a reader must never see a half-replaced pair.
func TestSetMainIsSafeUnderLoad(t *testing.T) {
	root := recordingCompleter("root", nil)
	role := recordingCompleter("role", nil)
	e := &Engine{LLM: root}
	e.SetMain(root, root, "")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = e.Fallback(context.Background(), "привет")
				_ = e.llmClient()
				_ = e.mainPrompt()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			e.SetMain(role, role, "Ты — хелпер.")
			e.SetMain(root, root, "")
		}
	}()
	wg.Wait()
}

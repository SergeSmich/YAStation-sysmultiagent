package bridge

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

func TestPinnedRouteReachesOnlyTheInjectedTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 64)
	down := make(chan []byte, 64)
	engine := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Error("the default model answered a pinned turn")
		return "", nil
	})}
	if err := engine.SetAgents(map[string]*Agent{"briefing": {Name: "briefing", Answerer: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return "Сводка готова", nil
	}), Prompt: "Ты роль briefing."}}); err != nil {
		t.Fatal(err)
	}
	// Everything without a pin goes to the cloud, so the test distinguishes the
	// pin from an accidental routing rule.
	policy := DefaultRoutingConfig()
	policy.Rules = nil
	policy.DefaultRoute = "yandex"
	if err := engine.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	counted := &countingResolver{Engine: engine}
	n := NewNative(ctx,
		func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil },
		func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil },
		counted)
	defer n.Close()

	frame, _, err := SyntheticTextInput("какая сегодня сводка")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Client(WithPinnedRoute(ctx, AgentRoute("briefing")), websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
	// The role answers locally; the answer may be preceded by other frames
	// (cards, stream controls), so read until the speech text arrives.
	answered := false
	for !answered {
		select {
		case b := <-down:
			text := wire.Decode(b, wire.ServerToClient).Server.GetEvent().GetAliceResponse().GetVoiceResponse().GetOutputSpeech().GetText()
			answered = strings.Contains(text, "Сводка готова")
		case <-ctx.Done():
			t.Fatal("no local answer for the pinned turn")
		}
	}
	if got := counted.count(); got != 0 {
		t.Fatalf("pinned turn was classified %d times", got)
	}

	// The next turn of the same session has no pin: it must be classified and
	// forwarded as it arrived. Since this frame arrives upstream after the
	// pinned turn is done, the loop also proves the pinned text never left the
	// bridge.
	plain := pinTextInput("plain-turn")
	if err := n.Client(ctx, websocket.MessageBinary, plain); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case b := <-up:
			text := wire.Decode(b, wire.ClientToServer).Client.GetEvent().GetTextInput().GetRequest().GetEvent().GetText()
			if strings.Contains(text, "какая сегодня сводка") {
				t.Fatal("pinned turn was forwarded to the cloud")
			}
			if text == "обычный вопрос" {
				if got := counted.count(); got != 1 {
					t.Fatalf("unpinned turn classified %d times", got)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("unpinned turn did not reach the cloud")
		}
	}
}

// countingResolver proves a pinned turn never asks the classifier. The decision
// is taken on a session goroutine, hence the atomic counter.
type countingResolver struct {
	*Engine
	decisions atomic.Int64
}

func (r *countingResolver) count() int64 { return r.decisions.Load() }

func (r *countingResolver) Decide(ctx context.Context, text string) RouteDecision {
	r.decisions.Add(1)
	return r.Engine.Decide(ctx, text)
}

func pinTextInput(id string) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: id},
		Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{
			Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("обычный вопрос")}},
		}},
	}}})
}

func TestPinnedRouteHelpers(t *testing.T) {
	ctx := t.Context()
	if route, ok := PinnedRoute(ctx); ok || route != "" {
		t.Fatalf("clean context reports a pin: %q %v", route, ok)
	}
	if blank := WithPinnedRoute(ctx, "   "); blank != ctx {
		t.Fatal("blank route changed the context")
	}
	pinned := WithPinnedRoute(ctx, " agent:briefing ")
	if route, ok := PinnedRoute(pinned); !ok || route != "agent:briefing" {
		t.Fatalf("pin = %q %v", route, ok)
	}
	if route, ok := PinnedRoute(ctx); ok || route != "" {
		t.Fatalf("pin leaked to the parent context: %q %v", route, ok)
	}
}

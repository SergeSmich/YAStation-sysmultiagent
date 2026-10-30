package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"

	"quasarbridge/internal/dialogue"
)

// voiceWindow is the microphone window the station opens by itself: after a
// start word and again after every answer. Each window carries its own stream,
// exactly like the station numbers them.
func voiceWindow(id string, stream int64) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: id, StreamId: new(stream)},
		Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{
			Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}},
			Format:  new("audio/opus"),
		}},
	}}})
}

// utter runs one window up to the routing decision: open the microphone,
// dictate a phrase, wait for the decision. A turn routed to the cloud produces
// no local answer, so the answer is not expected here.
func utter(t *testing.T, ctx context.Context, n *Native, router *presetRouter, id string, stream int64, phrase string) RouteDecision {
	t.Helper()
	if err := n.Client(ctx, websocket.MessageBinary, voiceWindow(id, stream)); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(ctx, websocket.MessageBinary, asrFinalFrame(id, phrase)); err != nil {
		t.Fatal(err)
	}
	select {
	case decision := <-router.decisions:
		select {
		case text := <-router.texts:
			if text != phrase {
				t.Fatalf("routed text = %q, want %q", text, phrase)
			}
		case <-ctx.Done():
			t.Fatal("the phrase was not routed")
		}
		return decision
	case <-ctx.Done():
		t.Fatal("no decision")
		return RouteDecision{}
	}
}

// speak is utter plus the local answer a pinned or held turn produces.
func speak(t *testing.T, ctx context.Context, n *Native, router *presetRouter, down chan []byte, id string, stream int64, phrase string) RouteDecision {
	t.Helper()
	decision := utter(t, ctx, n, router, id, stream, phrase)
	readAnswer(t, ctx, down)
	return decision
}

// TestRoleKeepsTheDialogueAcrossWindows verifies the role a start word selected
// keeps the conversation after it answers: the station reopens the microphone
// by itself, and those phrases must not fall back to the cloud or the default
// model in the middle of a dialogue.
func TestRoleKeepsTheDialogueAcrossWindows(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 32)
	down := make(chan []byte, 32)
	router := &presetRouter{texts: make(chan string, 8), decisions: make(chan RouteDecision, 8), answer: "Ответ роли."}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, router)
	defer n.Close()

	if err := n.ArmPreset("", "agent:briefing", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := speak(t, ctx, n, router, down, "held-1", 11, "какая погода"); got.Source != "pinned" || got.Route != "agent:briefing" {
		t.Fatalf("first turn = %+v", got)
	}
	if got := speak(t, ctx, n, router, down, "held-2", 12, "а завтра"); got.Source != "dialogue_agent" || got.Route != "agent:briefing" {
		t.Fatalf("second turn = %+v", got)
	}
	if got := speak(t, ctx, n, router, down, "held-3", 13, "спасибо"); got.Source != "dialogue_agent" || got.Route != "agent:briefing" {
		t.Fatalf("third turn = %+v", got)
	}
}

// TestSilenceEndsTheHeldRole verifies an empty reopened window ends the
// conversation: the person walked away, so the next phrase is classified from
// scratch instead of going to a role nobody is talking to any more.
func TestSilenceEndsTheHeldRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 32)
	down := make(chan []byte, 32)
	router := &presetRouter{texts: make(chan string, 8), decisions: make(chan RouteDecision, 8), answer: "Ответ роли."}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, router)
	defer n.Close()

	if err := n.ArmPreset("", "agent:briefing", time.Minute); err != nil {
		t.Fatal(err)
	}
	speak(t, ctx, n, router, down, "silent-1", 21, "сводка на сегодня")
	// The reopened window catches no speech at all.
	if err := n.Client(ctx, websocket.MessageBinary, voiceWindow("silent-2", 22)); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(ctx, websocket.MessageBinary, asrFinalFrame("silent-2", "")); err != nil {
		t.Fatal(err)
	}
	readAnswer(t, ctx, down)
	// Now a phrase belongs to nobody again, so it is routed by the rules and
	// goes to the cloud: no local answer is produced for that turn.
	if got := utter(t, ctx, n, router, "silent-3", 23, "включи свет"); got.Source != "rules" {
		t.Fatalf("a phrase after silence was held by the role: %+v", got)
	}
}

// TestFarewellEndsTheHeldRole verifies an explicit phrase ends the conversation
// on purpose; the turn itself is still routed as usual, so "хватит" keeps
// stopping playback instead of becoming a question to the role.
func TestFarewellEndsTheHeldRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 32)
	down := make(chan []byte, 32)
	router := &presetRouter{texts: make(chan string, 8), decisions: make(chan RouteDecision, 8), answer: "Ответ роли."}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, router)
	defer n.Close()

	if err := n.ArmPreset("", "agent:briefing", time.Minute); err != nil {
		t.Fatal(err)
	}
	speak(t, ctx, n, router, down, "bye-1", 31, "что нового")
	if got := utter(t, ctx, n, router, "bye-2", 32, "хватит"); got.Source != "rules" {
		t.Fatalf("the farewell was sent to the role: %+v", got)
	}
	if got := utter(t, ctx, n, router, "bye-3", 33, "какая погода"); got.Source != "rules" {
		t.Fatalf("the dialogue survived the farewell: %+v", got)
	}
}

// TestStartWordWinsOverHeldRole verifies a new start word replaces the role a
// conversation was holding: the word the person just said is more specific than
// the conversation it interrupted.
func TestStartWordWinsOverHeldRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 32)
	down := make(chan []byte, 32)
	router := &presetRouter{texts: make(chan string, 8), decisions: make(chan RouteDecision, 8), answer: "Ответ роли."}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, router)
	defer n.Close()

	if err := n.ArmPreset("", "agent:briefing", time.Minute); err != nil {
		t.Fatal(err)
	}
	speak(t, ctx, n, router, down, "swap-1", 41, "что нового")
	if err := n.ArmPreset("", "agent:home", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := speak(t, ctx, n, router, down, "swap-2", 42, "включи свет"); got.Source != "pinned" || got.Route != "agent:home" {
		t.Fatalf("the start word did not win: %+v", got)
	}
	// The new role owns the following windows.
	if got := speak(t, ctx, n, router, down, "swap-3", 43, "а в спальне"); got.Source != "dialogue_agent" || got.Route != "agent:home" {
		t.Fatalf("the dialogue did not switch roles: %+v", got)
	}
}

// TestHeldRoleExpiresAndStaysPerSpeaker verifies the hold is bounded in time and
// scoped to the speaker: another device of the same household never inherits a
// conversation it did not have.
func TestHeldRoleExpiresAndStaysPerSpeaker(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	n := NewNative(ctx, nopSender, nopSender, nil)
	defer n.Close()
	n.HoldTTL = 20 * time.Millisecond

	first := dialogue.WithSpeaker(context.Background(), "device:first")
	second := dialogue.WithSpeaker(context.Background(), "device:second")
	n.holdRole(first, "agent:briefing")
	if got := n.heldRouteOf(first); got != "agent:briefing" {
		t.Fatalf("held route = %q", got)
	}
	if got := n.heldRouteOf(second); got != "" {
		t.Fatalf("another speaker inherited the dialogue: %q", got)
	}
	// A plain model answer does not own a conversation.
	n.holdRole(first, "llm")
	if got := n.heldRouteOf(first); got != "agent:briefing" {
		t.Fatalf("a non-role route replaced the hold: %q", got)
	}
	time.Sleep(40 * time.Millisecond)
	if got := n.heldRouteOf(first); got != "" {
		t.Fatalf("an expired hold survived: %q", got)
	}
	// Releasing reports the route it ended and forgets it.
	n.holdRole(first, "agent:briefing")
	if route, ok := n.releaseRole(first); !ok || route != "agent:briefing" {
		t.Fatalf("release = %q, %v", route, ok)
	}
	if got := n.heldRouteOf(first); got != "" {
		t.Fatalf("the released hold came back: %q", got)
	}
}

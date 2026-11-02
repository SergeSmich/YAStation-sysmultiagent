package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestVoiceGateAnswersWithoutRouting verifies a phrase the gate handles (a
// spoken permission decision) is answered locally and never reaches the router.
func TestVoiceGateAnswersWithoutRouting(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 32)
	down := make(chan []byte, 32)
	router := &presetRouter{texts: make(chan string, 8), decisions: make(chan RouteDecision, 8), answer: "Ответ роли."}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, router)
	defer n.Close()
	gateCalls := make(chan string, 8)
	n.VoiceGate = func(_ context.Context, text string) (string, string, bool) {
		gateCalls <- text
		if text == "да только сейчас" {
			return "Разрешено, выполняю.", "agent:sysadmin", true
		}
		return "", "", false
	}

	if err := n.Client(ctx, websocket.MessageBinary, voiceWindow("gate-1", 21)); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(ctx, websocket.MessageBinary, asrFinalFrame("gate-1", "да только сейчас")); err != nil {
		t.Fatal(err)
	}
	readAnswer(t, ctx, down)
	select {
	case got := <-gateCalls:
		if got != "да только сейчас" {
			t.Fatalf("gate saw %q", got)
		}
	default:
		t.Fatal("gate was not asked")
	}
	select {
	case d := <-router.decisions:
		t.Fatalf("a gated phrase reached the router: %+v", d)
	case <-time.After(200 * time.Millisecond):
	}
	// The gate named the role, so it keeps the conversation: the next phrase,
	// which the gate declines, continues with that role.
	got := speak(t, ctx, n, router, down, "gate-2", 22, "какая погода")
	if got.Source != "dialogue_agent" || got.Route != "agent:sysadmin" {
		t.Fatalf("declined phrase = %+v", got)
	}
}
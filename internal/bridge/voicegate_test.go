package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"quasarbridge/internal/wire"
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
	n.VoiceGate = func(_ context.Context, text string, _ VoiceGateCall) (string, string, bool) {
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
// syncBuffer collects log lines written from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestVoiceGateKeepAliveStartsProgress verifies a gate can hold the cloud
// request open. Without the silent stream the station gives up at ~15 s and
// plays "произошла ошибка" while the gate is still waiting for an action.
func TestVoiceGateKeepAliveStartsProgress(t *testing.T) {
	var logs syncBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan wire.Frame, 32)
	down := make(chan wire.Frame, 32)
	router := &presetRouter{texts: make(chan string, 8), decisions: make(chan RouteDecision, 8), answer: "Ответ роли."}
	n := NewNative(ctx,
		func(_ context.Context, _ websocket.MessageType, b []byte) error {
			up <- wire.Decode(b, wire.ClientToServer)
			return nil
		},
		func(_ context.Context, _ websocket.MessageType, b []byte) error {
			down <- wire.Decode(b, wire.ServerToClient)
			return nil
		}, router)
	defer n.Close()
	n.ProgressAfter = time.Millisecond
	n.ProgressInterval = 10 * time.Millisecond

	starts := make(chan time.Time, 1)
	n.VoiceGate = func(_ context.Context, _ string, call VoiceGateCall) (string, string, bool) {
		if call.KeepAlive == nil {
			return "", "", false
		}
		starts <- call.Started
		stop := call.KeepAlive()
		time.Sleep(40 * time.Millisecond) // stands in for a slow approved action
		stop()
		return "Хорошо, делаю.", "agent:sysadmin", true
	}

	if err := n.Client(ctx, websocket.MessageBinary, voiceWindow("gate-progress", 31)); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(ctx, websocket.MessageBinary, asrFinalFrame("gate-progress", "да")); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-starts:
		if at.IsZero() {
			t.Fatal("the gate was not told when the turn started")
		}
	case <-time.After(time.Second):
		t.Fatal("the gate was not asked")
	}
	// The stream cancels the cloud's own answer and opens a deferred response:
	// exactly what the resolver path does for a slow local answer.
	canceled, streaming := false, false
	deadline := time.After(time.Second)
	for !canceled || !streaming {
		select {
		case f := <-up:
			if f.Kind() == "CancelRequest" {
				canceled = true
			}
		case f := <-down:
			if r := f.Server.GetEvent().GetAliceResponse(); r != nil && r.GetResponse().GetIsStreaming() {
				streaming = true
			}
		case <-deadline:
			t.Fatalf("keep-alive was not started: canceled=%v streaming=%v", canceled, streaming)
		}
	}
	time.Sleep(50 * time.Millisecond) // the gate logs the turn right after it answers
	if !strings.Contains(logs.String(), "native_voice_gate") {
		t.Fatalf("the gated turn was not logged: %s", logs.String())
	}
}

// TestVoiceGateWithoutProgressAnswersInstead verifies the keep-alive is a no-op
// when local_progress is off: the gate then has to answer on its own, so the
// turn must produce a plain response instead of a streaming one.
func TestVoiceGateWithoutProgressAnswersInstead(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	down := make(chan wire.Frame, 32)
	n := NewNative(ctx,
		func(context.Context, websocket.MessageType, []byte) error { return nil },
		func(_ context.Context, _ websocket.MessageType, b []byte) error {
			down <- wire.Decode(b, wire.ServerToClient)
			return nil
		},
		&presetRouter{texts: make(chan string, 8), decisions: make(chan RouteDecision, 8), answer: "Ответ роли."})
	defer n.Close()
	// ProgressAfter stays zero: local_progress is disabled in this configuration.
	n.VoiceGate = func(_ context.Context, _ string, call VoiceGateCall) (string, string, bool) {
		if call.KeepAlive == nil {
			t.Error("the gate was not given a keep-alive hook")
			return "", "", false
		}
		stop := call.KeepAlive()
		defer stop()
		return "Хорошо, делаю.", "", true
	}
	if err := n.Client(ctx, websocket.MessageBinary, voiceWindow("gate-noprogress", 41)); err != nil {
		t.Fatal(err)
	}
	if err := n.Server(ctx, websocket.MessageBinary, asrFinalFrame("gate-noprogress", "да")); err != nil {
		t.Fatal(err)
	}
	// The session mirrors AsrResult downstream before the answer, so read past
	// the mirrored frames instead of assuming the answer comes first.
	answered := false
	for !answered {
		select {
		case f := <-down:
			r := f.Server.GetEvent().GetAliceResponse()
			if r == nil {
				continue
			}
			if r.GetResponse().GetIsStreaming() {
				t.Fatal("the progress stream started without local_progress")
			}
			if r.GetVoiceResponse().GetOutputSpeech().GetText() != "Хорошо, делаю." {
				t.Fatalf("answer=%q", r.GetVoiceResponse().GetOutputSpeech().GetText())
			}
			answered = true
		case <-ctx.Done():
			t.Fatal("no answer")
		}
	}
}

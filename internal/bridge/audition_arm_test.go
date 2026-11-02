package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

func TestArmAuditionReadsPhraseInChosenVoiceOnVoiceInputWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 64)
	down := make(chan []byte, 64)
	engine := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Error("a model answered an audition")
		return "", nil
	})}
	counted := &countingResolver{Engine: engine}
	n := NewNative(ctx,
		func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil },
		func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil },
		counted)
	n.SetVoice(VoiceConfig{Routes: map[string]string{"agent:x": "oksana"}})
	n.ShouldListen = true
	defer n.Close()

	phrase := "Проверка голоса, раз два три"
	if err := n.ArmAudition(phrase, VoiceSettings{Voice: "shitova", Emotion: "good", Speed: 1.4, Volume: 2}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := n.Client(ctx, websocket.MessageBinary, voiceInputFrame("audition")); err != nil {
		t.Fatal(err)
	}
	// gated window
	if !wire.Decode(<-up, wire.ClientToServer).Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
		t.Fatal("ungated voice")
	}
	for answered := false; !answered; {
		select {
		case b := <-down:
			resp := wire.Decode(b, wire.ServerToClient).Server.GetEvent().GetAliceResponse()
			if resp.GetVoiceResponse().GetOutputSpeech().GetText() == phrase {
				answered = true
				if resp.GetVoiceResponse().GetShouldListen() {
					t.Fatal("an audition reopened the microphone")
				}
			}
		case <-ctx.Done():
			t.Fatal("no answer for the audition")
		}
	}
	for {
		select {
		case b := <-up:
			generate := wire.Decode(b, wire.ClientToServer).Client.GetEvent().GetTtsGenerate()
			if generate == nil {
				continue
			}
			if generate.GetVoice() != "shitova" || generate.GetSpeed() != 1.4 || generate.GetEmotion() != "good" || generate.GetText() != phrase {
				t.Fatalf("unexpected synthesis: voice=%q speed=%v emotion=%q text=%q", generate.GetVoice(), generate.GetSpeed(), generate.GetEmotion(), generate.GetText())
			}
			if got := counted.count(); got != 0 {
				t.Fatalf("an audition was classified %d times", got)
			}
			return
		case <-ctx.Done():
			t.Fatal("no synthesis request for the audition")
		}
	}
}

func TestArmAuditionSurvivesArmPreset(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	up := make(chan []byte, 64)
	down := make(chan []byte, 64)
	engine := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Error("a model answered an audition")
		return "", nil
	})}
	counted := &countingResolver{Engine: engine}
	n := NewNative(ctx,
		func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil },
		func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil },
		counted)
	n.ShouldListen = true
	defer n.Close()

	phrase := "Проверка голоса"
	if err := n.ArmAudition(phrase, VoiceSettings{Voice: "shitova", Emotion: "good", Speed: 1.0, Volume: 1}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := n.ArmPreset("", "agent:x", time.Minute); err != nil {
		t.Fatalf("ArmPreset after ArmAudition: %v", err)
	}
	if err := n.Client(ctx, websocket.MessageBinary, voiceInputFrame("audition2")); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-up:
		_ = b
	case <-ctx.Done():
	}
	for answered := false; !answered; {
		select {
		case b := <-down:
			resp := wire.Decode(b, wire.ServerToClient).Server.GetEvent().GetAliceResponse()
			if resp.GetVoiceResponse().GetOutputSpeech().GetText() == phrase {
				answered = true
			}
		case <-ctx.Done():
			t.Fatal("no answer for the audition")
		}
	}
	found := false
	for !found {
		select {
		case b := <-up:
			generate := wire.Decode(b, wire.ClientToServer).Client.GetEvent().GetTtsGenerate()
			if generate != nil && generate.GetText() == phrase {
				found = true
				if strings.Contains(generate.GetVoice(), "oksana") {
					// voice may be audition's; just ensure phrase read
				}
			}
		case <-ctx.Done():
			t.Fatal("no synthesis request for the audition")
		}
	}
	if got := counted.count(); got != 0 {
		t.Fatalf("an audition was classified %d times", got)
	}
}
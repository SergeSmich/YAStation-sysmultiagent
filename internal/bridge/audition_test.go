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

// TestAuditionReadsThePhraseInTheChosenVoice: the phrase is answered verbatim by
// the bridge, in the requested voice, without a classifier, a model or a reopened
// microphone.
func TestAuditionReadsThePhraseInTheChosenVoice(t *testing.T) {
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

	phrase := "Проверка голоса, раз два три"
	if _, err := n.Audition(ctx, phrase, VoiceSettings{Voice: "shitova", Emotion: "good", Speed: 1.4, Volume: 2}); err != nil {
		t.Fatal(err)
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
				if text := wire.Decode(b, wire.ClientToServer).Client.GetEvent().GetTextInput().GetRequest().GetEvent().GetText(); strings.Contains(text, phrase) {
					t.Fatal("the audition phrase was forwarded to the cloud")
				}
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

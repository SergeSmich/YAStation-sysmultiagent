package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asr "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"

	"quasarbridge/internal/wire"
)

// voiceInputFrame is the microphone window a start word opens: the station sends
// it itself, which is why the prepared reaction may be bound to that request.
func voiceInputFrame(id string) []byte {
	return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: id, StreamId: new(int64(9))},
		Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{
			Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}},
			Format:  new("audio/opus"),
		}},
	}}})
}

// asrFinalFrame is the recognition the station reports when the person stops
// speaking: without it a pinned window would never be answered.
func asrFinalFrame(id, normalized string) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: id},
		Event:  &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new(normalized)}}}},
	}}})
}

// presetRouter answers like a role would: the text of the turn is a question to
// the model, so the reply is never the phrase itself. It also records the
// decision, which is how a test tells a pinned turn from a classified one.
type presetRouter struct {
	texts     chan string
	decisions chan RouteDecision
	answer    string
}

func (r *presetRouter) Decide(context.Context, string) RouteDecision {
	return RouteDecision{Route: "yandex", Source: "rules"}
}

func (r *presetRouter) Resolve(ctx context.Context, text string) (Resolution, error) {
	return r.ResolveDecision(ctx, text, r.Decide(ctx, text))
}

func (r *presetRouter) ResolveDecision(_ context.Context, text string, decision RouteDecision) (Resolution, error) {
	r.texts <- text
	r.decisions <- decision
	return Resolution{Route: decision.Route, Answer: r.answer}, nil
}

// readAnswer returns the spoken answer, skipping the frames the session mirrors
// downstream.
func readAnswer(t *testing.T, ctx context.Context, down chan []byte) *response.TAliceResponseProto {
	t.Helper()
	for {
		select {
		case b := <-down:
			if f := wire.Decode(b, wire.ServerToClient); f.Server != nil {
				if got := f.Server.GetEvent().GetAliceResponse(); got != nil {
					return got
				}
			}
		case <-ctx.Done():
			t.Fatal("no answer")
			return nil
		}
	}
}

// TestPresetTextIsAnsweredByTheRole verifies a word with a phrase asks the role
// instead of reading the phrase out: the text is routed with the pinned route,
// the model writes the answer, and the role's voice speaks it.
func TestPresetTextIsAnsweredByTheRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	up := make(chan []byte, 8)
	down := make(chan []byte, 8)
	router := &presetRouter{texts: make(chan string, 1), decisions: make(chan RouteDecision, 1), answer: "Доброе утро. Пять писем, две встречи."}
	routes := make(chan string, 1)
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, router)
	n.SetVoice(VoiceConfig{Routes: map[string]string{"agent:briefing": "oksana"}})
	n.OnRoute = func(_, route string) { routes <- route }
	defer n.Close()

	if err := n.ArmPreset("  Дай краткую сводку.  ", "agent:briefing", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := n.Client(ctx, websocket.MessageBinary, voiceInputFrame("preset")); err != nil {
		t.Fatal(err)
	}
	// The window is gated first, then released: the phrase replaces the
	// recognition nobody is going to dictate into this window.
	if !wire.Decode(<-up, wire.ClientToServer).Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
		t.Fatal("ungated voice")
	}
	if kind := wire.Decode(<-up, wire.ClientToServer).Kind(); kind != "CancelRequest" {
		t.Fatalf("upstream=%s want CancelRequest", kind)
	}
	select {
	case text := <-router.texts:
		if text != "Дай краткую сводку." {
			t.Fatalf("routed text = %q", text)
		}
	case <-ctx.Done():
		t.Fatal("the phrase was not routed")
	}
	select {
	case decision := <-router.decisions:
		if decision.Route != "agent:briefing" || decision.Source != "pinned" {
			t.Fatalf("decision = %+v", decision)
		}
	case <-ctx.Done():
		t.Fatal("no decision")
	}
	answer := readAnswer(t, ctx, down)
	if got := answer.GetVoiceResponse().GetOutputSpeech().GetText(); got != router.answer {
		t.Fatalf("answer=%q, want the model's one", got)
	}
	if !answer.GetVoiceResponse().GetHasVoiceResponse() {
		t.Fatal("answer is silent")
	}
	select {
	case route := <-routes:
		if route != "agent:briefing" {
			t.Fatalf("route=%s", route)
		}
	case <-ctx.Done():
		t.Fatal("route not recorded")
	}
	// The answer is synthesized with the voice of the role, not the default one.
	tts := wire.Decode(<-up, wire.ClientToServer).Client.GetEvent().GetTtsGenerate()
	if tts == nil {
		t.Fatal("no synthesis request")
	}
	if got := tts.GetVoice(); got != "oksana" {
		t.Fatalf("voice = %q", got)
	}
	if got := tts.GetText(); got != router.answer {
		t.Fatalf("tts text = %q", got)
	}
}

// TestPresetWithoutTextWaitsForThePhrase verifies a word with no phrase only
// opens the microphone: the route is pinned, and the person's own words reach
// it with the final recognition.
func TestPresetWithoutTextWaitsForThePhrase(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	up := make(chan []byte, 8)
	down := make(chan []byte, 8)
	router := &presetRouter{texts: make(chan string, 1), decisions: make(chan RouteDecision, 1), answer: "Свет включён."}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(_ context.Context, _ websocket.MessageType, b []byte) error { down <- b; return nil }, router)
	n.SetVoice(VoiceConfig{Routes: map[string]string{"agent:home": "jane"}})
	defer n.Close()

	if err := n.ArmPreset("", "agent:home", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := n.Client(ctx, websocket.MessageBinary, voiceInputFrame("listen")); err != nil {
		t.Fatal(err)
	}
	if !wire.Decode(<-up, wire.ClientToServer).Client.GetEvent().GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
		t.Fatal("ungated voice")
	}
	// Nothing to ask yet: the window is open and the phrase is still unsaid.
	select {
	case text := <-router.texts:
		t.Fatalf("routed %q before the person spoke", text)
	case <-time.After(50 * time.Millisecond):
	}
	if err := n.Server(ctx, websocket.MessageBinary, asrFinalFrame("listen", "включи свет в кабинете")); err != nil {
		t.Fatal(err)
	}
	if kind := wire.Decode(<-up, wire.ClientToServer).Kind(); kind != "CancelRequest" {
		t.Fatalf("upstream=%s want CancelRequest", kind)
	}
	select {
	case text := <-router.texts:
		if text != "включи свет в кабинете" {
			t.Fatalf("routed text = %q", text)
		}
	case <-ctx.Done():
		t.Fatal("the spoken phrase was not routed")
	}
	select {
	case decision := <-router.decisions:
		if decision.Route != "agent:home" || decision.Source != "pinned" {
			t.Fatalf("decision = %+v", decision)
		}
	case <-ctx.Done():
		t.Fatal("no decision")
	}
	if got := readAnswer(t, ctx, down).GetVoiceResponse().GetOutputSpeech().GetText(); got != router.answer {
		t.Fatalf("answer=%q", got)
	}
	tts := wire.Decode(<-up, wire.ClientToServer).Client.GetEvent().GetTtsGenerate()
	if tts == nil || tts.GetVoice() != "jane" {
		t.Fatalf("tts = %v", tts)
	}
}

// TestPresetRecognitionDoesNotReplaceThePhrase verifies the empty recognition of
// a window nobody spoke into cannot overwrite the prepared phrase.
func TestPresetRecognitionDoesNotReplaceThePhrase(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	up := make(chan []byte, 8)
	router := &presetRouter{texts: make(chan string, 1), decisions: make(chan RouteDecision, 1), answer: "Пять писем."}
	n := NewNative(ctx, func(_ context.Context, _ websocket.MessageType, b []byte) error { up <- b; return nil }, func(context.Context, websocket.MessageType, []byte) error { return nil }, router)
	defer n.Close()

	if err := n.ArmPreset("Сводка", "agent:briefing", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := n.Client(ctx, websocket.MessageBinary, voiceInputFrame("mixed")); err != nil {
		t.Fatal(err)
	}
	<-up // the approval frame
	if err := n.Server(ctx, websocket.MessageBinary, asrFinalFrame("mixed", "шум")); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-router.texts:
		if text != "Сводка" {
			t.Fatalf("routed text = %q, want the prepared phrase", text)
		}
	case <-ctx.Done():
		t.Fatal("the phrase was not routed")
	}
}

// TestPresetIsOneShot verifies the next window does not repeat the same words.
func TestPresetIsOneShot(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	n := NewNative(ctx, nopSender, nopSender, nil)
	defer n.Close()
	if err := n.ArmPreset("текст", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	preset, ok := n.takePreset()
	if !ok || preset.Text != "текст" {
		t.Fatalf("preset = %+v, ok = %v", preset, ok)
	}
	if _, ok := n.takePreset(); ok {
		t.Fatal("the same reaction was used twice")
	}
}

// TestPresetExpires verifies a reaction is dropped instead of being used long
// after the word that armed it.
func TestPresetExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	n := NewNative(ctx, nopSender, nopSender, nil)
	defer n.Close()
	if err := n.ArmPreset("старый", "", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, ok := n.takePreset(); ok {
		t.Fatal("an expired reaction was used")
	}
}

// TestPresetRequiresTextOrRoute verifies a word that would do nothing fails
// loudly, while a word that only pins a role is accepted.
func TestPresetRequiresTextOrRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	n := NewNative(ctx, nopSender, nopSender, nil)
	defer n.Close()
	if err := n.ArmPreset("   ", "  ", time.Minute); err == nil {
		t.Fatal("an empty reaction was accepted")
	}
	if err := n.ArmPreset("", "agent:briefing", time.Minute); err != nil {
		t.Fatalf("a pin-only reaction was rejected: %v", err)
	}
	preset, ok := n.takePreset()
	if !ok || preset.Text != "" || preset.Route != "agent:briefing" {
		t.Fatalf("preset = %+v, ok = %v", preset, ok)
	}
}

// TestInjectedTextDoesNotTakePreset verifies the reaction belongs to the
// station's own window, not to a phrase the bridge injected.
func TestInjectedTextDoesNotTakePreset(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	routed := make(chan string, 1)
	release := make(chan struct{})
	n := NewNative(ctx, nopSender, nopSender, resolverFunc(func(_ context.Context, text string) (Resolution, error) {
		routed <- text
		<-release
		return Resolution{Route: "llm", Answer: "ответ"}, nil
	}))
	defer n.Close()
	if err := n.ArmPreset("заготовка", "agent:briefing", time.Minute); err != nil {
		t.Fatal(err)
	}
	frame, _, err := SyntheticTextInput("привет")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Client(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-routed:
		if text != "привет" {
			t.Fatalf("routed text = %q", text)
		}
	case <-ctx.Done():
		t.Fatal("the typed phrase was not routed")
	}
	if preset, ok := n.takePreset(); !ok || preset.Text != "заготовка" {
		t.Fatal("an injected phrase consumed the prepared reaction")
	}
	close(release)
}

func nopSender(context.Context, websocket.MessageType, []byte) error { return nil }

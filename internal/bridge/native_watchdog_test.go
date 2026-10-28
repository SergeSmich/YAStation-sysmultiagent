package bridge

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	asr "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	cancellation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	ack "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/request_stat_ack"
	ready "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/server_request_ready"
	spotter "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/spotter_validation"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	fileplayer "github.com/freequasar/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/audio_file_player"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/wire"
)

func newWatchdogTestSession(t *testing.T, timeout time.Duration, route string) (*Native, chan wire.Frame, chan wire.Frame, *atomic.Int64) {
	t.Helper()
	up, down := make(chan wire.Frame, 16), make(chan wire.Frame, 16)
	var stalls atomic.Int64
	n := NewNative(t.Context(), func(_ context.Context, _ websocket.MessageType, data []byte) error {
		up <- wire.Decode(data, wire.ClientToServer)
		return nil
	}, func(_ context.Context, _ websocket.MessageType, data []byte) error {
		down <- wire.Decode(data, wire.ServerToClient)
		return nil
	}, resolverFunc(func(context.Context, string) (Resolution, error) {
		return Resolution{Route: route, Answer: "Локальный ответ."}, nil
	}))
	n.FirstFrameTimeout = timeout
	n.OnCloudStall = func() { stalls.Add(1) }
	t.Cleanup(n.Close)
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("cloud-watchdog-request")); err != nil {
		t.Fatal(err)
	}
	return n, up, down, &stalls
}

func waitWatchdogFrame(t *testing.T, frames <-chan wire.Frame, kind string) wire.Frame {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case frame := <-frames:
			if frame.Kind() == kind {
				return frame
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", kind)
			return wire.Frame{}
		}
	}
}

func TestNativeCloudWatchdogDisabled(t *testing.T) {
	_, up, down, stalls := newWatchdogTestSession(t, 0, "yandex")
	if got := waitWatchdogFrame(t, up, "TextInput").Kind(); got != "TextInput" {
		t.Fatal(got)
	}
	time.Sleep(300 * time.Millisecond)
	if stalls.Load() != 0 {
		t.Fatalf("stall count=%d", stalls.Load())
	}
	select {
	case frame := <-up:
		t.Fatalf("unexpected upstream frame while disabled: %s", frame.Kind())
	default:
	}
	select {
	case frame := <-down:
		t.Fatalf("unexpected downstream frame while disabled: %s", frame.Kind())
	default:
	}
}

func TestNativeCloudWatchdogFiresAndSendsErrorResponse(t *testing.T) {
	_, up, down, stalls := newWatchdogTestSession(t, 150*time.Millisecond, "yandex")
	_ = waitWatchdogFrame(t, up, "TextInput")
	cancel := waitWatchdogFrame(t, up, "CancelRequest")
	if got := cancel.Client.GetEvent().GetCancelRequest().GetRequestMessageId(); got != "cloud-watchdog-request" {
		t.Fatalf("cancel request id=%q", got)
	}
	answer := waitWatchdogFrame(t, down, "AliceResponse").Server.GetEvent().GetAliceResponse()
	if answer.GetVoiceResponse().GetHasVoiceResponse() || answer.GetVoiceResponse().GetShouldListen() {
		t.Fatalf("unexpected voice response flags: %v", answer.GetVoiceResponse())
	}
	if got := answer.GetVoiceResponse().GetOutputSpeech().GetText(); got != "" {
		t.Fatalf("fallback speech=%q", got)
	}
	if len(answer.GetResponse().GetDirectives()) != 1 || answer.GetResponse().GetDirectives()[0].GetName() != "local_audio_file_play" {
		t.Fatalf("fallback directives=%v", answer.GetResponse().GetDirectives())
	}
	var sound fileplayer.TAudioFilePlayerCapability_TLocalAudioFilePlayDirective
	if err := proto.Unmarshal(answer.GetResponse().GetDirectives()[0].GetPayloadRaw(), &sound); err != nil {
		t.Fatal(err)
	}
	if sound.GetSoundToPlay() != fileplayer.TAudioFilePlayerCapability_SpotterError {
		t.Fatalf("fallback sound=%v", sound.GetSoundToPlay())
	}
	if stalls.Load() != 1 {
		t.Fatalf("stall count=%d", stalls.Load())
	}
	select {
	case frame := <-up:
		if frame.Kind() == "TtsGenerate" {
			t.Fatal("fallback unexpectedly requested TTS")
		}
	default:
	}
}

func TestStatErrorFlattensAndBounds(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"  timeout  ":   "timeout",
		"a\nb":          "a b",
		"x\r\ny\tz":     "x  y z",
		"\\ \"quoted\"": "\\ \"quoted\"",
	}
	// A newline from the device must not be able to break a JSONL record in
	// two if the log handler is ever switched to text, and a very long message
	// must not bloat the log the panel shows.
	for raw, want := range cases {
		if got := statError(raw); got != want {
			t.Fatalf("statError(%q)=%q, want %q", raw, got, want)
		}
	}
	long := strings.Repeat("я", 400)
	got := statError(long)
	if runes := []rune(got); len(runes) != 161 || !strings.HasSuffix(got, "…") {
		t.Fatalf("statError truncated to %d runes, want 161 ending in an ellipsis", len(runes))
	}
	// The device chooses the bytes: invalid UTF-8 must degrade, not panic.
	if got := statError(string([]byte{0xff, 0xfe, 'a'})); !strings.HasSuffix(got, "a") {
		t.Fatalf("invalid UTF-8 survived as %q", got)
	}
}

func TestNativeCloudWatchdogStopsOnFirstFrame(t *testing.T) {
	n, up, down, stalls := newWatchdogTestSession(t, 150*time.Millisecond, "yandex")
	_ = waitWatchdogFrame(t, up, "TextInput")
	readyFrame := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: "cloud-watchdog-request"},
		Event:  &server.TServerMessage_TEvent_ServerRequestReady{ServerRequestReady: &ready.TServerRequestReady{}},
	}}}
	if err := n.Server(t.Context(), websocket.MessageBinary, aa(readyFrame)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if stalls.Load() != 0 {
		t.Fatalf("stall count=%d", stalls.Load())
	}
	select {
	case frame := <-up:
		if frame.Kind() == "CancelRequest" {
			t.Fatal("first server frame did not stop watchdog")
		}
	default:
	}
	select {
	case frame := <-down:
		if frame.Kind() == "AliceResponse" {
			t.Fatal("fallback sent after first server frame")
		}
	default:
	}
}

// The cloud correlates its own acknowledgement frames with the request within
// milliseconds of the forward. None of them is the answer, so none may disarm
// the first-frame watchdog: otherwise a silent stall never falls back.
func TestNativeCloudWatchdogIgnoresAcknowledgedChatter(t *testing.T) {
	n, up, down, stalls := newWatchdogTestSession(t, 150*time.Millisecond, "yandex")
	_ = waitWatchdogFrame(t, up, "TextInput")
	eventHeader := func() *header.TEventHeader { return &header.TEventHeader{RefMessageId: "cloud-watchdog-request"} }
	chatter := []*server.TServerMessage{
		{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
			Header: eventHeader(),
			Event:  &server.TServerMessage_TEvent_SpotterValidation{SpotterValidation: &spotter.TSpotterValidation{Result: spotter.TSpotterValidation_Ok}},
		}}},
		{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
			Header: eventHeader(),
			Event:  &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("")}}}},
		}}},
		{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
			Header: eventHeader(),
			Event:  &server.TServerMessage_TEvent_RequestStatAck{RequestStatAck: &ack.TRequestStatAck{}},
		}}},
	}
	for _, msg := range chatter {
		if err := n.Server(t.Context(), websocket.MessageBinary, aa(msg)); err != nil {
			t.Fatal(err)
		}
	}
	cancel := waitWatchdogFrame(t, up, "CancelRequest")
	if got := cancel.Client.GetEvent().GetCancelRequest().GetRequestMessageId(); got != "cloud-watchdog-request" {
		t.Fatalf("cancel request id=%q", got)
	}
	answer := waitWatchdogFrame(t, down, "AliceResponse").Server.GetEvent().GetAliceResponse()
	if got := answer.GetResponse().GetCards()[0].GetText(); got != "Сейчас не получилось получить ответ." {
		t.Fatalf("fallback text=%q", got)
	}
	if stalls.Load() != 1 {
		t.Fatalf("stall count=%d", stalls.Load())
	}
}

func TestNativeCloudWatchdogDoesNotArmForLocalRoute(t *testing.T) {
	_, up, _, stalls := newWatchdogTestSession(t, 150*time.Millisecond, "llm")
	frame := waitWatchdogFrame(t, up, "TtsGenerate")
	if frame.Client.GetEvent().GetTtsGenerate().GetText() != "Локальный ответ." {
		t.Fatal("unexpected local speech request")
	}
	time.Sleep(250 * time.Millisecond)
	if stalls.Load() != 0 {
		t.Fatalf("stall count=%d", stalls.Load())
	}
	select {
	case frame := <-up:
		if frame.Kind() == "CancelRequest" {
			t.Fatal("local route sent a watchdog cancellation")
		}
	default:
	}
}

func TestNativeCloudWatchdogRaceWithCancel(t *testing.T) {
	n, up, down, stalls := newWatchdogTestSession(t, 200*time.Millisecond, "yandex")
	_ = waitWatchdogFrame(t, up, "TextInput")
	cancel := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: "client-cancel"},
		Event:  &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: "cloud-watchdog-request"}},
	}}})
	if err := n.Client(t.Context(), websocket.MessageBinary, cancel); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if stalls.Load() != 0 {
		t.Fatalf("stall count=%d", stalls.Load())
	}
	count := 0
	for len(up) > 0 {
		if frame := <-up; frame.Kind() == "CancelRequest" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("upstream cancellation count=%d, want 1", count)
	}
	if len(down) != 0 {
		t.Fatal("canceled request received fallback response")
	}
}

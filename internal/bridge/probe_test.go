package bridge

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	exceptions "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/event_exception"
	"github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	"github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/wire"
)

func nextProbeGenerate(t *testing.T, frames <-chan wire.Frame) wire.Frame {
	t.Helper()
	select {
	case frame := <-frames:
		if frame.Kind() != "TtsGenerate" {
			t.Fatalf("got %s, want TtsGenerate", frame.Kind())
		}
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("missing TtsGenerate")
		return wire.Frame{}
	}
}

func probeSpeak(id string, stream uint32) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &event_header.TEventHeader{RefMessageId: id, StreamId: new(int64(stream))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &tts_speak.TTtsSpeak{Format: "audio/opus"}}}}})
}

func probeException(id, message string) []byte {
	return aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &event_header.TEventHeader{RefMessageId: id}, Event: &server.TServerMessage_TEvent_EventException{EventException: &exceptions.TEventException{Error: &exceptions.TEventException_TError{ErrorMessage: message}}}}}})
}

func TestNativeProbeCollectsAcceptAndReject(t *testing.T) {
	n, up, down := soundSession(t, nil)
	cases := []VoiceProbeCase{{Voice: "jane"}, {Voice: "x"}}
	type outcome struct {
		results []VoiceProbeResult
		err     error
	}
	result := make(chan outcome, 1)
	go func() {
		results, err := n.Probe(t.Context(), cases, time.Second)
		result <- outcome{results: results, err: err}
	}()
	first := nextProbeGenerate(t, up)
	firstID := first.Client.GetEvent().GetHeader().GetMessageId()
	if err := n.Server(t.Context(), websocket.MessageBinary, probeSpeak(firstID, 7)); err != nil {
		t.Fatal(err)
	}
	second := nextProbeGenerate(t, up)
	secondID := second.Client.GetEvent().GetHeader().GetMessageId()
	if err := n.Server(t.Context(), websocket.MessageBinary, probeException(secondID, "Unsupported voice 'x'")); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.results) != 2 || !got.results[0].OK || got.results[0].Voice != "jane" || got.results[0].ElapsedMS < 0 || got.results[0].ElapsedMS >= 1000 {
		t.Fatalf("unexpected accepted result: %+v", got.results)
	}
	if got.results[1].OK || got.results[1].Error != "Unsupported voice 'x'" || got.results[1].Voice != "x" || got.results[1].ElapsedMS < 0 || got.results[1].ElapsedMS >= 1000 {
		t.Fatalf("unexpected rejected result: %+v", got.results[1])
	}
	if len(down) != 0 {
		t.Fatalf("probe responses leaked downstream: %d frames", len(down))
	}
	firstGenerate := first.Client.GetEvent().GetTtsGenerate()
	if firstGenerate.GetLang() != "ru-RU" || firstGenerate.GetFormat() != "audio/opus" || firstGenerate.GetText() != "Проверка голоса" {
		t.Fatalf("unexpected generated probe: %v", firstGenerate)
	}
}

func TestNativeProbeTimeout(t *testing.T) {
	n, up, down := soundSession(t, nil)
	result := make(chan []VoiceProbeResult, 1)
	go func() {
		results, err := n.Probe(t.Context(), []VoiceProbeCase{{Voice: "jane"}}, 100*time.Millisecond)
		if err != nil {
			t.Errorf("Probe returned error: %v", err)
		}
		result <- results
	}()
	_ = nextProbeGenerate(t, up)
	select {
	case got := <-result:
		if len(got) != 1 || got[0].OK || got[0].Error != "timeout" {
			t.Fatalf("unexpected timeout result: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not time out")
	}
	if len(down) != 0 {
		t.Fatal("timeout produced downstream frames")
	}
}

func TestNativeProbeDropsAudio(t *testing.T) {
	n, up, down := soundSession(t, nil)
	result := make(chan []VoiceProbeResult, 1)
	go func() {
		results, _ := n.Probe(t.Context(), []VoiceProbeCase{{Voice: "jane"}}, time.Second)
		result <- results
	}()
	frame := nextProbeGenerate(t, up)
	id := frame.Client.GetEvent().GetHeader().GetMessageId()
	if err := n.Server(t.Context(), websocket.MessageBinary, probeSpeak(id, 7)); err != nil {
		t.Fatal(err)
	}
	if got := <-result; len(got) != 1 || !got[0].OK {
		t.Fatalf("unexpected probe result: %+v", got)
	}
	if err := n.Server(t.Context(), websocket.MessageBinary, probeSpeak(id, 8)); err != nil {
		t.Fatal(err)
	}
	audio := append(binary.BigEndian.AppendUint32(nil, 7), []byte{1, 2, 3}...)
	if err := n.Server(t.Context(), websocket.MessageBinary, audio); err != nil {
		t.Fatal(err)
	}
	if len(down) != 0 {
		t.Fatalf("probe audio leaked downstream: %d frames", len(down))
	}
}

// TestNativeProbePlayIsHeardOnTheStation: with Play the probe sample is not a
// silent check — the TtsSpeak and its audio reach the station, which is the
// whole point of the panel's «Прослушать … на станции» button.
func TestNativeProbePlayIsHeardOnTheStation(t *testing.T) {
	n, up, down := soundSession(t, nil)
	result := make(chan []VoiceProbeResult, 1)
	go func() {
		got, _ := n.Probe(t.Context(), []VoiceProbeCase{{Voice: "omazh", Play: true}}, time.Second)
		result <- got
	}()
	frame := nextProbeGenerate(t, up)
	id := frame.Client.GetEvent().GetHeader().GetMessageId()
	if err := n.Server(t.Context(), websocket.MessageBinary, probeSpeak(id, 9)); err != nil {
		t.Fatal(err)
	}
	if got := <-result; len(got) != 1 || !got[0].OK {
		t.Fatalf("unexpected probe result: %+v", got)
	}
	select {
	case forwarded := <-down:
		if forwarded.Kind() != "TtsSpeak" {
			t.Fatalf("forwarded %s, want the probe TtsSpeak", forwarded.Kind())
		}
	default:
		t.Fatal("played probe TtsSpeak was swallowed")
	}
	n.mu.Lock()
	_, plays := n.probePlay[9]
	n.mu.Unlock()
	if !plays {
		t.Fatal("played probe stream is not marked for forwarding")
	}
}

func TestNativeProbeRejectsConcurrentRun(t *testing.T) {
	n, up, _ := soundSession(t, nil)
	firstResult := make(chan error, 1)
	go func() {
		_, err := n.Probe(t.Context(), []VoiceProbeCase{{Voice: "jane"}}, 100*time.Millisecond)
		firstResult <- err
	}()
	_ = nextProbeGenerate(t, up)
	if _, err := n.Probe(t.Context(), []VoiceProbeCase{{Voice: "alena"}}, time.Second); err == nil || !strings.Contains(err.Error(), "voice probe already running") {
		t.Fatalf("second Probe error = %v, want already-running", err)
	}
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
}

func TestNativeProbeContextCancellation(t *testing.T) {
	n, up, _ := soundSession(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := n.Probe(ctx, []VoiceProbeCase{{Voice: "jane"}}, time.Second)
		result <- err
	}()
	_ = nextProbeGenerate(t, up)
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled probe returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled probe did not return")
	}
}

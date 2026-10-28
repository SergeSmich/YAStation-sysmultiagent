package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	exceptions "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/event_exception"
	tts "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_generate"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
)

func TestVoiceConfigResolve(t *testing.T) {
	config := VoiceConfig{Voice: "filipp", Emotion: "neutral", Speed: 1.1, Routes: map[string]string{"home": "zahar"}}
	if got := config.Resolve("llm"); got != (VoiceSettings{Voice: "filipp", Emotion: "neutral", Speed: 1.1}) {
		t.Fatalf("llm = %+v", got)
	}
	if got := config.Resolve("home"); got.Voice != "zahar" {
		t.Fatalf("home = %+v", got)
	}
	// Cloud answers are synthesized by the station; this value is never used,
	// the bridge only resolves it for symmetry.
	if got := config.Resolve("yandex"); got.Voice != "filipp" {
		t.Fatalf("yandex = %+v", got)
	}
	empty := VoiceConfig{Routes: map[string]string{"home": "zahar"}}
	if got := empty.Resolve("llm"); got != (VoiceSettings{}) {
		t.Fatalf("unconfigured route = %+v", got)
	}
	if (VoiceConfig{}).Enabled() || !empty.Enabled() || !config.Enabled() {
		t.Fatal("Enabled mismatch")
	}
}

func TestVoiceConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config VoiceConfig
		bad    bool
	}{
		{name: "empty"},
		{name: "full", config: VoiceConfig{Voice: "filipp", Emotion: "neutral", Speed: 1.1, Routes: map[string]string{"home": "zahar"}}},
		{name: "space", config: VoiceConfig{Voice: "fil ipp"}, bad: true},
		{name: "padded", config: VoiceConfig{Voice: " filipp"}, bad: true},
		{name: "padded emotion", config: VoiceConfig{Voice: "filipp", Emotion: " neutral"}, bad: true},
		{name: "slow", config: VoiceConfig{Voice: "filipp", Speed: 0.05}, bad: true},
		{name: "fast", config: VoiceConfig{Voice: "filipp", Speed: 3.5}, bad: true},
		{name: "cloud override", config: VoiceConfig{Routes: map[string]string{"yandex": "jane"}}, bad: true},
		{name: "empty route", config: VoiceConfig{Routes: map[string]string{"": "jane"}}, bad: true},
		{name: "empty voice", config: VoiceConfig{Routes: map[string]string{"llm": " "}}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if (err != nil) != tc.bad {
				t.Fatalf("Validate() = %v, want bad=%v", err, tc.bad)
			}
		})
	}
}

// TestVoiceConfigUnknownVoices covers the control-plane guard: the dispenser
// rejects these names, so a patch that would make every answer fall back to the
// stock Alice voice must be refused before it reaches config.json.
func TestVoiceConfigUnknownVoices(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config VoiceConfig
		want   []string
	}{
		{name: "empty"},
		{name: "known", config: VoiceConfig{Voice: "omazh", Candidates: []string{"oksana", "jane"}, Routes: map[string]string{"llm": "zahar"}}},
		{name: "rejected primary", config: VoiceConfig{Voice: "filipp"}, want: []string{"filipp"}},
		{name: "silent candidate", config: VoiceConfig{Voice: "omazh", Candidates: []string{"tanya"}}, want: []string{"tanya"}},
		{name: "route override", config: VoiceConfig{Routes: map[string]string{"llm": "silaerkan"}}, want: []string{"silaerkan"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.config.UnknownVoices(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("UnknownVoices() = %v, want %v", got, tc.want)
			}
		})
	}
	for _, name := range KnownVoices {
		if !IsKnownVoice(name) {
			t.Fatalf("%s not recognised", name)
		}
	}
	if IsKnownVoice("filipp") {
		t.Fatal("filipp recognised")
	}
	if !strings.Contains(KnownVoicesList(), "omazh") {
		t.Fatalf("KnownVoicesList() = %q", KnownVoicesList())
	}
}

func TestNativeSynthesizesLocalAnswerWithVoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config VoiceConfig
		want   VoiceSettings
	}{
		{name: "configured", config: VoiceConfig{Voice: "filipp", Emotion: "neutral", Speed: 1.1, Volume: 1.3}, want: VoiceSettings{Voice: "filipp", Emotion: "neutral", Speed: 1.1, Volume: 1.3}},
		{name: "empty", config: VoiceConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, up, _ := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
				return Resolution{Route: "llm", Answer: "Ответ своей модели"}, nil
			}))
			n.SetVoice(tc.config)
			if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("voice")); err != nil {
				t.Fatal(err)
			}
			for {
				f := nextSoundFrame(t, up)
				if f.Kind() != "TtsGenerate" {
					continue
				}
				generate := f.Client.GetEvent().GetTtsGenerate()
				if generate.GetVoice() != tc.want.Voice {
					t.Fatalf("voice = %q, want %q", generate.GetVoice(), tc.want.Voice)
				}
				if generate.GetEmotion() != tc.want.Emotion {
					t.Fatalf("emotion = %q, want %q", generate.GetEmotion(), tc.want.Emotion)
				}
				if generate.GetSpeed() != tc.want.Speed {
					t.Fatalf("speed = %v, want %v", generate.GetSpeed(), tc.want.Speed)
				}
				if generate.GetVolume() != tc.want.Volume {
					t.Fatalf("volume = %v, want %v", generate.GetVolume(), tc.want.Volume)
				}
				if generate.GetText() != "Ответ своей модели" {
					t.Fatalf("text = %q", generate.GetText())
				}
				return
			}
		})
	}
}

func TestNativeRetriesRejectedVoiceWithDefault(t *testing.T) {
	n, up, _ := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
		return Resolution{Route: "llm", Answer: "Ответ своей модели"}, nil
	}))
	n.SetVoice(VoiceConfig{Voice: "filipp"})
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("voice")); err != nil {
		t.Fatal(err)
	}
	var rejected string
	for {
		f := nextSoundFrame(t, up)
		if f.Kind() != "TtsGenerate" {
			continue
		}
		if f.Client.GetEvent().GetTtsGenerate().GetVoice() != "filipp" {
			t.Fatal("first synthesis must use the configured voice")
		}
		rejected = f.Client.GetEvent().GetHeader().GetMessageId()
		break
	}
	exception := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: rejected}, Event: &server.TServerMessage_TEvent_EventException{EventException: &exceptions.TEventException{Error: &exceptions.TEventException_TError{ErrorMessage: "voice is not allowed"}}}}}}
	if err := n.Server(t.Context(), websocket.MessageBinary, aa(exception)); err != nil {
		t.Fatal(err)
	}
	for {
		f := nextSoundFrame(t, up)
		if f.Kind() != "TtsGenerate" {
			continue
		}
		generate := f.Client.GetEvent().GetTtsGenerate()
		if generate.GetVoice() != "" || generate.GetEmotion() != "" || generate.GetSpeed() != 0 {
			t.Fatalf("retry kept the rejected voice: %+v", generate)
		}
		if generate.GetText() != "Ответ своей модели" {
			t.Fatalf("retry text = %q", generate.GetText())
		}
		if f.Client.GetEvent().GetHeader().GetMessageId() == rejected {
			t.Fatal("retry reused the rejected message id")
		}
		return
	}
}

// TestNativeVoiceChainTriesCandidatesInOrder walks local_voice.candidates: a
// rejected voice must be replaced by the next one, and only after all of them
// fail does the bridge fall back to the default Alice voice.
func TestNativeVoiceChainTriesCandidatesInOrder(t *testing.T) {
	n, up, _ := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
		return Resolution{Route: "llm", Answer: "Ответ своей модели"}, nil
	}))
	n.SetVoice(VoiceConfig{Voice: "filipp", Emotion: "neutral", Speed: 1.1, Volume: 1.3, Candidates: []string{"ermil", "zahar"}})
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("voice")); err != nil {
		t.Fatal(err)
	}
	want := []string{"filipp", "ermil", "zahar", ""}
	for i, name := range want {
		var generate *tts.TTtsGenerate
		var id string
		for {
			f := nextSoundFrame(t, up)
			if f.Kind() != "TtsGenerate" {
				continue
			}
			generate = f.Client.GetEvent().GetTtsGenerate()
			id = f.Client.GetEvent().GetHeader().GetMessageId()
			break
		}
		if generate.GetVoice() != name {
			t.Fatalf("attempt %d voice = %q, want %q", i, generate.GetVoice(), name)
		}
		if name != "" {
			// A candidate answers with the same delivery as the configured voice.
			if generate.GetEmotion() != "neutral" || generate.GetSpeed() != 1.1 || generate.GetVolume() != 1.3 {
				t.Fatalf("attempt %d lost emotion/speed/volume: %+v", i, generate)
			}
		}
		if i == len(want)-1 {
			return
		}
		exception := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: id}, Event: &server.TServerMessage_TEvent_EventException{EventException: &exceptions.TEventException{Error: &exceptions.TEventException_TError{ErrorMessage: "Unsupported voice '" + name + "'"}}}}}}
		if err := n.Server(t.Context(), websocket.MessageBinary, aa(exception)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNativeSpeechWatchdogAdvancesOnSilence covers the silent rejection path:
// the synthesizer accepts the request but never starts speaking, so the
// per-attempt watchdog must move through the candidates and finally retry in
// the default Alice voice exactly once.
func TestNativeSpeechWatchdogAdvancesOnSilence(t *testing.T) {
	n, up, _ := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
		return Resolution{Route: "llm", Answer: "Ответ своей модели"}, nil
	}))
	n.SetVoice(VoiceConfig{Voice: "filipp", Candidates: []string{"ermil"}})
	n.SpeechTimeout = 150 * time.Millisecond
	if err := n.Client(t.Context(), websocket.MessageBinary, soundInput("voice")); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"filipp", "ermil", ""} {
		for {
			f := nextSoundFrame(t, up)
			if f.Kind() != "TtsGenerate" {
				continue
			}
			if got := f.Client.GetEvent().GetTtsGenerate().GetVoice(); got != name {
				t.Fatalf("attempt %d voice = %q, want %q", i, got, name)
			}
			break
		}
	}
	select {
	case f := <-up:
		t.Fatalf("unexpected frame after the final retry: %s", f.Kind())
	case <-time.After(400 * time.Millisecond):
	}
}

// TestNativeVoiceHotSwapIsConcurrencySafe covers the hot-apply path: the voice
// of a live session is replaced from the HTTP goroutine while answers read it,
// so the swap must never race.
func TestNativeVoiceHotSwapIsConcurrencySafe(t *testing.T) {
	n, _, _ := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
		return Resolution{Route: "llm", Answer: "Ответ своей модели"}, nil
	}))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			n.SetVoice(VoiceConfig{Voice: "omazh", Volume: 1.2, Candidates: []string{"oksana"}})
			// The other goroutine may win the swap, so only the invariants that
			// hold for both writers are checked here.
			got := n.Voice().Resolve("llm")
			if got.Volume != 1.2 || (got.Voice != "omazh" && got.Voice != "jane") {
				t.Errorf("resolved voice = %+v", got)
				return
			}
		}
	}()
	for range 200 {
		n.SetVoice(VoiceConfig{Voice: "jane", Volume: 1.2})
		if got := n.Voice(); got.Volume != 1.2 || (got.Voice != "jane" && got.Voice != "omazh") {
			t.Fatalf("voice = %+v", got)
		}
	}
	close(stop)
	wg.Wait()
}

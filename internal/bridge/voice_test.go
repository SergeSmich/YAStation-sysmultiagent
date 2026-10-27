package bridge

import (
	"context"
	"testing"

	"github.com/coder/websocket"
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

func TestNativeSynthesizesLocalAnswerWithVoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config VoiceConfig
		want   VoiceSettings
	}{
		{name: "configured", config: VoiceConfig{Voice: "filipp", Emotion: "neutral", Speed: 1.1}, want: VoiceSettings{Voice: "filipp", Emotion: "neutral", Speed: 1.1}},
		{name: "empty", config: VoiceConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, up, _ := soundSession(t, resolverFunc(func(context.Context, string) (Resolution, error) {
				return Resolution{Route: "llm", Answer: "Ответ своей модели"}, nil
			}))
			n.Voice = tc.config
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
				if generate.GetText() != "Ответ своей модели" {
					t.Fatalf("text = %q", generate.GetText())
				}
				return
			}
		})
	}
}

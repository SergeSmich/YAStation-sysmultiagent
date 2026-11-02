package bridge

import "testing"

func TestStyleSpeech(t *testing.T) {
	cases := []struct {
		effect  string
		whisper bool
		want    string
	}{
		{"", false, "привет"},
		{"bogus", false, "привет"},
		{"", true, `<speaker is_whisper="true">привет`},
		{"megaphone", false, `<speaker effect="megaphone">привет<speaker effect="-">`},
		{"hamster", true, `<speaker effect="hamster"><speaker is_whisper="true">привет<speaker effect="-">`},
	}
	for _, c := range cases {
		if got := StyleSpeech("привет", c.effect, c.whisper); got != c.want {
			t.Errorf("StyleSpeech(%q,%v) = %q, want %q", c.effect, c.whisper, got, c.want)
		}
	}
}

func TestLegacyVoiceStillAccepted(t *testing.T) {
	if !IsKnownVoice("shitova.us") {
		t.Fatal("shitova.us must stay accepted for existing configs")
	}
	for _, name := range KnownVoices {
		if name == "shitova.us" {
			t.Fatal("shitova.us must not be offered any more")
		}
	}
	if !IsSpeechEmotion("evil") || IsSpeechEmotion("angry") || !IsSpeechEffect("hamster") || IsSpeechEffect("x") {
		t.Fatal("emotion/effect whitelists broken")
	}
}

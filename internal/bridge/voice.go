package bridge

import (
	"errors"
	"strings"
)

// VoiceConfig selects the speech synthesis settings for answers the bridge
// generates itself. Local patch (not upstream): the stock Alice voice is used
// when the section is empty, so an empty configuration keeps prior behaviour.
type VoiceConfig struct {
	Voice   string            `json:"voice"`
	Emotion string            `json:"emotion"`
	Speed   float64           `json:"speed"`
	Routes  map[string]string `json:"routes"`
}

// VoiceSettings is the resolved request shape for one answer.
type VoiceSettings struct {
	Voice   string
	Emotion string
	Speed   float64
}

// Resolve returns the voice for a locally spoken answer of the given route.
// Route overrides win; an empty result means "keep the Alice voice".
func (c VoiceConfig) Resolve(route string) VoiceSettings {
	voice := c.Voice
	if route != "" {
		if override, ok := c.Routes[route]; ok {
			voice = override
		}
	}
	voice = strings.TrimSpace(voice)
	if voice == "" {
		return VoiceSettings{}
	}
	settings := VoiceSettings{Voice: voice, Emotion: strings.TrimSpace(c.Emotion)}
	if c.Speed > 0 {
		settings.Speed = c.Speed
	}
	return settings
}

// Enabled reports whether any answer should use a non-default voice. It is
// kept for callers that want to log an effective configuration at startup.
func (c VoiceConfig) Enabled() bool {
	if strings.TrimSpace(c.Voice) != "" {
		return true
	}
	for _, name := range c.Routes {
		if strings.TrimSpace(name) != "" {
			return true
		}
	}
	return false
}

// Validate rejects settings the synthesis backend cannot accept.
func (c VoiceConfig) Validate() error {
	if err := validateVoiceName(c.Voice); err != nil {
		return err
	}
	if emotion := strings.TrimSpace(c.Emotion); emotion != c.Emotion {
		return errors.New("local_voice.emotion must not have surrounding spaces")
	}
	if c.Speed < 0 || (c.Speed > 0 && (c.Speed < 0.1 || c.Speed > 3)) {
		return errors.New("local_voice.speed must be between 0.1 and 3")
	}
	for route, name := range c.Routes {
		route = strings.TrimSpace(route)
		if route == "" {
			return errors.New("local_voice.routes keys must be route names")
		}
		if route == "yandex" {
			return errors.New("local_voice.routes must not override cloud answers")
		}
		if err := validateVoiceName(name); err != nil {
			return err
		}
		if strings.TrimSpace(name) == "" {
			return errors.New("local_voice.routes values must name a voice")
		}
	}
	return nil
}

func validateVoiceName(voice string) error {
	if voice == "" {
		return nil
	}
	if strings.TrimSpace(voice) != voice {
		return errors.New("local_voice.voice must not have surrounding spaces")
	}
	if strings.ContainsAny(voice, " \t") {
		return errors.New("local_voice.voice must be a single name")
	}
	return nil
}

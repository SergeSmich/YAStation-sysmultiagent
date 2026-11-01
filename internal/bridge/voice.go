package bridge

import (
	"errors"
	"strings"
)

// VoiceConfig selects the speech synthesis settings for answers the bridge
// generates itself. Local patch (not upstream): the stock Alice voice is used
// when the section is empty, so an empty configuration keeps prior behaviour.
type VoiceConfig struct {
	Voice      string            `json:"voice,omitempty"`
	Emotion    string            `json:"emotion,omitempty"`
	Speed      float64           `json:"speed,omitempty"`
	Volume     float64           `json:"volume,omitempty"`
	Routes     map[string]string `json:"routes,omitempty"`
	Candidates []string          `json:"candidates,omitempty"`
}

// KnownVoices lists the voices the synthesis dispenser accepted when the list
// was measured against the live upstream (see stage7\voices-available.json).
// Any other name is rejected with "Unsupported voice" and makes every answer
// fall back to the stock Alice voice, so the control plane refuses such settings
// up front instead of letting them reach the synthesizer.
//
// The second probe (2026-11-01, 53 names) added the voices below the first five.
// levitan, zhenya, nick and erkanyavas are accepted but stay silent (timeout),
// like tanya and silaerkan, so they are deliberately absent.
var KnownVoices = []string{
	"jane", "omazh", "oksana", "ermil", "zahar",
	"shitova", "shitova.us", "valtz", "kolya", "kostya", "nastya", "sasha", "alyss",
	"oksana.gpu", "good_oksana", "omazh.gpu", "jane.gpu", "zahar.gpu", "ermil.gpu",
	"robot", "zombie",
}

// IsKnownVoice reports whether the dispenser is expected to accept the name.
func IsKnownVoice(voice string) bool {
	for _, name := range KnownVoices {
		if name == voice {
			return true
		}
	}
	return false
}

// KnownVoicesList renders the accepted names for error messages and logs.
func KnownVoicesList() string { return strings.Join(KnownVoices, ", ") }

// UnknownVoices returns the configured names the dispenser is not expected to
// accept: the primary voice, the retry candidates and the per-route overrides.
func (c VoiceConfig) UnknownVoices() []string {
	var unknown []string
	check := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !IsKnownVoice(name) {
			unknown = append(unknown, name)
		}
	}
	check(c.Voice)
	for _, name := range c.Candidates {
		check(name)
	}
	for _, name := range c.Routes {
		check(name)
	}
	return unknown
}

// VoiceSettings is the resolved request shape for one answer.
type VoiceSettings struct {
	Voice   string
	Emotion string
	Speed   float64
	Volume  float64
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
	if c.Volume > 0 {
		settings.Volume = c.Volume
	}
	return settings
}

// Enabled reports whether any answer should use a non-default voice. It is
// kept for callers that want to log an effective configuration at startup.
func (c VoiceConfig) Enabled() bool {
	if strings.TrimSpace(c.Voice) != "" {
		return true
	}
	if len(c.candidatesFor("")) > 0 {
		return true
	}
	for _, name := range c.Routes {
		if strings.TrimSpace(name) != "" {
			return true
		}
	}
	return false
}

// candidatesFor returns the extra voices to try, in order, when the resolved
// voice of a route is rejected by the synthesizer. The resolved voice itself is
// skipped, so a rejected primary never repeats.
func (c VoiceConfig) candidatesFor(route string) []string {
	primary := c.Resolve(route).Voice
	var names []string
	for _, name := range c.Candidates {
		name = strings.TrimSpace(name)
		if name == "" || name == primary {
			continue
		}
		names = append(names, name)
	}
	return names
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
	if c.Volume < 0 || (c.Volume > 0 && (c.Volume < 0.1 || c.Volume > 3)) {
		return errors.New("local_voice.volume must be between 0.1 and 3")
	}
	for _, name := range c.Candidates {
		if err := validateVoiceName(name); err != nil {
			return err
		}
		if strings.TrimSpace(name) == "" {
			return errors.New("local_voice.candidates must name voices")
		}
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

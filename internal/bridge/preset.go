package bridge

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"quasarbridge/internal/reply"
)

// DefaultPresetTTL bounds how long an armed reaction waits for its microphone
// window. The wake-word daemon opens that window within a second; a longer wait
// would only risk reacting to an unrelated request.
const DefaultPresetTTL = 15 * time.Second

// ErrPresetEmpty reports a start word that carries neither a phrase nor a
// route: it would open the microphone and then do nothing a plain "listen"
// word does not already do.
var ErrPresetEmpty = errors.New("preset has neither text nor route")

// PresetAnswer is what a start word prepared for the next request the station
// sends itself. One struct serves both kinds of word:
//
//   - Text set: the phrase is the question of the turn. It goes through the
//     same path as a spoken one — routing honours the pinned role, the role's
//     model writes the answer — so nothing is read out verbatim.
//   - Text empty: the word only opens the microphone. The route is pinned and
//     the phrase the person says next belongs to it.
//
// The station plays only answers to requests it has sent itself, so a prepared
// reaction is bound to that window, never injected as a turn.
type PresetAnswer struct {
	Text    string
	Route   string
	Expires time.Time
	// Audition, when set, turns the window into a verbatim reading of its phrase
	// in the chosen voice (the panel's "listen" button).
	Audition *audition
}

// ArmAudition prepares a reading of text for the next window the station opens.
// A synthetic request the station never sent is not played (its dialog state
// machine logs "InputStartAck ... no such id was sent" and stays silent), so
// the phrase waits for a real window: the person says the wake word or taps the
// station, and the bridge answers that request with the phrase.
func (n *Native) ArmAudition(text string, voice VoiceSettings, ttl time.Duration) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return ErrPresetEmpty
	}
	if !IsKnownVoice(voice.Voice) {
		return errors.New("unknown voice")
	}
	if voice.Speed < 0 || voice.Speed > 3 || voice.Volume < 0 || voice.Volume > 3 {
		return errors.New("speed and volume must be within 0..3")
	}
	if ttl <= 0 {
		ttl = DefaultPresetTTL
	}
	n.mu.Lock()
	n.preset = &PresetAnswer{Text: text, Expires: time.Now().Add(ttl), Audition: &audition{text: text, voice: voice}}
	n.mu.Unlock()
	return nil
}

// ArmPreset stores a prepared reaction for the next window from the station. A
// newer call replaces an older one: the word that was just detected wins. An
// empty route leaves the route to classification, as a spoken turn would.
func (n *Native) ArmPreset(text, route string, ttl time.Duration) error {
	text = strings.TrimSpace(text)
	route = strings.TrimSpace(route)
	if text == "" && route == "" {
		return ErrPresetEmpty
	}
	if ttl <= 0 {
		ttl = DefaultPresetTTL
	}
	n.mu.Lock()
	if old := n.preset; old != nil && old.Audition != nil && time.Now().Before(old.Expires) {
		// A reading the operator asked for waits for the next window; a start
		// word said meanwhile opens that window and must not discard it.
		n.mu.Unlock()
		return nil
	}
	n.preset = &PresetAnswer{Text: text, Route: route, Expires: time.Now().Add(ttl)}
	n.mu.Unlock()
	return nil
}

// takePreset removes the armed reaction. It is one-shot: a second window must
// not repeat the same words, and an expired preset is dropped instead of used
// long after the word that armed it.
func (n *Native) takePreset() (PresetAnswer, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	preset := n.preset
	n.preset = nil
	if preset == nil || time.Now().After(preset.Expires) {
		return PresetAnswer{}, false
	}
	return *preset, true
}

// applyPreset binds an armed reaction to the request that opened the
// microphone. It reports whether the turn has to be resolved now: a phrase is
// the question of the turn, an empty one waits for the final recognition and
// only pins the route the person's phrase belongs to.
func (n *Native) applyPreset(p *nativeRequest, preset PresetAnswer) bool {
	n.mu.Lock()
	if p.canceled {
		n.mu.Unlock()
		return false
	}
	// A route named by the caller is more specific than the word's default.
	if preset.Route != "" && p.pin == "" {
		p.pin = preset.Route
	}
	effective, text := p.pin, preset.Text
	release := false
	if text != "" {
		// The phrase is typed by the bridge, not heard: the recognizer of this
		// window is listening to silence, so its result must not replace it.
		p.text = text
		p.presetText = true
		p.final = true
		// Nothing is expected from the cloud any more. Releasing the window now
		// keeps it from answering on its own while the role's model is thinking.
		release = p.gated && !p.streaming && effective != "" && effective != "yandex"
		if preset.Audition != nil {
			// Read back as is, never sent to the cloud, microphone stays closed.
			p.audition = preset.Audition
			reply.SuppressListening(p.ctx)
			release = p.gated && !p.streaming
		}
		p.cloudReleased = release
	}
	n.mu.Unlock()
	if text == "" {
		slog.Info("native_preset_listen", "request", requestTag(p.id), "route", effective, "preset_route", preset.Route)
		return false
	}
	slog.Info("native_preset_question", "request", requestTag(p.id), "route", effective, "preset_route", preset.Route, "text_runes", len([]rune(text)))
	if release {
		if err := n.sendCancel(p.ctx, p.id); err != nil {
			slog.Warn("native_cancel_failed", "request", requestTag(p.id))
		}
	}
	return true
}

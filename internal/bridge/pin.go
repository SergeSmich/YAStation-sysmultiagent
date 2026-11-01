package bridge

import (
	"context"
	"errors"
	"strings"

	"github.com/coder/websocket"
)

// pinnedRouteKey marks a context whose turn already has a chosen executor.
type pinnedRouteKey struct{}

// WithPinnedRoute tells Native to resolve the injected turn with the given
// route instead of classifying its text. It is set by the bridge itself when a
// start word carries a preset phrase: the caller named the executor, so a
// classifier guess must not override it.
//
// The marker lives on the caller's context only. Native reads it while
// accepting the frame and keeps it on that single request, so the next spoken
// turn of the same speaker is classified as usual.
func WithPinnedRoute(ctx context.Context, route string) context.Context {
	route = strings.TrimSpace(route)
	if route == "" {
		return ctx
	}
	return context.WithValue(ctx, pinnedRouteKey{}, route)
}

// PinnedRoute reports the route attached to ctx, if any.
func PinnedRoute(ctx context.Context) (string, bool) {
	route, ok := ctx.Value(pinnedRouteKey{}).(string)
	return route, ok && route != ""
}

// auditionKey marks a context whose turn only reads a phrase aloud.
type auditionKey struct{}

// audition is a phrase the operator wants to hear in a chosen voice.
type audition struct {
	text  string
	voice VoiceSettings
}

// Audition speaks text on the station in the given voice. The phrase travels
// the same path as a spoken turn (a synthetic TextInput the station plays the
// answer of), but it never reaches the cloud or a model: the bridge answers it
// itself, verbatim, and does not reopen the microphone afterwards.
func (n *Native) Audition(ctx context.Context, text string, voice VoiceSettings) (string, error) {
	if !IsKnownVoice(voice.Voice) {
		return "", errors.New("unknown voice")
	}
	if voice.Speed < 0 || voice.Speed > 3 || voice.Volume < 0 || voice.Volume > 3 {
		return "", errors.New("speed and volume must be within 0..3")
	}
	frame, id, err := SyntheticTextInput(text)
	if err != nil {
		return "", err
	}
	ctx = context.WithValue(ctx, auditionKey{}, &audition{text: strings.TrimSpace(text), voice: voice})
	if err := n.Client(ctx, websocket.MessageBinary, frame); err != nil {
		return "", err
	}
	return id, nil
}

func auditionOf(ctx context.Context) *audition {
	a, _ := ctx.Value(auditionKey{}).(*audition)
	return a
}

package bridge

import (
	"context"
	"log/slog"
	"time"

	"quasarbridge/internal/dialogue"
)

// DefaultHoldTTL is how long a role keeps the dialogue after its own answer.
// The station reopens the microphone after every answer, so the next phrase of
// the same speaker belongs to the same role until the person falls silent or
// ends the conversation; the bound matches the default dialogue lifetime.
const DefaultHoldTTL = 5 * time.Minute

// heldRoute is the role a speaker's conversation is bound to. It is not a
// prepared answer: nothing is said or asked in advance, the role is only kept
// for the windows that follow its answer.
type heldRoute struct {
	route   string
	expires time.Time
}

// holdRole keeps the role for the next windows of the same speaker. Only a role
// route is held: a plain model or cloud answer does not own a conversation.
func (n *Native) holdRole(ctx context.Context, route string) {
	if !isAgentRoute(route) {
		return
	}
	ttl := n.HoldTTL
	if ttl <= 0 {
		ttl = DefaultHoldTTL
	}
	key := dialogue.SpeakerKey(ctx)
	if key == "" {
		return
	}
	n.mu.Lock()
	if n.held == nil {
		n.held = map[string]heldRoute{}
	}
	n.held[key] = heldRoute{route: route, expires: time.Now().Add(ttl)}
	n.mu.Unlock()
	slog.Info("native_role_held", "route", route, "ttl_s", int(ttl.Seconds()))
}

// heldRouteOf returns the role that still owns the speaker's conversation. An
// expired hold is dropped on the way: a person who came back much later starts
// a new conversation instead of continuing a forgotten one.
func (n *Native) heldRouteOf(ctx context.Context) string {
	key := dialogue.SpeakerKey(ctx)
	if key == "" {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	held, ok := n.held[key]
	if !ok {
		return ""
	}
	if !time.Now().Before(held.expires) {
		delete(n.held, key)
		n.notifyReleased(held.route, "ttl")
		return ""
	}
	return held.route
}

// notifyReleased tells the bridge that a role no longer owns a conversation, so
// whatever it was waiting on (permission requests, an approved plan) can end.
// It runs on its own goroutine: the caller holds the native lock.
func (n *Native) notifyReleased(route, reason string) {
	if fn := n.OnRoleReleased; fn != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("role_released_hook_panic", "panic", r)
				}
			}()
			fn(route, reason)
		}()
	}
}

// releaseRole ends the held conversation of this speaker and reports the route
// that was held, so the caller can log what ended.
func (n *Native) releaseRole(ctx context.Context) (string, bool) {
	return n.releaseRoleWhy(ctx, "release")
}

// releaseRoleWhy is releaseRole with the reason handed to OnRoleReleased: an
// explicit end ("release") closes open requests; silence ("silence") does not,
// because the person may simply be deciding.
func (n *Native) releaseRoleWhy(ctx context.Context, why string) (string, bool) {
	key := dialogue.SpeakerKey(ctx)
	if key == "" {
		return "", false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	held, ok := n.held[key]
	delete(n.held, key)
	if !ok || !time.Now().Before(held.expires) {
		if ok {
			n.notifyReleased(held.route, "ttl")
		}
		return "", false
	}
	n.notifyReleased(held.route, why)
	return held.route, true
}

// endsDialogue reports a phrase that closes the conversation with the role on
// purpose. The station reopens the microphone after every answer, so without
// such a phrase the only way out would be silence — and a person who is done
// usually says one of these.
//
// "хватит" and "стоп" are the playback commands, so they are matched only here,
// where the held role is dropped; routing of the turn itself stays untouched and
// keeps stopping the music.
func endsDialogue(text string) bool {
	if dialogue.IsReset(text) {
		return true
	}
	switch dialogue.Normalize(text) {
	case "стоп", "хватит", "замолчи", "закончим", "до свидания", "пока":
		return true
	}
	return false
}

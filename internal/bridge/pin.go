package bridge

import (
	"context"
	"strings"
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

package main

import (
	"errors"
	"reflect"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
)

func reloadRouting(path string, initial config, engine *bridge.Engine, logs ...*journal.Writer) error {
	next, err := readConfig(path)
	if err != nil {
		return err
	}
	// Listener, credentials, backend clients and warm-up jobs remain bound to the
	// original deployment. Do not silently pretend those settings were reloaded.
	a, b := initial, next
	a.Routing = bridge.RoutingConfig{}
	b.Routing = bridge.RoutingConfig{}
	// The local voice is hot-applied by the caller, so a voice tuning must not
	// force a restart through this comparison.
	a.LocalVoice = bridge.VoiceConfig{}
	b.LocalVoice = bridge.VoiceConfig{}
	if len(logs) > 0 {
		a.ConversationLog = journal.Config{}
		b.ConversationLog = journal.Config{}
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("non-routing settings changed; restart required")
	}
	if len(logs) > 0 {
		if err := logs[0].Configure(next.ConversationLog); err != nil {
			return err
		}
	}
	return engine.SetRouting(next.Routing)
}

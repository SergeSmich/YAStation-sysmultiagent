package main

import (
	"errors"
	"reflect"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/journal"
)

// reloadHooks are the sections of the configuration the running process can
// swap without a restart. Everything else still needs one: the listener, the
// credentials and the backend clients stay bound to the original deployment.
type reloadHooks struct {
	applyAgents func(config) error
	applyWake   func(config) error
	logs        *journal.Writer
}

func reloadRouting(path string, initial config, engine *bridge.Engine, hooks reloadHooks) error {
	next, err := readConfig(path)
	if err != nil {
		return err
	}
	// Do not silently pretend a non-reloadable setting was applied.
	a, b := initial, next
	a.Routing = bridge.RoutingConfig{}
	b.Routing = bridge.RoutingConfig{}
	// The local voice is hot-applied by the caller, so a voice tuning must not
	// force a restart through this comparison.
	a.LocalVoice = bridge.VoiceConfig{}
	b.LocalVoice = bridge.VoiceConfig{}
	// Roles and start words are hot-applied for the same reason.
	a.Agents, b.Agents = nil, nil
	a.WakeWords, b.WakeWords = wakeWordsConfig{}, wakeWordsConfig{}
	if hooks.logs != nil {
		a.ConversationLog = journal.Config{}
		b.ConversationLog = journal.Config{}
	}
	if !reflect.DeepEqual(a, b) {
		return errors.New("non-routing settings changed; restart required")
	}
	if hooks.logs != nil {
		if err := hooks.logs.Configure(next.ConversationLog); err != nil {
			return err
		}
	}
	if err := engine.SetRouting(next.Routing); err != nil {
		return err
	}
	if hooks.applyAgents != nil {
		if err := hooks.applyAgents(next); err != nil {
			return err
		}
	}
	if hooks.applyWake != nil {
		return hooks.applyWake(next)
	}
	return nil
}

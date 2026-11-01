package bridge

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// AgentRoutePrefix marks a routing target that a named agent answers with its
// own model client and system prompt. The same string is the key in
// VoiceConfig.Routes, so one role can speak with its own voice.
const AgentRoutePrefix = "agent:"

// MaxAgentPromptRunes bounds a role prompt: it travels with every turn.
const MaxAgentPromptRunes = 8000

// Agent is a named answering role. The dialogue history and the local voice
// machinery stay shared with the default assistant: only the model client and
// the system prompt differ, so a role cannot change behaviour by accident.
type Agent struct {
	Name     string
	Answerer Completer
	Prompt   string
}

// ValidAgentName keeps route strings, VoiceConfig keys and journal entries
// parseable: the name travels through all three. Latin and Cyrillic lowercase
// letters, digits, '-' and '_' are accepted (not first), up to 32 characters.
func ValidAgentName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 32 {
		return false
	}
	for i, r := range []rune(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r >= 'а' && r <= 'я', r == 'ё':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}
// AgentRoute builds the route string for a role.
func AgentRoute(name string) string { return AgentRoutePrefix + name }

// ParseAgentRoute reports whether the route names an agent and returns its name.
func ParseAgentRoute(route string) (string, bool) {
	name, ok := strings.CutPrefix(route, AgentRoutePrefix)
	if !ok || !ValidAgentName(name) {
		return "", false
	}
	return name, true
}

func isAgentRoute(route string) bool {
	_, ok := ParseAgentRoute(route)
	return ok
}

// hasAgentPrefix reports a route the author meant as a role, valid or not, so a
// malformed name is reported instead of silently answered by another backend.
func hasAgentPrefix(route string) bool { return strings.HasPrefix(route, AgentRoutePrefix) }

// SetAgents replaces the registry. An empty registry means no roles: an agent
// route then fails loudly instead of answering with another model.
func (e *Engine) SetAgents(agents map[string]*Agent) error {
	for name, agent := range agents {
		if !ValidAgentName(name) {
			return fmt.Errorf("invalid agent name %q", name)
		}
		if agent == nil || agent.Answerer == nil {
			return fmt.Errorf("agent %q has no model client", name)
		}
		if len([]rune(agent.Prompt)) > MaxAgentPromptRunes {
			return fmt.Errorf("agent %q prompt is too long", name)
		}
	}
	if len(agents) == 0 {
		agents = nil
	}
	e.agents.Store(&agents)
	return nil
}

// Agent resolves a configured role by name.
func (e *Engine) Agent(name string) (*Agent, bool) {
	registry := e.agents.Load()
	if registry == nil {
		return nil, false
	}
	agent, ok := (*registry)[name]
	if !ok || agent == nil {
		return nil, false
	}
	return agent, true
}

// AgentNames lists the configured roles in a stable order.
func (e *Engine) AgentNames() []string {
	registry := e.agents.Load()
	if registry == nil {
		return nil
	}
	names := make([]string, 0, len(*registry))
	for name := range *registry {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// answerAs routes one turn to a named role.
func (e *Engine) answerAs(ctx context.Context, route, text, name string) (Resolution, error) {
	agent, ok := e.Agent(name)
	if !ok {
		return Resolution{Route: route}, fmt.Errorf("agent %q is not configured", name)
	}
	return e.answer(ctx, route, text, agent.Answerer, agent.Prompt)
}

var errNoAnswerer = errors.New("LLM is not configured")

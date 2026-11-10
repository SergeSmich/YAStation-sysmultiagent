package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"quasarbridge/internal/llm"
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
	// Description is shown to the other roles; Peers tells the roster that this
	// role may call them.
	Description string
	Peers       bool
	// Sysadmin marks a role that carries the sysadmin toolset; text channels
	// (Telegram) refuse to talk to a role without it.
	Sysadmin bool
	// PromptExtra, when set, is appended to Prompt on every turn, so an
	// editable block (the sysadmin charter) takes effect without a reload.
	PromptExtra func() string
	// Callable marks a role a person may address by name. A role that is not
	// callable stays silent even if its name is spoken.
	Callable bool
	// Aliases are extra spoken names, already validated by the writer.
	Aliases []string
	// Greeting is what a bare call answers, without asking any model.
	Greeting string
}

// MaxAgentGreetingRunes bounds the spoken greeting: it is one short sentence.
const MaxAgentGreetingRunes = 300

// DefaultRoleGreeting answers a bare call of a role that has no greeting of its
// own. It is deliberately about presence, not about the model.
const DefaultRoleGreeting = "Я здесь. Слушаю."

// FullPrompt is the role prompt with its live extra block.
func (a *Agent) FullPrompt() string {
	if a.PromptExtra == nil {
		return a.Prompt
	}
	return a.Prompt + a.PromptExtra()
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
		// The built prompt may carry a bridge-added block (sysadmin rules) on top
		// of the operator's text, which config validation limits separately.
		if len([]rune(agent.Prompt)) > MaxAgentPromptRunes+2000 {
			return fmt.Errorf("agent %q prompt is too long", name)
		}
		if !ValidAgentDescription(agent.Description) {
			return fmt.Errorf("agent %q description is too long", name)
		}
		if utf8.RuneCountInString(agent.Greeting) > MaxAgentGreetingRunes {
			return fmt.Errorf("agent %q greeting is too long", name)
		}
		if len(agent.Aliases) > MaxRoleAliases {
			return fmt.Errorf("agent %q has more than %d aliases", name, MaxRoleAliases)
		}
	}
	if err := validateRoleNames(agents); err != nil {
		return err
	}
	if len(agents) == 0 {
		agents = nil
	}
	e.agents.Store(&agents)
	e.roles.Store(roleDirectoryOf(agents))
	return nil
}

// validateRoleNames keeps the spoken names unique across the registry: a name and
// an alias are the same thing to a person, so one word may not mean two roles.
func validateRoleNames(agents map[string]*Agent) error {
	owner := make(map[string]string, len(agents))
	for name, agent := range agents {
		if agent == nil {
			continue
		}
		for _, word := range append([]string{name}, agent.Aliases...) {
			if !ValidAgentName(word) {
				return fmt.Errorf("agent %q: alias %q is not a valid name", name, word)
			}
			if other, taken := owner[word]; taken && other != name {
				return fmt.Errorf("agent %q: name or alias %q is already used by %q", name, word, other)
			}
			owner[word] = name
		}
	}
	return nil
}

// roleDirectoryOf builds the spoken-name directory from the callable roles only.
func roleDirectoryOf(agents map[string]*Agent) *RoleDirectory {
	entries := make([]RoleDirEntry, 0, len(agents))
	for name, agent := range agents {
		if agent == nil || !agent.Callable {
			continue
		}
		entries = append(entries, RoleDirEntry{Role: name, Aliases: agent.Aliases})
	}
	if len(entries) == 0 {
		return nil
	}
	dir := BuildRoleDirectory(entries)
	return &dir
}

// RoleCallConfig is the operator switch of the spoken call. Off by default: an
// upgrade must not start answering to names that were never announced.
type RoleCallConfig struct {
	Enabled bool `json:"enabled"`
	DryRun  bool `json:"dry_run"`
}

// SetRoleCall publishes the switch; the zero value is "off".
func (e *Engine) SetRoleCall(cfg RoleCallConfig) {
	e.roleCall.Store(&cfg)
}

func (e *Engine) roleCallSettings() RoleCallConfig {
	if p := e.roleCall.Load(); p != nil {
		return *p
	}
	return RoleCallConfig{}
}

// MatchRoleCall resolves a phrase against the callable roles without acting on it:
// the phrase probe and the tests need to know what the directory holds, while the
// decision itself goes through spokenRoleCall and the operator switch.
func (e *Engine) MatchRoleCall(text string) (RoleCall, bool) {
	dir := e.roles.Load()
	if dir == nil || dir.empty() {
		return RoleCall{}, false
	}
	return ParseRoleCall(text, *dir)
}

// spokenRoleCall resolves a call and honours the operator switch. A dry run only
// writes its journal line: the phrase falls through to the normal routing, so the
// operator can hear that the resolver fires without the station reacting to names.
func (e *Engine) spokenRoleCall(text string) (RoleCall, bool) {
	cfg := e.roleCallSettings()
	if !cfg.Enabled {
		return RoleCall{}, false
	}
	call, ok := e.MatchRoleCall(text)
	if !ok {
		return RoleCall{}, false
	}
	if cfg.DryRun {
		slog.Info("role_call", "role", call.Role, "kind", call.Kind, "dry_run", true)
		return RoleCall{}, false
	}
	return call, true
}

// RoleGreeting is what a bare call answers: the role's own line, or the default.
func (e *Engine) RoleGreeting(name string) string {
	if agent, ok := e.Agent(name); ok && strings.TrimSpace(agent.Greeting) != "" {
		return agent.Greeting
	}
	return DefaultRoleGreeting
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
	prompt := agent.FullPrompt()
	if roster := e.roster(agent); roster != "" {
		if strings.TrimSpace(prompt) == "" {
			prompt = defaultAnswerPrompt
		}
		prompt += "\n" + roster
	}
	var slot *handoffSlot
	if agent.Peers {
		ctx, slot = withHandoff(ctx)
	}
	res, err := e.answer(ctx, route, text, agent.Answerer, prompt)
	if err == nil && slot != nil {
		if target := slot.get(); target != "" && target != name {
			// The registry may have been reloaded during the turn.
			if _, exists := e.Agent(target); exists {
				res.Handoff = AgentRoute(target)
			}
		}
	}
	return res, err
}

// textChatTail replaces the speakable-answer constraint for a text channel.
const textChatTail = "\nОтвет уйдёт текстом в чат (не вслух): можно короткие списки и команды, без лишних вступлений."

// AskAgent runs one turn of a named role over a text channel (Telegram). It
// deliberately does not touch the voice dialogue store: a chat message must
// neither inherit the room's conversation nor cancel a spoken turn in flight.
// The caller keeps its own history and passes it in.
func (e *Engine) AskAgent(ctx context.Context, name, text string, history []llm.Message) (string, error) {
	agent, ok := e.Agent(name)
	if !ok {
		return "", fmt.Errorf("agent %q is not configured", name)
	}
	if agent.Answerer == nil {
		return "", errNoAnswerer
	}
	prompt := agent.FullPrompt()
	if strings.TrimSpace(prompt) == "" {
		prompt = "Ты помощник пользователя. Отвечай по-русски кратко и по существу."
	}
	if roster := e.roster(agent); roster != "" {
		prompt += "\n" + roster
	}
	messages := []llm.Message{{Role: "system", Content: prompt + textChatTail}}
	messages = append(messages, history...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	answer, err := agent.Answerer.Complete(ctx, messages, false)
	if err != nil {
		return "", err
	}
	return stripAnswerLabel(answer), nil
}

var errNoAnswerer = errors.New("LLM is not configured")

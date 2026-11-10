package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"quasarbridge/internal/dialogue"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/playback"
	"quasarbridge/internal/reply"
	"quasarbridge/internal/webaddress"
)

type Completer interface {
	Complete(context.Context, []llm.Message, bool) (string, error)
}
type HomeHandler interface {
	Handle(context.Context, string) (string, error)
}
type PlaybackController interface {
	Active(context.Context) bool
	Control(context.Context, string) (string, error)
}
type MusicStarter interface {
	CanStartMusic(string) bool
	StartMusic(context.Context, string) (string, error)
}

// Engine answers one station. LLM is the deployment client from llm_*: it is set
// once at start and stays the fallback for an empty main_agent, the home planner
// and Guard. Guard vets untrusted tool payloads and is deliberately not the role
// that answers the person, so a smaller role model cannot weaken a safety check.
type Engine struct {
	AirPlay      AirPlayController
	LLM          Completer
	Guard        Completer
	Assistant    Completer
	Home         HomeHandler
	Playback     PlaybackController
	Music        MusicStarter
	Dialogue     *dialogue.Store
	dialogueOnce sync.Once
	routing      atomic.Pointer[RoutingPolicy]
	agents       atomic.Pointer[map[string]*Agent]
	main         atomic.Pointer[mainClients]
}

// mainClients is the answerer of route llm, replaced as a whole by SetMain. A
// request therefore reads either the previous pair or the new one, never half of
// each; the pair is immutable once stored.
type mainClients struct {
	llm      Completer
	answerer Completer
	prompt   string
}

// SetMain replaces the llm answerer without a restart. llm thinks (classifier and
// replay), answerer speaks to the person and may carry the assistant tools, and
// prompt replaces the shared assistant prompt when non-empty. A nil answerer
// means the same client answers.
func (e *Engine) SetMain(llm, answerer Completer, prompt string) {
	if answerer == nil {
		answerer = llm
	}
	if llm == nil && answerer == nil {
		e.main.Store(nil)
		return
	}
	e.main.Store(&mainClients{llm: llm, answerer: answerer, prompt: prompt})
}

// llmClient is the client that thinks: classifier, replay and retry.
func (e *Engine) llmClient() Completer {
	if pair := e.main.Load(); pair != nil && pair.llm != nil {
		return pair.llm
	}
	return e.LLM
}

// answerer is who answers the person: the main pair, then the tool-carrying
// assistant, then the deployment client.
func (e *Engine) answerer() Completer {
	if pair := e.main.Load(); pair != nil && pair.answerer != nil {
		return pair.answerer
	}
	if e.Assistant != nil {
		return e.Assistant
	}
	return e.LLM
}

// mainPrompt is the role prompt of the main answerer; empty keeps the shared
// assistant prompt.
func (e *Engine) mainPrompt() string {
	if pair := e.main.Load(); pair != nil {
		return pair.prompt
	}
	return ""
}

// guard is the model that vets tool payloads: the deployment client, whatever
// role answers the person.
func (e *Engine) guard() Completer {
	if e.Guard != nil {
		return e.Guard
	}
	return e.LLM
}

func (e *Engine) dialogue() *dialogue.Store {
	e.dialogueOnce.Do(func() {
		if e.Dialogue == nil {
			e.Dialogue = dialogue.New(dialogue.DefaultConfig())
		}
	})
	return e.Dialogue
}

// Prepare reserves this speaker's request before ASR/classification can finish late.
func (e *Engine) Prepare(ctx context.Context) context.Context {
	if ctx.Err() != nil {
		return ctx
	}
	ctx, _, _ = e.dialogue().Start(ctx)
	return ctx
}

func (e *Engine) Resolve(ctx context.Context, text string) (Resolution, error) {
	return e.ResolveDecision(ctx, text, e.Decide(ctx, text))
}
func (e *Engine) ResolveDecision(ctx context.Context, text string, decision RouteDecision) (Resolution, error) {
	route := decision.Route
	if decision.prepared {
		ctx = e.dialogue().Use(ctx, decision.ticket)
	} else {
		ctx = e.Prepare(ctx)
	}
	_, _, ticket := e.dialogue().Start(ctx)
	if ctx.Err() != nil {
		return Resolution{Route: route}, ctx.Err()
	}

	if !e.dialogue().Current(ctx, ticket) {
		return Resolution{Route: route}, context.Canceled
	}
	if decision.Source == "dialogue_retry" {
		return e.retryQuestion(ctx, ticket)
	}
	if decision.Source == "dialogue_reset" {
		e.dialogue().ClearReplay(ctx, ticket)
		if !e.dialogue().Clear(ctx, ticket) {
			return Resolution{Route: route}, context.Canceled
		}
		return Resolution{Route: "llm", Answer: "Начинаем новый разговор."}, nil
	}
	if route != "llm" && !isAgentRoute(route) && !e.dialogue().Clear(ctx, ticket) {
		return Resolution{Route: route}, context.Canceled
	}
	if name, ok := ParseAgentRoute(route); ok {
		// A role keeps the dialogue history: it is the same assistant with a
		// different model and prompt, not a one-shot command.
		return e.answerAs(ctx, route, text, name)
	}
	if hasAgentPrefix(route) {
		// A misspelled role must not fall through to the cloud: the caller asked
		// for a configured profile that does not exist.
		return Resolution{Route: route}, fmt.Errorf("invalid agent route %q", route)
	}
	result := Resolution{Route: route}
	switch route {
	case "airplay":
		if e.AirPlay == nil {
			return Resolution{Route: "yandex"}, nil
		}
		action := airPlayCommand(text)
		if decision.Source == "airplay_media" {
			controller, ok := e.AirPlay.(AirPlayMediaController)
			if !ok || !controller.Active(ctx) {
				return Resolution{Route: "yandex"}, nil
			}
			action = airPlayMediaCommand(text)
			if action == "" {
				return result, errors.New("unknown AirPlay media control")
			}
			answer, err := controller.ControlMedia(ctx, action)
			result.Answer = answer
			return result, err
		}
		if action == "" {
			return result, errors.New("unknown AirPlay control")
		}
		answer, err := e.AirPlay.Control(ctx, action)
		if err == nil && action == "start" {
			reply.SuppressListening(ctx)
		}
		result.Answer = answer
		return result, err
	case "music":
		if e.Music == nil {
			return result, errors.New("music playback is unavailable")
		}
		answer, err := e.Music.StartMusic(ctx, text)
		if !e.dialogue().Current(ctx, ticket) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "playback":
		if e.Playback == nil {
			return Resolution{Route: "yandex"}, nil
		}
		action := playback.Command(text)
		if action == "" {
			return result, errors.New("unknown playback control")
		}
		answer, err := e.Playback.Control(ctx, action)
		if !e.dialogue().Current(ctx, ticket) {
			return result, context.Canceled
		}
		result.Answer = answer
		return result, err
	case "home":
		if e.Home == nil {
			return Resolution{Route: "yandex"}, nil
		}
		answer, err := e.Home.Handle(ctx, text)
		result.Answer = answer
		return result, err
	case "llm":
		return e.Fallback(ctx, text)
	default:
		return Resolution{Route: "yandex"}, nil
	}
}

const defaultAnswerPrompt = "Ты голосовой помощник пользователя. Отвечай по-русски, кратко и по существу, обычно до трёх предложений. Ответ будет прочитан вслух: без Markdown и служебных рассуждений. Отвечай сразу по существу, без вступительных пометок и без названия модели. Обороты «спроси нейросеть», «спроси свою модель», «спроси у нейросети» — это обращение к тебе самому: не ищи для них инструмент, пропусти вводную и отвечай на суть; если после вводной вопроса нет, коротко попроси его задать. Выполняй внешние действия только доступными инструментами по явному запросу пользователя. Не заявляй о выполнении действий без успешного результата инструмента; если инструмента нет, прямо скажи об этом. Для Home Assistant есть отдельный обработчик умного дома."

// answerLabel is the spoken prefix earlier builds asked the model to prepend.
// The operator asked for plain answers, but a role prompt or a configuration
// written by hand may still carry the instruction, so the label is cut from
// whatever the model actually returns.
const answerLabel = "Нейросеть:"

func stripAnswerLabel(answer string) string {
	trimmed := strings.TrimSpace(answer)
	original := trimmed
	for len(trimmed) >= len(answerLabel) && strings.EqualFold(trimmed[:len(answerLabel)], answerLabel) {
		trimmed = strings.TrimSpace(trimmed[len(answerLabel):])
	}
	if trimmed == "" {
		return original
	}
	return trimmed
}

// roleTail keeps a role prompt compatible with a spoken answer: the author of a
// role describes the task, not the output format.
const roleTail = "\nОтвет будет прочитан вслух: без Markdown, списков и служебных рассуждений, обычно до трёх предложений."

func answerPrompt(role string) string {
	if strings.TrimSpace(role) == "" {
		return defaultAnswerPrompt
	}
	return role + roleTail
}

func (e *Engine) Fallback(ctx context.Context, text string) (Resolution, error) {
	answerer := e.answerer()
	if answerer == nil {
		return Resolution{Route: "llm"}, errNoAnswerer
	}
	return e.answer(ctx, "llm", text, answerer, e.mainPrompt())
}

// answer runs one local turn with the given model client. An empty prompt means
// the shared assistant prompt; a role prompt replaces it and keeps only the
// constraints that make an answer speakable.
func (e *Engine) answer(ctx context.Context, route, text string, answerer Completer, prompt string) (Resolution, error) {
	if strings.Contains(strings.ToLower(text), "сайт") || webaddress.MusicRequest(text) {
		text = webaddress.Normalize(text)
	}
	r := Resolution{Route: route}
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	messages := []llm.Message{{Role: "system", Content: answerPrompt(prompt)}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	private := snapshot.PrivateHistory
	var answer string
	var err error
	if contextual, ok := answerer.(interface {
		CompleteDialogue(context.Context, []llm.Message, bool) (string, bool, error)
	}); ok {
		answer, private, err = contextual.CompleteDialogue(ctx, messages, private)
	} else {
		answer, err = answerer.Complete(ctx, messages, false)
	}
	if !e.dialogue().Current(ctx, ticket) {
		return r, context.Canceled
	}
	if err == nil {
		answer = stripAnswerLabel(answer)
		if !e.dialogue().Commit(ctx, ticket, text, answer, private) {
			return r, context.Canceled
		}
	}
	r.Answer = answer
	return r, err
}
func containsOne(text string, needles []string) bool {
	for _, s := range needles {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}
func (e *Engine) SetRouting(cfg RoutingConfig) error {
	p, err := CompileRouting(cfg)
	if err != nil {
		return err
	}
	e.routing.Store(p)
	return nil
}
func (e *Engine) policy() *RoutingPolicy {
	if p := e.routing.Load(); p != nil {
		return p
	}
	return builtInRouting
}

type RoutingStatus struct {
	Revision    string
	ActiveRules int
}

func (e *Engine) RoutingStatus() RoutingStatus {
	p := e.policy()
	return RoutingStatus{Revision: p.revision, ActiveRules: len(p.rules)}
}
func (e *Engine) IsRefusal(text string) bool { return e.policy().isRefusal(text) }
func IsRefusal(text string) bool             { return builtInRouting.isRefusal(text) }
func (e *Engine) Decide(ctx context.Context, text string) (decision RouteDecision) {
	ctx, snapshot, ticket := e.dialogue().Start(ctx)
	defer func() {
		// Choose the cloud path before Native can start local progress/TTS.
		if decision.Route == "home" && e.Home == nil {
			decision.Route = "yandex"
			decision.Source = "home_disabled"
		}
		decision.ticket = ticket
		decision.prepared = true
	}()
	p := e.policy()
	if e.AirPlay != nil && airPlayCommand(text) != "" {
		return RouteDecision{Route: "airplay", Source: "airplay"}
	}
	if isRetryCommand(text) {
		return RouteDecision{Route: "llm", Source: "dialogue_retry"}
	}
	if dialogue.IsReset(text) {
		return RouteDecision{Route: "llm", Source: "dialogue_reset"}
	}
	if controller, ok := e.AirPlay.(AirPlayMediaController); ok && airPlayMediaCommand(text) != "" && controller.Active(ctx) {
		return RouteDecision{Route: "airplay", Source: "airplay_media"}
	}
	if e.Playback != nil && playback.Command(text) != "" && e.Playback.Active(ctx) {
		return RouteDecision{Route: "playback", Source: "playback"}
	}
	if d, ok := p.match(text); ok {
		if d.Route == "llm" && e.Music != nil && e.Music.CanStartMusic(text) {
			d.Route = "music"
		}
		return d
	}
	if len(snapshot.Messages) > 0 && dialogue.IsFollowup(text) {
		return RouteDecision{Route: "llm", Source: "dialogue"}
	}
	if p.defaultRoute != "classify" {
		return RouteDecision{Route: p.defaultRoute, Source: "default"}
	}
	failed := RouteDecision{Route: p.classifier.OnError, Source: "classifier_error"}
	client := e.llmClient()
	if client == nil {
		return failed
	}
	classCtx, cancel := context.WithTimeout(ctx, time.Duration(p.classifier.TimeoutMS)*time.Millisecond)
	defer cancel()
	messages := []llm.Message{{Role: "system", Content: p.classifier.Prompt}}
	messages = append(messages, snapshot.Messages...)
	messages = append(messages, llm.Message{Role: "user", Content: text})
	answer, err := client.Complete(classCtx, messages, true)
	if err != nil {
		return failed
	}
	var choice struct {
		Route string `json:"route"`
	}
	if json.Unmarshal([]byte(answer), &choice) != nil || !directRoute(choice.Route) {
		return failed
	}
	return RouteDecision{Route: choice.Route, Source: "classifier"}
}

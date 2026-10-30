package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"quasarbridge/internal/bridge"
)

// The reactions a start word can have. The daemon only reports the hit; the
// bridge decides what happens, so a word and its reaction can be changed in the
// configuration without rebuilding the daemon.
const (
	wakeListen  = "listen"
	wakeAsk     = "ask"
	wakeCommand = "command"

	// maxWakeWordRunes bounds one reported word: the detector reports a short
	// name, never a phrase.
	maxWakeWordRunes = 64

	// wakeActionTimeout bounds one silent answer: the injected turn has to reach
	// the native session and be accepted, not to be spoken in full.
	wakeActionTimeout = 30 * time.Second
)

// presetArmer is the part of the native session a start word needs to prepare a
// spoken answer. It is separate from textInjector so a test double that only
// injects text still satisfies the interface.
type presetArmer interface {
	ArmPreset(text, route string, ttl time.Duration) error
}

// The failures the shared action path reports, so the HTTP handler can answer
// with the same status codes as before the daemon poller started using it.
var (
	errWakeNoSession = errors.New("no upstream session")
	errWakeInject    = errors.New("wake inject")
	errWakeNoPreset  = errors.New("wake preset unsupported")
)

// wakePunctuation is trimmed from a reported word: a detector sometimes reports
// its own separator together with the word.
const wakePunctuation = ".,!?;:()[]\"'«»"

func normalizeWakeWord(word string) string {
	word = strings.ToLower(strings.TrimSpace(word))
	return strings.TrimSpace(strings.Trim(word, wakePunctuation))
}

// wakeTable is the compiled start-word table. A reload replaces the whole
// table, so one request always sees a consistent set of words.
type wakeTable struct {
	dryRun bool
	words  map[string]wakeWordConfig
}

func newWakeTable(cfg wakeWordsConfig) *wakeTable {
	t := &wakeTable{dryRun: cfg.DryRun, words: make(map[string]wakeWordConfig, len(cfg.Words))}
	for _, w := range cfg.Words {
		t.words[normalizeWakeWord(w.Word)] = w
	}
	return t
}

func (t *wakeTable) lookup(word string) (wakeWordConfig, bool) {
	if t == nil {
		return wakeWordConfig{}, false
	}
	// Tolerate a caller that did not normalize: the table is keyed by normalized
	// words, and the daemon reports whatever it detected.
	w, ok := t.words[normalizeWakeWord(word)]
	return w, ok
}

func (t *wakeTable) size() int {
	if t == nil {
		return 0
	}
	return len(t.words)
}

// wakeState holds the table the request path reads and SIGHUP swaps.
type wakeState struct{ table atomic.Pointer[wakeTable] }

func (s *wakeState) load() *wakeTable { return s.table.Load() }

// set stores the words of a configuration. An empty table is stored as well:
// a reload that removes every word must really stop acting on them.
func (s *wakeState) set(cfg wakeWordsConfig) { s.table.Store(newWakeTable(cfg)) }

// status describes the table for /healthz and the panel. The words themselves
// are not published there: they are user configuration, not bridge telemetry.
func (s *wakeState) status() map[string]any {
	table := s.load()
	return map[string]any{"words": table.size(), "dry_run": table != nil && table.dryRun}
}

// wakeResult is what the bridge did with a reported word. The HTTP handler
// answers with it, and the daemon poller uses Listen to decide whether the
// microphone has to be opened explicitly.
type wakeResult struct {
	Known   bool
	Action  string
	Agent   string
	DryRun  bool
	Route   string
	Request string
	// Listen tells the daemon to open the microphone: an unknown word, a
	// "listen" word, a dry run and an "ask" word (whose prepared answer needs a
	// real request to be played) all want that window.
	Listen bool
}

// payload is the response body the wake-word daemon expects.
func (r wakeResult) payload() map[string]any {
	body := map[string]any{"ok": true, "known": r.Known, "action": r.Action, "listen": r.Listen}
	if r.DryRun {
		body["dry_run"] = true
	}
	if r.Request != "" {
		body["request"] = r.Request
	}
	return body
}

// runWakeAction performs the reaction configured for a word. It is shared by
// the HTTP callback and the daemon poller so both obey the same table.
func runWakeAction(ctx context.Context, words *wakeTable, word string, native func() textInjector) (wakeResult, error) {
	dryRun := words != nil && words.dryRun
	entry, known := words.lookup(word)
	if !known {
		// An unknown word is not an error: the daemon may know a word this
		// bridge was never told about. Listening keeps the stock behaviour.
		slog.Info("wake_hit", "word", word, "known", false, "action", wakeListen, "dry_run", dryRun)
		return wakeResult{Known: false, Action: wakeListen, Listen: true}, nil
	}
	slog.Info("wake_hit", "word", word, "known", true, "action", entry.Action, "agent", entry.Agent, "dry_run", dryRun)
	if entry.Action == wakeListen {
		return wakeResult{Known: true, Action: wakeListen, Listen: true}, nil
	}
	if dryRun {
		// The word is detected and logged, but the silent action is skipped:
		// the station simply listens, so a new word can be tried safely.
		slog.Info("wake_skipped", "word", word, "action", entry.Action, "reason", "dry_run")
		return wakeResult{Known: true, Action: wakeListen, Listen: true, DryRun: true}, nil
	}
	// A "command" text is injected without a pin: routing picks the executor
	// (home, music, cloud), so the word stays deterministic without naming a
	// role. An "ask" word prepares the reaction to the window the same word
	// opens: with a phrase the role's model answers that question, without one
	// the role answers whatever the person says next.
	result := wakeResult{Known: true, Action: entry.Action, Agent: entry.Agent}
	if entry.Action == wakeAsk {
		result.Route = "llm"
		if entry.Agent != "" {
			result.Route = bridge.AgentRoute(entry.Agent)
		}
		injector := native()
		if injector == nil {
			return result, errWakeNoSession
		}
		// The station plays only answers to requests it has sent itself, so the
		// reaction cannot be injected as a turn: the word opens the microphone
		// instead, and the turn becomes the reply to that window.
		armer, ok := injector.(presetArmer)
		if !ok {
			return result, errWakeNoPreset
		}
		if err := armer.ArmPreset(entry.Text, result.Route, bridge.DefaultPresetTTL); err != nil {
			return result, err
		}
		result.Listen = true
		delivery := "preset"
		if entry.Text == "" {
			// An empty phrase only opens the microphone: the words are the
			// person's, so the answer cannot be prepared in advance.
			delivery = "listen"
		}
		slog.Info("wake_action", "word", word, "action", entry.Action, "agent", entry.Agent, "route", result.Route, "text_runes", utf8.RuneCountInString(entry.Text), "delivery", delivery)
		return result, nil
	}
	frame, id, err := bridge.SyntheticTextInput(entry.Text)
	if err != nil {
		// The configuration already passed validation, so this is a broken
		// invariant rather than a bad request.
		return result, err
	}
	result.Request = id
	injector := native()
	if injector == nil {
		return result, errWakeNoSession
	}
	// The caller owns the deadline: the HTTP path detaches from a client that
	// hangs up, the poller bounds the answer itself.
	if result.Route != "" {
		ctx = bridge.WithPinnedRoute(ctx, result.Route)
	}
	if err := injector.Client(ctx, websocket.MessageBinary, frame); err != nil {
		return result, fmt.Errorf("%w: %w", errWakeInject, err)
	}
	slog.Info("wake_action", "word", word, "action", entry.Action, "agent", entry.Agent, "route", result.Route, "request", id, "text_runes", utf8.RuneCountInString(entry.Text))
	return result, nil
}

// wakeHandler accepts a start word reported by the wake-word daemon and decides
// what the station should do next. Loopback only: it acts with the station's
// upstream credentials and may open the microphone.
//
// The response is the contract with the daemon: "listen" tells it whether to
// open the microphone. Any failure leaves the daemon free to fall back to the
// stock behaviour, so an unknown word, a dry run and an explicit "listen" word
// all answer with listen:true.
func wakeHandler(table func() *wakeTable, native func() textInjector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requestIsLoopback(r) {
			writeProbeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		var body struct {
			Word string `json:"word"`
		}
		decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		if err := decoder.Decode(&body); err != nil {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		word := normalizeWakeWord(body.Word)
		if word == "" {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty word"})
			return
		}
		// Detached from r.Context(): a caller that hangs up mid-routing must not
		// cancel the turn that is already in flight.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), wakeActionTimeout)
		defer cancel()
		result, err := runWakeAction(ctx, table(), word, native)
		if err != nil {
			slog.Error("wake_action_failed", "word", word, "action", result.Action, "request", result.Request, "error", err)
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, errWakeNoSession), errors.Is(err, errWakeNoPreset):
				status = http.StatusServiceUnavailable
			case errors.Is(err, errWakeInject):
				status = http.StatusBadGateway
			}
			writeProbeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeProbeJSON(w, http.StatusOK, result.payload())
	}
}

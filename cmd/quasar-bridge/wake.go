package main

import (
	"context"
	stdjson "encoding/json"
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
		words := table()
		dryRun := words != nil && words.dryRun
		entry, known := words.lookup(word)
		if !known {
			// An unknown word is not an error: the daemon may know a word this
			// bridge was never told about. Listening keeps the stock behaviour.
			slog.Info("wake_hit", "word", word, "known", false, "action", wakeListen, "dry_run", dryRun)
			writeProbeJSON(w, http.StatusOK, map[string]any{"ok": true, "known": false, "action": wakeListen, "listen": true})
			return
		}
		slog.Info("wake_hit", "word", word, "known", true, "action", entry.Action, "agent", entry.Agent, "dry_run", dryRun)
		if entry.Action == wakeListen {
			writeProbeJSON(w, http.StatusOK, map[string]any{"ok": true, "known": true, "action": wakeListen, "listen": true})
			return
		}
		if dryRun {
			// The word is detected and logged, but the silent action is skipped:
			// the station simply listens, so a new word can be tried safely.
			slog.Info("wake_skipped", "word", word, "action", entry.Action, "reason", "dry_run")
			writeProbeJSON(w, http.StatusOK, map[string]any{"ok": true, "known": true, "action": wakeListen, "listen": true, "dry_run": true})
			return
		}
		// A "command" text is injected without a pin: routing picks the executor
		// (home, music, cloud), so the word stays deterministic without naming a
		// role. An "ask" text is pinned to its role, or to the default local
		// assistant when the word names none.
		route := ""
		if entry.Action == wakeAsk {
			route = "llm"
			if entry.Agent != "" {
				route = bridge.AgentRoute(entry.Agent)
			}
		}
		frame, id, err := bridge.SyntheticTextInput(entry.Text)
		if err != nil {
			// The configuration already passed validation, so this is a broken
			// invariant rather than a bad request.
			slog.Error("wake_action_failed", "word", word, "action", entry.Action, "error", err)
			writeProbeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		injector := native()
		if injector == nil {
			writeProbeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no upstream session"})
			return
		}
		// Detached from r.Context(): a caller that hangs up mid-routing must not
		// cancel the turn that is already in flight.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if route != "" {
			ctx = bridge.WithPinnedRoute(ctx, route)
		}
		if err := injector.Client(ctx, websocket.MessageBinary, frame); err != nil {
			slog.Error("wake_action_failed", "word", word, "action", entry.Action, "request", id, "error", err)
			writeProbeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		slog.Info("wake_action", "word", word, "action", entry.Action, "agent", entry.Agent, "route", route, "request", id, "text_runes", utf8.RuneCountInString(entry.Text))
		writeProbeJSON(w, http.StatusOK, map[string]any{"ok": true, "known": true, "action": entry.Action, "listen": false, "request": id})
	}
}

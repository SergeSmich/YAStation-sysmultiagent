package main

import (
	"context"
	stdjson "encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"quasarbridge/internal/wire"
)

// fakeWakeDaemon serves /api/state and counts /api/listen calls.
type fakeWakeDaemon struct {
	mu     sync.Mutex
	hits   []wakeHit
	status int
	listen int

	events bool        // serve /api/events
	push   chan string // raw SSE text to send on the stream
}

func (f *fakeWakeDaemon) setHits(hits ...wakeHit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = hits
}

func (f *fakeWakeDaemon) listenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listen
}

func (f *fakeWakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status, hits := f.status, append([]wakeHit{}, f.hits...)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/state" && r.Method == http.MethodGet:
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		// The daemon reports the newest hit first.
		for i, j := 0, len(hits)-1; i < j; i, j = i+1, j-1 {
			hits[i], hits[j] = hits[j], hits[i]
		}
		_ = stdjson.NewEncoder(w).Encode(map[string]any{"hits": hits})
	case r.URL.Path == "/api/events" && r.Method == http.MethodGet:
		if !f.events {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-f.push:
				_, _ = w.Write([]byte(line))
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
		}
	case r.URL.Path == "/api/listen" && r.Method == http.MethodPost:
		f.mu.Lock()
		f.listen++
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		http.NotFound(w, r)
	}
}

func newTestWakePoller(t *testing.T, daemon *fakeWakeDaemon, words wakeWordsConfig, injector *pinnedInjector) *wakePoller {
	t.Helper()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	return newWakePoller(server.URL, time.Second, func() *wakeTable { return newWakeTable(words) }, func() textInjector { return injector })
}

// TestWakePollerIgnoresHitsSeenAtStart verifies a restart does not replay the
// words the daemon already reported.
func TestWakePollerIgnoresHitsSeenAtStart(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 5, Word: "егорка"}, {ID: 6, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.poll(context.Background())
	if injector.count() != 0 || daemon.listenCount() != 0 {
		t.Fatalf("injected %d, listened %d on the first poll", injector.count(), daemon.listenCount())
	}
	if poller.lastHit != 6 {
		t.Fatalf("last hit = %d, want 6", poller.lastHit)
	}
}

// TestWakePollerActsOnNewHit verifies a fresh detection prepares the answer and
// opens the microphone the station will speak it into.
func TestWakePollerActsOnNewHit(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 6, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.poll(context.Background())
	daemon.setHits(
		wakeHit{ID: 6, Word: "егорка"},
		wakeHit{ID: 7, Word: "Егорка!", Score: 0.99, Fired: false},
	)
	poller.poll(context.Background())
	if injector.count() != 0 {
		t.Fatalf("ask injected %d frames", injector.count())
	}
	presets := injector.armed()
	if len(presets) != 1 {
		t.Fatalf("armed %d presets", len(presets))
	}
	if presets[0].text != "дай сводку" || presets[0].route != "agent:briefing" {
		t.Fatalf("preset = %+v", presets[0])
	}
	if daemon.listenCount() != 1 {
		t.Fatalf("microphone windows = %d, want 1", daemon.listenCount())
	}
}

// TestWakePollerActsOnEveryNewHitInOrder verifies a burst of detections is not
// reduced to the newest one.
func TestWakePollerActsOnEveryNewHitInOrder(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 1, Word: "свет"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "свет", Action: wakeCommand, Text: "включи свет"},
	}}, injector)
	poller.poll(context.Background())
	daemon.setHits(
		wakeHit{ID: 1, Word: "свет"},
		wakeHit{ID: 2, Word: "свет"},
		wakeHit{ID: 3, Word: "свет"},
	)
	poller.poll(context.Background())
	if injector.count() != 2 {
		t.Fatalf("injected %d frames, want 2", injector.count())
	}
	for _, frame := range injector.frames {
		decoded := wire.Decode(frame, wire.ClientToServer)
		if got := decoded.Client.GetEvent().GetTextInput().GetRequest().GetEvent().GetText(); got != "включи свет" {
			t.Fatalf("text = %q", got)
		}
	}
	// A repeated poll must not repeat the answer.
	poller.poll(context.Background())
	if injector.count() != 2 {
		t.Fatalf("injected %d frames after a repeated poll", injector.count())
	}
}

// TestWakePollerListenWordOpensMicrophone verifies the fallback the station
// needs while the daemon is in dry_run.
func TestWakePollerListenWordOpensMicrophone(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 1, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeListen},
	}}, injector)
	poller.poll(context.Background())
	daemon.setHits(wakeHit{ID: 1, Word: "егорка"}, wakeHit{ID: 2, Word: "егорка"})
	poller.poll(context.Background())
	if injector.count() != 0 {
		t.Fatalf("listen word injected %d frames", injector.count())
	}
	if daemon.listenCount() != 1 {
		t.Fatalf("listen calls = %d, want 1", daemon.listenCount())
	}
}

// TestWakePollerUnknownWordListens verifies an untaught word keeps the stock
// behaviour.
func TestWakePollerUnknownWordListens(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 1, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{}, injector)
	poller.poll(context.Background())
	daemon.setHits(wakeHit{ID: 1, Word: "егорка"}, wakeHit{ID: 2, Word: "неизвестное"})
	poller.poll(context.Background())
	if injector.count() != 0 || daemon.listenCount() != 1 {
		t.Fatalf("injected %d, listened %d", injector.count(), daemon.listenCount())
	}
}

// TestWakePollerSkipsDryRunAction verifies the bridge rehearsal mode leaves the
// reaction to the microphone.
func TestWakePollerSkipsDryRunAction(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 1, Word: "егорка"}}}
	injector := &pinnedInjector{}
	words := wakeWordsConfig{DryRun: true, Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}
	poller := newTestWakePoller(t, daemon, words, injector)
	poller.poll(context.Background())
	daemon.setHits(wakeHit{ID: 1, Word: "егорка"}, wakeHit{ID: 2, Word: "егорка"})
	poller.poll(context.Background())
	if injector.count() != 0 || daemon.listenCount() != 1 {
		t.Fatalf("injected %d, listened %d", injector.count(), daemon.listenCount())
	}
}

// TestWakePollerSurvivesUnavailableDaemon verifies a missing daemon does not
// stop the poller and that the next good answer is used.
func TestWakePollerSurvivesUnavailableDaemon(t *testing.T) {
	daemon := &fakeWakeDaemon{status: http.StatusInternalServerError, hits: []wakeHit{{ID: 1, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.poll(context.Background())
	if !poller.failing || poller.primed {
		t.Fatalf("failing = %v, primed = %v", poller.failing, poller.primed)
	}
	daemon.mu.Lock()
	daemon.status = 0
	daemon.mu.Unlock()
	poller.poll(context.Background())
	// The first good poll only primes the counter.
	if poller.failing || !poller.primed || poller.lastHit != 1 {
		t.Fatalf("failing = %v, primed = %v, last hit = %d", poller.failing, poller.primed, poller.lastHit)
	}
	if injector.count() != 0 {
		t.Fatalf("injected %d frames while priming", injector.count())
	}
}

// TestWakePollerHandlesCounterReset verifies a restarted daemon does not replay
// its new, small hit ids as fresh words.
func TestWakePollerHandlesCounterReset(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 10, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.poll(context.Background())
	daemon.setHits(wakeHit{ID: 1, Word: "егорка"})
	poller.poll(context.Background())
	if injector.count() != 0 {
		t.Fatalf("injected %d frames after a counter reset", injector.count())
	}
	if poller.lastHit != 1 {
		t.Fatalf("last hit = %d, want 1", poller.lastHit)
	}
}

// TestWakePollerActsOnFirstHitWhenListWasEmpty verifies a fresh daemon with no
// hits yet does not make the bridge swallow the first spoken word.
func TestWakePollerActsOnFirstHitWhenListWasEmpty(t *testing.T) {
	daemon := &fakeWakeDaemon{}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.poll(context.Background())
	if !poller.primed {
		t.Fatal("poll with an empty hit list did not prime the poller")
	}
	daemon.setHits(wakeHit{ID: 1, Word: "егорка"})
	poller.poll(context.Background())
	if presets := injector.armed(); len(presets) != 1 {
		t.Fatalf("armed %d presets, want 1", len(presets))
	}
	if daemon.listenCount() != 1 {
		t.Fatalf("microphone windows = %d, want 1", daemon.listenCount())
	}
}

// TestWakePollerSkipsEmptyWord verifies a malformed hit is consumed without a
// reaction and without stalling the counter.
func TestWakePollerSkipsEmptyWord(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 1, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.poll(context.Background())
	daemon.setHits(wakeHit{ID: 1, Word: "егорка"}, wakeHit{ID: 2, Word: "  "})
	poller.poll(context.Background())
	if injector.count() != 0 || daemon.listenCount() != 0 {
		t.Fatalf("injected %d, listened %d", injector.count(), daemon.listenCount())
	}
	if poller.lastHit != 2 {
		t.Fatalf("last hit = %d, want 2", poller.lastHit)
	}
}

// TestWakePollIntervalFromEnvironment verifies the development switch.
func TestWakePollIntervalFromEnvironment(t *testing.T) {
	t.Setenv("QUASAR_WAKE_POLL_MS", "0")
	if got := wakePollInterval(); got != 0 {
		t.Fatalf("interval = %v, want 0", got)
	}
	t.Setenv("QUASAR_WAKE_POLL_MS", "")
	if got := wakePollInterval(); got != defaultWakePollInterval {
		t.Fatalf("interval = %v, want %v", got, defaultWakePollInterval)
	}
}

func waitWake(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWakePollerEventStreamWakesPoller verifies a word reaches the bridge right
// after the daemon announces it, long before the safety-net timer fires.
func TestWakePollerEventStreamWakesPoller(t *testing.T) {
	daemon := &fakeWakeDaemon{
		hits:   []wakeHit{{ID: 3, Word: "егорка"}},
		events: true,
		push:   make(chan string, 4),
	}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitWake(t, "event stream", 2*time.Second, poller.eventsUp.Load)
	// Let the catch-up poll that follows the connection finish first, so the
	// reaction below can only come from the "data:" line.
	time.Sleep(150 * time.Millisecond)
	daemon.setHits(wakeHit{ID: 3, Word: "егорка"}, wakeHit{ID: 4, Word: "егорка"})
	daemon.push <- ": ping\n\n"
	daemon.push <- "data: {\"id\":4}\n\n"
	waitWake(t, "reaction to the event", time.Second, func() bool { return len(injector.armed()) == 1 })
	if poller.nextInterval() != time.Hour {
		t.Fatalf("next interval = %v", poller.nextInterval())
	}
}

// TestWakePollerFallsBackWithoutEventStream verifies an older daemon without
// /api/events is still polled at the fast period.
func TestWakePollerFallsBackWithoutEventStream(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 3, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(60 * time.Millisecond)
	daemon.setHits(wakeHit{ID: 3, Word: "егорка"}, wakeHit{ID: 4, Word: "егорка"})
	waitWake(t, "polled reaction", time.Second, func() bool { return len(injector.armed()) == 1 })
	if poller.eventsUp.Load() || poller.nextInterval() != 20*time.Millisecond {
		t.Fatalf("events up = %v, next = %v", poller.eventsUp.Load(), poller.nextInterval())
	}
}

// TestWakePollerFirstWordAfterDaemonRestart verifies an emptied hit list resets
// the counter, so the first word after a restart is not swallowed.
func TestWakePollerFirstWordAfterDaemonRestart(t *testing.T) {
	daemon := &fakeWakeDaemon{hits: []wakeHit{{ID: 10, Word: "егорка"}}}
	injector := &pinnedInjector{}
	poller := newTestWakePoller(t, daemon, wakeWordsConfig{Words: []wakeWordConfig{
		{Word: "егорка", Action: wakeAsk, Agent: "briefing", Text: "дай сводку"},
	}}, injector)
	poller.poll(context.Background())
	daemon.setHits()
	poller.poll(context.Background())
	daemon.setHits(wakeHit{ID: 1, Word: "егорка"})
	poller.poll(context.Background())
	if len(injector.armed()) != 1 {
		t.Fatalf("armed %d presets after a restart", len(injector.armed()))
	}
}

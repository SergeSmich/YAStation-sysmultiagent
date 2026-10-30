package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The wake-word daemon on the station detects a word and, by its own design,
// starts a conversation with the stock assistant. It never tells the bridge
// about the hit: there is no callback in either binary. So the bridge watches
// the daemon's own hit list instead and reacts with the table from the panel.
// The daemon runs with dry_run enabled, which keeps detection and the log but
// stops it from opening the microphone on its own.
const (
	defaultWakePollInterval = 250 * time.Millisecond

	wakePollTimeout   = 2 * time.Second
	wakeListenTimeout = 5 * time.Second
	wakePollBytes     = 1 << 20
)

// wakePollInterval is the poll period. A development machine without the
// daemon can disable the poller with QUASAR_WAKE_POLL_MS=0.
func wakePollInterval() time.Duration {
	value := strings.TrimSpace(os.Getenv("QUASAR_WAKE_POLL_MS"))
	if value == "" {
		return defaultWakePollInterval
	}
	ms, err := strconv.Atoi(value)
	if err != nil {
		return defaultWakePollInterval
	}
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// wakeHit is one detection as /api/state reports it. Newest first in the
// daemon's answer, but the poller sorts by id and ignores "fired": in dry run
// the conversation is skipped and the flag may stay false while the hit is
// still real.
type wakeHit struct {
	ID    int64   `json:"id"`
	Word  string  `json:"word"`
	Name  string  `json:"name"`
	Score float64 `json:"score"`
	At    string  `json:"at"`
	Fired bool    `json:"fired"`
}

// wakePoller turns the daemon's detections into the reactions configured in
// the panel.
type wakePoller struct {
	baseURL  string
	interval time.Duration
	table    func() *wakeTable
	native   func() textInjector
	client   *http.Client

	lastHit int64
	primed  bool
	failing bool
}

func newWakePoller(baseURL string, interval time.Duration, table func() *wakeTable, native func() textInjector) *wakePoller {
	return &wakePoller{
		baseURL:  strings.TrimRight(baseURL, "/"),
		interval: interval,
		table:    table,
		native:   native,
		client:   &http.Client{Timeout: wakePollTimeout},
	}
}

// run polls until the context is cancelled. The first poll only records the
// newest hit the daemon already knows: after a restart the bridge must not
// replay yesterday's words.
func (p *wakePoller) run(ctx context.Context) {
	if p.interval <= 0 {
		slog.Info("wake_poll_disabled", "url", p.baseURL)
		return
	}
	p.poll(ctx)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	slog.Info("wake_poll_started", "url", p.baseURL, "interval_ms", p.interval.Milliseconds())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll reads the daemon state once and acts on every hit newer than the last
// one seen. An unavailable daemon is reported once and swallowed: the station
// keeps its stock behaviour, the poller keeps trying.
func (p *wakePoller) poll(ctx context.Context) {
	hits, err := p.fetchState(ctx)
	if err != nil {
		// A station without the daemon (a test, a development machine) must not
		// flood the log with one line every 250 ms.
		if !p.failing {
			p.failing = true
			slog.Warn("wake_daemon_unavailable", "url", p.baseURL, "error", err)
		}
		return
	}
	if p.failing {
		p.failing = false
		slog.Info("wake_daemon_ready", "url", p.baseURL)
	}
	if len(hits) == 0 {
		// An empty list is a legitimate first answer: the daemon was restarted
		// and has not detected anything yet. Priming on it matters, otherwise
		// the very first spoken word would be swallowed as "already known".
		if !p.primed {
			p.primed = true
			slog.Info("wake_poll_ready", "url", p.baseURL, "last_hit", 0, "hits", 0)
		}
		return
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	newest := hits[len(hits)-1].ID
	if !p.primed {
		p.primed = true
		p.lastHit = newest
		slog.Info("wake_poll_ready", "url", p.baseURL, "last_hit", newest, "hits", len(hits))
		return
	}
	if newest < p.lastHit {
		// The daemon restarted and its counter began again: do not replay the
		// hits it already reported.
		slog.Info("wake_poll_reset", "last_hit", p.lastHit, "newest", newest)
		p.lastHit = newest
		return
	}
	for _, hit := range hits {
		if hit.ID <= p.lastHit {
			continue
		}
		// Advance before acting: a failed injection must not be retried on the
		// next tick, which would talk over the household twice.
		p.lastHit = hit.ID
		p.handle(ctx, hit)
	}
}

// handle performs the configured reaction for one hit.
func (p *wakePoller) handle(ctx context.Context, hit wakeHit) {
	word := normalizeWakeWord(hit.Word)
	if word == "" {
		return
	}
	slog.Info("wake_polled", "hit", hit.ID, "word", word, "score", hit.Score, "fired", hit.Fired)
	actionCtx, cancel := context.WithTimeout(ctx, wakeActionTimeout)
	defer cancel()
	result, err := runWakeAction(actionCtx, p.table(), word, p.native)
	if err != nil {
		slog.Error("wake_action_failed", "hit", hit.ID, "word", word, "action", result.Action, "request", result.Request, "error", err)
		return
	}
	if !result.Listen {
		return
	}
	// The daemon in dry_run does not open the microphone by itself; an explicit
	// request is the same thing its own page does with "listen".
	listenCtx, cancelListen := context.WithTimeout(ctx, wakeListenTimeout)
	defer cancelListen()
	if err := p.listen(listenCtx); err != nil {
		slog.Error("wake_listen_failed", "hit", hit.ID, "word", word, "error", err)
		return
	}
	slog.Info("wake_listen", "hit", hit.ID, "word", word)
}

// fetchState reads the hit list of the daemon.
func (p *wakePoller) fetchState(ctx context.Context) ([]wakeHit, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/state", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain the error body so the loopback connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("wake-word daemon returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, wakePollBytes+1))
	if err != nil || len(data) > wakePollBytes {
		return nil, errors.New("cannot read the wake-word daemon state")
	}
	var state struct {
		Hits []wakeHit `json:"hits"`
	}
	if err := stdjson.Unmarshal(data, &state); err != nil {
		return nil, errors.New("invalid wake-word daemon state")
	}
	return state.Hits, nil
}

// listen asks the daemon to open the microphone, as its own page does.
func (p *wakePoller) listen(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/listen", nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wake-word daemon returned HTTP %d", resp.StatusCode)
	}
	return nil
}

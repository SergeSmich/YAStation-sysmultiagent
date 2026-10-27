package bridge

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	"github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_generate"
	"github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/wire"
)

// VoiceProbeCase describes one TtsGenerate to try.
type VoiceProbeCase struct {
	Voice   string  `json:"voice"`
	Emotion string  `json:"emotion"`
	Quality string  `json:"quality"`
	Speed   float64 `json:"speed"`
	Volume  float64 `json:"volume"`
	Text    string  `json:"text"`
}

// VoiceProbeResult is the outcome of one case.
type VoiceProbeResult struct {
	VoiceProbeCase
	OK        bool   `json:"ok"`
	Error     string `json:"error"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

type probeEntry struct {
	id        string
	started   time.Time
	value     VoiceProbeCase
	done      chan VoiceProbeResult
	completed bool
	streams   map[uint32]struct{}
	grace     time.Duration
	cleanup   *time.Timer
}

// Probe sends synthesis probes sequentially over the live upstream connection.
func (n *Native) Probe(ctx context.Context, cases []VoiceProbeCase, perCase time.Duration) ([]VoiceProbeResult, error) {
	if len(cases) == 0 {
		return nil, errors.New("voice probe cases must not be empty")
	}
	if len(cases) > 30 {
		return nil, errors.New("voice probe supports at most 30 cases")
	}
	if !n.probeBusy.CompareAndSwap(false, true) {
		return nil, errors.New("voice probe already running")
	}
	defer n.probeBusy.Store(false)
	if perCase <= 0 {
		perCase = 3 * time.Second
	}
	if perCase > 5*time.Second {
		perCase = 5 * time.Second
	}

	slog.Info("voice_probe_started", "count", len(cases), "per_case_ms", perCase.Milliseconds())
	results := make([]VoiceProbeResult, 0, len(cases))
	for i, probe := range cases {
		if err := ctx.Err(); err != nil {
			slog.Info("voice_probe_done", "count", len(results), "error", err.Error())
			return results, err
		}
		if probe.Text == "" {
			probe.Text = "Проверка голоса"
		}
		result := n.probeCase(ctx, probe, perCase)
		results = append(results, result)
		slog.Info("voice_probe_case", "index", i, "voice", probe.Voice, "ok", result.OK, "error", result.Error, "elapsed_ms", result.ElapsedMS)
		if err := ctx.Err(); err != nil {
			slog.Info("voice_probe_done", "count", len(results), "error", err.Error())
			return results, err
		}
		if i+1 < len(cases) {
			select {
			case <-ctx.Done():
				slog.Info("voice_probe_done", "count", len(results), "error", ctx.Err().Error())
				return results, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	slog.Info("voice_probe_done", "count", len(results))
	return results, nil
}

func (n *Native) probeCase(ctx context.Context, probe VoiceProbeCase, perCase time.Duration) VoiceProbeResult {
	id := newID()
	entry := &probeEntry{id: id, started: time.Now(), value: probe, done: make(chan VoiceProbeResult, 1), grace: perCase}
	n.mu.Lock()
	if n.ctx.Err() != nil {
		n.mu.Unlock()
		return VoiceProbeResult{VoiceProbeCase: probe, Error: "closed", ElapsedMS: time.Since(entry.started).Milliseconds()}
	}
	if n.probes == nil {
		n.probes = make(map[string]*probeEntry)
	}
	n.probes[id] = entry
	n.mu.Unlock()

	caseCtx, cancel := context.WithTimeout(ctx, perCase)
	defer cancel()
	frame, err := probeFrame(probe, id)
	if err == nil {
		err = n.up(caseCtx, websocket.MessageBinary, frame)
	}
	if err != nil {
		n.removeProbe(id)
		message := err.Error()
		if n.ctx.Err() != nil {
			message = "closed"
		} else if ctx.Err() != nil {
			message = ctx.Err().Error()
		} else if errors.Is(err, context.DeadlineExceeded) {
			message = "timeout"
		}
		return VoiceProbeResult{VoiceProbeCase: probe, Error: message, ElapsedMS: time.Since(entry.started).Milliseconds()}
	}
	select {
	case result := <-entry.done:
		n.mu.Lock()
		if n.probes[id] == entry {
			delay := perCase
			if len(entry.streams) != 0 {
				delay = time.Minute
			}
			n.scheduleProbeCleanupLocked(entry, delay)
		}
		n.mu.Unlock()
		return result
	case <-caseCtx.Done():
		n.removeProbe(id)
		if ctx.Err() != nil {
			return VoiceProbeResult{VoiceProbeCase: probe, Error: ctx.Err().Error(), ElapsedMS: time.Since(entry.started).Milliseconds()}
		}
		return VoiceProbeResult{VoiceProbeCase: probe, Error: "timeout", ElapsedMS: time.Since(entry.started).Milliseconds()}
	}
}

func (n *Native) removeProbe(id string) {
	n.mu.Lock()
	if entry := n.probes[id]; entry != nil {
		if entry.cleanup != nil {
			entry.cleanup.Stop()
		}
		n.dropProbeStreamsLocked(entry)
	}
	delete(n.probes, id)
	n.mu.Unlock()
}

// dropProbeStreamsLocked forgets the streams of a finished probe, so audio of a
// later real turn reusing the same stream id is not dropped.
func (n *Native) dropProbeStreamsLocked(entry *probeEntry) {
	for streamID := range entry.streams {
		delete(n.probeStreams, streamID)
	}
	entry.streams = nil
}

func (n *Native) scheduleProbeCleanupLocked(entry *probeEntry, delay time.Duration) {
	if entry.cleanup != nil {
		entry.cleanup.Stop()
	}
	if delay <= 0 {
		delay = time.Second
	}
	entry.cleanup = time.AfterFunc(delay, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.probes[entry.id] != entry {
			return
		}
		n.dropProbeStreamsLocked(entry)
		delete(n.probes, entry.id)
	})
}

func probeFrame(probe VoiceProbeCase, id string) ([]byte, error) {
	generate := &tts_generate.TTtsGenerate{Text: new(probe.Text), Lang: new("ru-RU"), Format: new("audio/opus")}
	if probe.Voice != "" {
		generate.Voice = new(probe.Voice)
	}
	if probe.Emotion != "" {
		generate.Emotion = new(probe.Emotion)
	}
	if probe.Quality != "" {
		generate.Quality = new(probe.Quality)
	}
	if probe.Speed != 0 {
		generate.Speed = new(probe.Speed)
	}
	if probe.Volume != 0 {
		generate.Volume = new(probe.Volume)
	}
	message := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &event_header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TtsGenerate{TtsGenerate: generate}}}}
	return (wire.Frame{Client: message, Prefix: []byte("AAPI")}).Marshal()
}

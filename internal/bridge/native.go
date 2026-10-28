package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	cancellation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	continuation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/continue_request"
	ack "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/input_start_ack"
	tts "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_generate"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

type Resolution struct {
	Route  string
	Answer string
}
type Resolver interface {
	Resolve(context.Context, string) (Resolution, error)
}
type RoutedResolver interface {
	Decide(context.Context, string) RouteDecision
	ResolveDecision(context.Context, string, RouteDecision) (Resolution, error)
}
type FallbackResolver interface {
	Fallback(context.Context, string) (Resolution, error)
}

type nativeRequest struct {
	streaming                       bool
	listenOnStreamStart             bool
	partial                         uint32
	responseID                      string
	final                           bool
	header                          *header.TEventHeader
	isText                          bool
	ttsStarted                      bool
	gated                           bool
	input                           []byte
	id                              string
	request                         *request.TAliceRequestProto
	text                            string
	route                           string
	voice                           VoiceSettings
	voiceQueue                      []string
	voiceChainReady                 bool
	voiceFallback                   bool
	speechText                      string
	speechReady                     bool
	speechAttempt                   int
	speechMu                        sync.Mutex
	ctx                             context.Context
	cancel                          context.CancelFunc
	started, canceled, local, cloud bool
	continuation                    []byte
	ttsID                           string
	progressTTS                     string
	audio                           oggSpeech
	audioIsProgress                 bool
	audioStarted                    bool
	audioStream                     uint32
	audioWaiting                    bool
	progressAudioDone               chan struct{}
	soundRequest                    *sound.Request
	soundDirective                  *soundDirective
	soundSealed                     bool
	expires                         *time.Timer
	cloudWatchdogGen                int64
	cloudFirstFrameSeen             bool
	cloudWatchdogFired              bool
	cloudFallbackSent               bool
}
type Native struct {
	connectionScope   string
	ctx               context.Context
	cancel            context.CancelFunc
	up, down          Sender
	resolver          Resolver
	mu                sync.Mutex
	requests          map[string]*nativeRequest
	tts               map[string]*nativeRequest
	streams           map[uint32]*nativeRequest
	blocked           map[uint32]*nativeRequest
	probes            map[string]*probeEntry
	probeStreams      map[uint32]struct{}
	probePlay         map[uint32]struct{}
	probeBusy         atomic.Bool
	wg                sync.WaitGroup
	OnRoute           func(id, route string)
	GateText          bool
	ShouldListen      bool
	voice             atomic.Pointer[VoiceConfig]
	DecisionDelay     time.Duration
	ProgressAfter     time.Duration
	ProgressInterval  time.Duration
	SpeechTimeout     time.Duration
	FirstFrameTimeout time.Duration
	OnCloudStall      func()
}

func NewNative(ctx context.Context, up, down Sender, resolver Resolver) *Native {
	ctx, cancel := context.WithCancel(ctx)
	return &Native{connectionScope: newID(), ctx: ctx, cancel: cancel, up: up, down: down, resolver: resolver, ShouldListen: true, requests: map[string]*nativeRequest{}, tts: map[string]*nativeRequest{}, streams: map[uint32]*nativeRequest{}, blocked: map[uint32]*nativeRequest{}, probes: map[string]*probeEntry{}, probeStreams: map[uint32]struct{}{}, probePlay: map[uint32]struct{}{}}
}

// SetVoice replaces the speech configuration for every answer that has not
// resolved its voice yet. It is safe to call while a session is live: the
// pointer swap is atomic, so an in-flight answer keeps the settings it took and
// no reconnect is needed to change the voice.
func (n *Native) SetVoice(cfg VoiceConfig) {
	c := cfg
	n.voice.Store(&c)
}

// Voice returns the speech configuration currently in force. A missing value
// keeps the stock Alice voice.
func (n *Native) Voice() VoiceConfig {
	if cfg := n.voice.Load(); cfg != nil {
		return *cfg
	}
	return VoiceConfig{}
}
func (n *Native) Close() {
	n.cancel()
	n.mu.Lock()
	for _, p := range n.requests {
		p.canceled = true
		p.cancel()
		p.expires.Stop()
		p.cloudWatchdogGen++
	}
	for id, probe := range n.probes {
		if probe.cleanup != nil {
			probe.cleanup.Stop()
		}
		select {
		case probe.done <- VoiceProbeResult{VoiceProbeCase: probe.value, Error: "closed", ElapsedMS: time.Since(probe.started).Milliseconds()}:
		default:
		}
		delete(n.probes, id)
	}
	clear(n.probeStreams)
	clear(n.probePlay)
	n.mu.Unlock()
	n.wg.Wait()
}
func (n *Native) Client(ctx context.Context, k websocket.MessageType, data []byte) error {
	f := wire.Decode(data, wire.ClientToServer)
	if f.Client == nil || f.Client.GetEvent() == nil {
		return n.up(ctx, k, data)
	}
	e := f.Client.GetEvent()
	if stat := e.GetRequestStat(); stat != nil {
		kind := "none"
		raw := strings.ToLower(stat.GetError())
		if raw != "" {
			kind = "other"
			if strings.Contains(raw, "timeout") || strings.Contains(raw, "timed out") || strings.Contains(raw, "deadline") {
				kind = "timeout"
			} else if strings.Contains(raw, "cancel") {
				kind = "canceled"
			}
		}
		timestamps := stat.GetTimestamps()
		reported := func(value string) bool { return value != "" && value != "0" }
		slog.Info("native_playback_status", "request", requestTag(stat.GetRequestMessageId()), "error_class", kind, "error", statError(stat.GetError()), "playback_reported", reported(timestamps.GetOnSoundPlayerBeginTime()) || reported(timestamps.GetOnPlayerBegin()), "finished_reported", reported(timestamps.GetOnSoundPlayerEndTime()) || reported(timestamps.GetOnPlayerEnd()))
	}
	var req *request.TAliceRequestProto
	switch f.Kind() {
	case "TextInput":
		req = e.GetTextInput()
	case "VoiceInput":
		req = e.GetVoiceInput()
	case "SpeechInput":
		req = e.GetSpeechInput()
	}
	if req != nil && f.Kind() == "TextInput" && strings.TrimSpace(req.GetRequest().GetEvent().GetText()) == "" {
		return n.up(ctx, k, data)
	}
	if req != nil {
		id := e.GetHeader().GetMessageId()
		if id == "" {
			return n.up(ctx, k, data)
		}
		scope := "connection:" + n.connectionScope
		if id := req.GetApplication().GetDeviceId(); id != "" {
			scope = "device:" + id
		} else if id := req.GetApplication().GetUuid(); id != "" {
			scope = "uuid:" + id
		}
		pctx, cancel := context.WithTimeout(dialogue.WithSpeaker(n.ctx, scope), 90*time.Second)
		playerState := nativePlaybackState(req)
		pctx = sound.WithPlaybackState(pctx, playerState)
		slog.Info("native_player_state", "request", requestTag(id), "present", playerState.Present, "player_state", playerState.PlayerState, "offset_known", playerState.OffsetKnown, "offset_ms", playerState.OffsetMS, "has_stream", playerState.StreamID != "", "stream_tag", requestTag(playerState.StreamID))
		p := &nativeRequest{id: id, request: proto.Clone(req).(*request.TAliceRequestProto), text: req.GetRequest().GetEvent().GetText(), ctx: pctx, cancel: cancel}
		p.ctx = sound.WithPlayer(p.ctx, func(ctx context.Context, request sound.Request) error {
			return n.queueSound(ctx, p, request)
		})
		p.header = proto.Clone(e.GetHeader()).(*header.TEventHeader)
		p.isText = f.Kind() == "TextInput"
		p.input = append([]byte{}, data...)
		p.gated = !p.isText || n.GateText

		slog.Info("native_request_shape", "request", requestTag(id), "text_bytes", len(p.text), "event_type", req.GetRequest().GetEvent().GetType().String(), "stream", e.GetHeader().GetStreamId(), "unknown_request_bytes", len(req.ProtoReflect().GetUnknown()), "payload_bytes", len(req.GetRequest().GetEvent().GetPayloadRaw()))
		n.mu.Lock()
		if n.requests[id] != nil {
			n.mu.Unlock()
			cancel()
			return nil
		}
		if len(n.requests) >= 128 {
			n.mu.Unlock()
			cancel()
			return errors.New("too many pending native requests")
		}
		if preparer, ok := n.resolver.(interface {
			Prepare(context.Context) context.Context
		}); ok {
			p.ctx = preparer.Prepare(p.ctx)
		}
		p.expires = time.AfterFunc(90*time.Second, func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			p.canceled = true
			p.cancel()
			p.cloudWatchdogGen++
			delete(n.requests, p.id)
			delete(n.tts, p.ttsID)
			delete(n.tts, p.progressTTS)
			for stream, q := range n.blocked {
				if q == p {
					delete(n.blocked, stream)
				}
			}
			for stream, q := range n.streams {
				if q == p {
					delete(n.streams, stream)
				}
			}
		})
		n.requests[id] = p
		n.mu.Unlock()
		if p.gated {
			gated, err := wire.EnableApproval(f)
			if err != nil {
				return err
			}
			if err := n.up(ctx, k, gated); err != nil {
				return err
			}
		}
		// The original input stays local until routing has selected exactly one executor.
		n.maybeResolve(p)
		return nil
	}
	if c := e.GetContinueRequest(); c != nil {
		n.mu.Lock()
		p := n.requests[c.GetRequestMessageId()]
		if p != nil && (p.canceled || p.local) {
			n.mu.Unlock()
			return nil
		}
		if p != nil && !p.cloud {
			if p.continuation != nil {
				n.mu.Unlock()
				return nil
			}
			p.continuation = append([]byte{}, data...)
			n.mu.Unlock()
			n.maybeResolve(p)
			return nil
		}
		n.mu.Unlock()
	}
	if c := e.GetCancelRequest(); c != nil {
		n.mu.Lock()
		p := n.requests[c.GetRequestMessageId()]
		ttsID := ""
		cloudCancelHandled := false
		if p != nil {
			p.canceled = true
			p.cancel()
			p.cloudWatchdogGen++
			ttsID = p.ttsID
			cloudCancelHandled = p.cloudWatchdogFired
		}
		n.mu.Unlock()
		if cloudCancelHandled {
			return nil
		}
		if ttsID != "" {
			_ = n.sendCancel(ctx, ttsID)
		}
	}
	return n.up(ctx, k, data)
}
func (n *Native) findLocked(h *header.TEventHeader) (*nativeRequest, bool) {
	if h == nil {
		return nil, false
	}
	for _, id := range []string{h.GetRefMessageId(), h.GetSubRequestRef().GetMessageId(), h.GetSubRequestRef().GetRequestId()} {
		if p := n.tts[id]; p != nil {
			return p, true
		}
		if p := n.requests[id]; p != nil {
			return p, false
		}
		if id != "" {
			for _, p := range n.requests {
				if p.request.GetHeader().GetRequestId() == id {
					return p, false
				}
			}
		}
	}
	return nil, false
}

func (n *Native) handleProbeResponseLocked(kind string, e *server.TServerMessage_TEvent) bool {
	if e == nil || e.GetHeader() == nil {
		return false
	}
	h := e.GetHeader()
	var entry *probeEntry
	for _, id := range []string{h.GetRefMessageId(), h.GetSubRequestRef().GetMessageId()} {
		if id != "" {
			entry = n.probes[id]
			if entry != nil {
				break
			}
		}
	}
	if entry == nil {
		return false
	}
	result := VoiceProbeResult{VoiceProbeCase: entry.value, ElapsedMS: time.Since(entry.started).Milliseconds()}
	if kind == "TtsSpeak" {
		result.OK = true
		if streamID := uint32(h.GetStreamId()); streamID != 0 {
			n.probeStreams[streamID] = struct{}{}
			if entry.play() {
				// The sample is meant to be heard: its audio frames are forwarded
				// to the station instead of being swallowed with the probe.
				n.probePlay[streamID] = struct{}{}
			}
			if entry.streams == nil {
				entry.streams = make(map[uint32]struct{})
			}
			entry.streams[streamID] = struct{}{}
			// Keep the stream id until its audio ends, but never forever: a probe
			// that is accepted and then goes quiet must not pin the id.
			n.scheduleProbeCleanupLocked(entry, time.Minute)
		}
	} else if exception := e.GetEventException(); exception != nil {
		result.Error = exception.GetError().GetErrorMessage()
	}
	if !entry.completed {
		entry.completed = true
		select {
		case entry.done <- result:
		default:
		}
	}
	// A playing probe keeps its TtsSpeak on the normal path, so the station
	// learns about the stream it is about to hear.
	return !(entry.play() && kind == "TtsSpeak")
}

func (n *Native) Server(ctx context.Context, k websocket.MessageType, data []byte) error {
	f := wire.Decode(data, wire.ServerToClient)
	if f.Server == nil {
		if len(data) >= 4 {
			n.mu.Lock()
			streamID := binary.BigEndian.Uint32(data)
			_, probeStream := n.probeStreams[streamID]
			_, probePlay := n.probePlay[streamID]
			p := n.streams[streamID]
			// A playing probe is the one exception: its audio goes to the speaker.
			drop := (probeStream && !probePlay) || n.blocked[streamID] != nil || (p != nil && p.canceled)
			if p != nil && !drop {
				n.cloudFrameReceivedLocked(p)
			}
			if p != nil && p.streaming && !drop {
				out, err := p.audio.feed(data[4:])
				if err == nil && len(out) > 0 {
					err = n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), out...))
				}
				if err != nil {
					p.cancel()
				}
				n.mu.Unlock()
				return err
			}
			n.mu.Unlock()
			if drop {
				return nil
			}
		}
		return n.down(ctx, k, data)
	}
	if sc := f.Server.GetStreamControl(); sc != nil {
		n.mu.Lock()
		streamID := uint32(sc.GetStreamId())
		if _, probeStream := n.probeStreams[streamID]; probeStream {
			if sc.GetClose() != nil {
				delete(n.probeStreams, streamID)
				delete(n.probePlay, streamID)
				for _, entry := range n.probes {
					delete(entry.streams, streamID)
					if entry.completed && len(entry.streams) == 0 {
						n.scheduleProbeCleanupLocked(entry, entry.grace)
					}
				}
			}
			n.mu.Unlock()
			return nil
		}
		if n.blocked[uint32(sc.GetStreamId())] != nil {
			delete(n.blocked, uint32(sc.GetStreamId()))
			n.mu.Unlock()
			return nil
		}
		p := n.tts[sc.GetMessageId()]
		if p == nil {
			p = n.streams[uint32(sc.GetStreamId())]
		}
		original := n.requests[sc.GetMessageId()]
		if p != nil {
			n.cloudFrameReceivedLocked(p)
		} else if original != nil {
			n.cloudFrameReceivedLocked(original)
		}
		if sc.GetClose() != nil {
			delete(n.streams, uint32(sc.GetStreamId()))
		}
		if p != nil {
			if p.canceled {
				n.mu.Unlock()
				return nil
			}
			if p.streaming && sc.GetClose() != nil {
				if err := p.audio.endSource(); err != nil {
					p.cancel()
					n.mu.Unlock()
					return err
				}
				if p.audioIsProgress && sc.GetClose().GetReason() == 0 {
					if tail := p.audio.flushPending(0, 0); len(tail) > 0 {
						if err := n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), tail...)); err != nil {
							n.mu.Unlock()
							return err
						}
					}
					if !p.audioWaiting {
						p.audioWaiting = true
						n.waitAudio(p)
						close(p.progressAudioDone)
					}
					n.mu.Unlock()
					return nil
				}
				end := p.audio.finish()
				if err := n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), end...)); err != nil {
					n.mu.Unlock()
					return err
				}
				sc.StreamId = int64(p.audioStream)
			}
			sc.MessageId = p.id
			n.mu.Unlock()
			out, err := f.Marshal()
			if err != nil {
				return err
			}
			return n.down(ctx, k, out)
		}
		drop := original != nil && original.local && (original.header.StreamId == nil || sc.GetStreamId() != original.header.GetStreamId())
		n.mu.Unlock()
		if drop {
			return nil
		}
		return n.down(ctx, k, data)
	}
	e := f.Server.GetEvent()
	if e == nil {
		return n.down(ctx, k, data)
	}

	n.mu.Lock()
	if (f.Kind() == "TtsSpeak" || f.Kind() == "EventException") && len(n.probes) > 0 {
		if n.handleProbeResponseLocked(f.Kind(), e) {
			n.mu.Unlock()
			return nil
		}
	}
	if f.Kind() == "TtsSpeak" {
		// A played probe shares this path; never drop a real stream that happens
		// to carry the same id.
		streamID := uint32(e.GetHeader().GetStreamId())
		if _, probe := n.probeStreams[streamID]; !probe {
			delete(n.streams, streamID)
			delete(n.blocked, streamID)
		}
	}
	p, isTTS := n.findLocked(e.GetHeader())
	if p != nil && cloudAnswerFrame(f.Kind()) {
		n.cloudFrameReceivedLocked(p)
	}
	if r := e.GetAliceResponse(); r != nil {
		var ds []string
		for _, d := range r.GetResponse().GetDirectives() {
			ds = append(ds, d.GetType()+":"+d.GetName())
		}

		slog.Info("native_cloud_shape", "matched", p != nil, "directives", ds, "cards", len(r.GetResponse().GetCards()), "voice_text_bytes", len(r.GetVoiceResponse().GetOutputSpeech().GetText()))
	}
	if p == nil {
		n.mu.Unlock()
		return n.down(ctx, k, data)
	}
	if isTTS {
		if p.canceled {
			n.mu.Unlock()
			return nil
		}
		if x := e.GetEventException(); x != nil {
			// The synthesizer rejected the request, typically because the voice
			// name is unknown to its backend. Try the next configured voice and
			// finally the default Alice voice instead of leaving the turn
			// without audio.
			reason := x.GetError().GetErrorMessage()
			n.mu.Unlock()
			slog.Warn("native_tts_exception", "request", requestTag(p.id), "message", reason)
			if !n.retrySpeech(p, -1, "native_tts_retry", reason, false) {
				return n.down(ctx, k, data)
			}
			return nil
		}
		e.Header.RefMessageId = p.id
		if ref := e.Header.SubRequestRef; ref != nil {
			ref.MessageId = p.id
			ref.RequestId = p.request.GetHeader().GetRequestId()
		}
		if e.GetTtsSpeak() != nil {
			p.speechReady = true
			n.streams[uint32(e.Header.GetStreamId())] = p
			if p.streaming {
				if e.GetTtsSpeak().GetFormat() != "audio/opus" {
					p.cancel()
					n.mu.Unlock()
					return errors.New("streaming progress requires Opus")
				}
				p.audioIsProgress = p.ttsID == p.progressTTS
				p.audio.next()
				p.audio.source.discardAudio = p.audioIsProgress
				if p.audioStarted {
					p.audioWaiting = false
					n.mu.Unlock()
					return nil
				}
				p.audioStream = uint32(e.Header.GetStreamId())
				p.audioStarted = true
			}
			slog.Info("native_tts_ready", "request", requestTag(p.id), "format", e.GetTtsSpeak().GetFormat(), "stream", e.Header.GetStreamId())
		}
		n.mu.Unlock()
		out, err := f.Marshal()
		if err != nil {
			return err
		}
		return n.down(ctx, k, out)
	}

	if f.Kind() == "TtsSpeak" {
		p.ttsStarted = true
		if p.local || p.canceled {
			n.blocked[uint32(e.GetHeader().GetStreamId())] = p
		}
	}
	if p.local || p.canceled {
		n.mu.Unlock()
		return nil
	}
	if r := e.GetAliceResponse(); r != nil && p.cloud && !p.ttsStarted && canReplaceResponse(r) {
		text := ""
		for _, card := range r.GetResponse().GetCards() {
			text += card.GetText() + " "
		}
		if strings.TrimSpace(text) == "" {
			text = r.GetVoiceResponse().GetOutputSpeech().GetText()
		}

		if resolver, ok := n.resolver.(FallbackResolver); ok && n.isRefusal(text) {
			p.local = true
			p.voice = n.Voice().Resolve("llm")
			if n.OnRoute != nil {
				n.OnRoute(p.id, "llm")
			}
			p.ttsID = newID()
			n.tts[p.ttsID] = p
			query := p.text
			n.mu.Unlock()
			n.wg.Go(func() {
				result, err := resolver.Fallback(p.ctx, query)
				if p.ctx.Err() != nil {
					return
				}
				if errors.Is(err, context.Canceled) {
					n.discardStale(p)
					return
				}
				if err != nil {
					n.discardSound(p)
					result.Answer = "Сейчас не получилось получить ответ локальной модели."
				}
				_ = n.sendCancel(p.ctx, p.id)
				if err := n.reply(p, result.Answer); err != nil {
					slog.Warn("native_reply_failed", "request", requestTag(p.id))
				}
				slog.Info("native_fallback", "request", requestTag(p.id), "route", "llm")
			})
			return nil
		}
	}
	switch f.Kind() {
	case "AliceResponse":
		if !p.cloud {
			p.canceled = true
			p.cancel()
			slog.Warn("native_approval_not_honored", "request", requestTag(p.id))
		}
	case "ServerRequestReady":
		slog.Info("native_server_ready", "request", requestTag(p.id))
	case "AsrResult":
		asr := e.GetAsrResult()
		if asr.GetEndOfUtt() && len(asr.GetRecognition()) > 0 {
			r := asr.GetRecognition()[0]
			p.text = r.GetNormalized()
			if p.text == "" {
				var words []string
				for _, w := range r.GetWords() {
					words = append(words, w.GetValue())
				}
				p.text = strings.Join(words, " ")
			}
		}
		if asr.GetEndOfUtt() {
			p.final = true
			// The length, not the text: an empty recognition silently sends the
			// request back to the cloud, and that is invisible without a size.
			slog.Info("native_asr_final", "request", requestTag(p.id), "hypotheses", len(asr.GetRecognition()), "code", asr.GetResponseCode().String(), "text_bytes", len(strings.TrimSpace(p.text)))
		}
	case "SpotterValidation":
		sv := e.GetSpotterValidation()
		slog.Info("native_spotter_validation", "request", requestTag(p.id), "result", sv.GetResult().String(), "canceled_cause", sv.GetCanceledCauseOfMultiactivation())

	}
	n.mu.Unlock()
	n.maybeResolve(p)
	if err := n.down(ctx, k, data); err != nil {
		return err
	}
	n.captureCloud(p, e)
	return nil
}
func (n *Native) maybeResolve(p *nativeRequest) {
	n.mu.Lock()
	if p.started || p.canceled || (strings.TrimSpace(p.text) == "" && !p.final) {
		n.mu.Unlock()
		return
	}
	p.started = true
	text := p.text
	n.mu.Unlock()
	n.wg.Go(func() {
		if n.DecisionDelay > 0 {
			select {
			case <-time.After(n.DecisionDelay):
			case <-p.ctx.Done():
				return
			}
		}
		result := Resolution{Route: "yandex"}
		var err error
		if n.resolver != nil && strings.TrimSpace(text) != "" {
			if routed, ok := n.resolver.(RoutedResolver); ok {
				decision := routed.Decide(p.ctx, text)
				n.mu.Lock()
				p.listenOnStreamStart = decision.Source == "dialogue_retry"
				n.mu.Unlock()
				slog.Info("native_route_decision", "request", requestTag(p.id), "route", decision.Route, "source", decision.Source)
				stop := func() {}
				if decision.Route != "yandex" && n.ProgressAfter > 0 {
					stop = n.progress(p)
				}
				result, err = routed.ResolveDecision(p.ctx, text, decision)
				stop()
			} else {
				result, err = n.resolver.Resolve(p.ctx, text)
			}
		}
		if p.ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			n.discardStale(p)
			return
		}
		if err != nil {
			n.discardSound(p)
			if result.Route == "home" {
				result.Answer = "Не удалось подтвердить выполнение команды в умном доме."
			} else if result.Route == "music" {
				result.Answer = "Не удалось включить музыку с этой страницы."
			} else {
				result.Route = "llm"
				result.Answer = "Сейчас не получилось получить ответ локальной модели."
			}
		}
		n.mu.Lock()
		if p.canceled {
			n.mu.Unlock()
			return
		}
		if n.OnRoute != nil {
			n.OnRoute(p.id, result.Route)
		}
		p.route = result.Route
		p.voice = n.Voice().Resolve(result.Route)
		if result.Route == "yandex" {
			p.cloud = true
			var err error
			submitted := false
			if p.gated {
				control := p.continuation
				if control == nil {
					control, err = n.makeContinue(p)
				}
				if err == nil {
					err = n.up(p.ctx, websocket.MessageBinary, control)
					submitted = err == nil
				}
			} else {
				err = n.up(p.ctx, websocket.MessageBinary, p.input)
				submitted = err == nil
				if err == nil && p.continuation != nil {
					err = n.up(p.ctx, websocket.MessageBinary, p.continuation)
				}
			}
			n.mu.Unlock()
			// An activation that caught no words has nothing to fall back to:
			// arming the watchdog would only make the station announce a
			// failure after a request it never understood. The request still
			// goes upstream, because the cloud re-recognizes the audio itself.
			empty := strings.TrimSpace(text) == ""
			if submitted && n.FirstFrameTimeout > 0 && !empty {
				n.armCloudWatchdog(p)
			}
			if empty {
				slog.Info("native_empty_asr", "request", requestTag(p.id), "sent", submitted)
			}
			slog.Info("native_route", "request", requestTag(p.id), "route", "yandex", "sent", err == nil)
			return
		}
		p.local = true
		if !p.streaming {
			p.ttsID = newID()
			n.tts[p.ttsID] = p
		}
		n.mu.Unlock()
		slog.Info("native_route", "request", requestTag(p.id), "route", result.Route)
		if p.gated && !p.streaming {
			if err := n.sendCancel(p.ctx, p.id); err != nil {
				return
			}
		}
		if err := n.reply(p, result.Answer); err != nil {
			slog.Warn("native_reply_failed", "request", requestTag(p.id))
		}
	})
}

func (n *Native) armCloudWatchdog(p *nativeRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.FirstFrameTimeout <= 0 || p.canceled || p.local || !p.cloud || p.cloudFirstFrameSeen || p.cloudWatchdogFired || n.requests[p.id] != p {
		return
	}
	p.cloudWatchdogGen++
	gen := p.cloudWatchdogGen
	slog.Info("native_cloud_watchdog_started", "request", requestTag(p.id), "timeout_ms", n.FirstFrameTimeout.Milliseconds())
	time.AfterFunc(n.FirstFrameTimeout, func() { n.cloudWatchdogFiredCallback(p, gen) })
}

// cloudAnswerFrame reports whether an upstream event is the answer starting
// rather than protocol chatter. The cloud also correlates SpotterValidation,
// AsrResult and RequestStatAck with the request and the station answers within
// milliseconds, so any of them counted as "the first frame" would disarm the
// stall watchdog for good: the fallback could never fire.
func cloudAnswerFrame(kind string) bool {
	switch kind {
	case "ServerRequestReady", "AliceResponse", "DeferredAliceResponse", "TtsSpeak", "EventException":
		return true
	}
	return false
}

// cloudFrameReceivedLocked invalidates a request's timer on its first correlated upstream frame.
func (n *Native) cloudFrameReceivedLocked(p *nativeRequest) {
	if n.FirstFrameTimeout <= 0 || p == nil || !p.cloud || p.cloudFirstFrameSeen || p.cloudWatchdogFired {
		return
	}
	p.cloudWatchdogGen++
	p.cloudFirstFrameSeen = true
	slog.Info("native_cloud_watchdog_first_frame", "request", requestTag(p.id))
}

func (n *Native) cloudWatchdogFiredCallback(p *nativeRequest, gen int64) {
	n.mu.Lock()
	if p.canceled || p.local || !p.cloud || p.cloudFirstFrameSeen || p.cloudWatchdogFired || p.cloudFallbackSent || p.cloudWatchdogGen != gen || n.requests[p.id] != p {
		n.mu.Unlock()
		return
	}
	p.cloudWatchdogFired = true
	p.local = true
	n.mu.Unlock()

	if n.OnCloudStall != nil {
		n.OnCloudStall()
	}
	slog.Warn("native_cloud_watchdog_fired", "request", requestTag(p.id), "timeout_ms", n.FirstFrameTimeout.Milliseconds())

	cancelCtx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
	_ = n.sendCancel(cancelCtx, p.id)
	cancel()

	if err := n.sendCloudFallback(p); err != nil {
		slog.Warn("native_cloud_fallback_failed", "request", requestTag(p.id), "error", err)
		return
	}
	n.mu.Lock()
	p.cloudFallbackSent = true
	n.mu.Unlock()
	slog.Info("native_cloud_fallback_sent", "request", requestTag(p.id))
}

func (n *Native) sendCloudFallback(p *nativeRequest) error {
	const text = "Сейчас не получилось получить ответ."
	_ = n.queueSound(p.ctx, p, sound.Request{Sound: "error"})

	requestID := p.request.GetHeader().GetRequestId()
	if requestID == "" {
		requestID = p.id
	}
	answer := &response.TAliceResponseProto{
		Header: &response.TAliceResponseProto_THeader{
			RequestId:      new(requestID),
			ResponseId:     new(newID()),
			DialogId:       new(p.request.GetHeader().GetDialogId()),
			SequenceNumber: new(p.request.GetHeader().GetSequenceNumber()),
		},
		VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{
			HasVoiceResponse: false,
			ShouldListen:     false,
			OutputSpeech:     &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("")},
		},
		Response: &response.TAliceResponseProto_TResponse{
			Cards: []*response.TAliceResponseProto_TResponse_TCard{{Type: "simple_text", Text: new(text), CardId: new(newID())}},
		},
	}
	answer.Response.Directives = n.takeSound(p)
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id},
		Event:  &server.TServerMessage_TEvent_AliceResponse{AliceResponse: answer},
	}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.down(p.ctx, websocket.MessageBinary, b)
}

func (n *Native) sendCancel(ctx context.Context, id string) error {
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: newID()}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: id, Reason: &cancellation.TCancelRequest_LocalExecution{LocalExecution: &cancellation.TCancelRequest_TReasonLocalExecution{}}}}}}}
	b, err := (wire.Frame{Client: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.up(ctx, websocket.MessageBinary, b)
}
func (n *Native) reply(p *nativeRequest, text string) error {
	if p.streaming {
		return n.finishDeferred(p, text)
	}
	requestID := p.request.GetHeader().GetRequestId()
	if requestID == "" {
		requestID = p.id
	}
	answer := &response.TAliceResponseProto{Header: &response.TAliceResponseProto_THeader{RequestId: new(requestID), ResponseId: new(newID()), DialogId: new(p.request.GetHeader().GetDialogId()), SequenceNumber: new(p.request.GetHeader().GetSequenceNumber())}, VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new(text)}, HasVoiceResponse: true}, Response: &response.TAliceResponseProto_TResponse{Cards: []*response.TAliceResponseProto_TResponse_TCard{{Type: "simple_text", Text: new(text), CardId: new(newID())}}}}
	silentPlayback := n.hasSilentPlayback(p)
	answer.VoiceResponse.ShouldListen = n.ShouldListen && !silentPlayback
	if silentPlayback {
		answer.VoiceResponse.HasVoiceResponse = false
		answer.VoiceResponse.OutputSpeech.Text = new("")
	}
	answer.Response.Directives = n.takeSound(p)
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: answer}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if !p.gated && !p.cloud {
		if err := n.ack(p); err != nil {
			return err
		}
	}
	if err := n.down(p.ctx, websocket.MessageBinary, b); err != nil {
		return err
	}
	n.captureDelivered(p, text, true, silentPlayback || len(answer.Response.Directives) > 0, true)
	if silentPlayback {
		return nil
	}
	return n.synthesize(p, text)
}
func (n *Native) synthesize(p *nativeRequest, text string) error {
	voice, route, id := n.synthesisState(p, text)
	generate := &tts.TTtsGenerate{Text: new(text), Lang: new("ru-RU"), Format: new("audio/opus")}
	voiced := voice.Voice != ""
	if voiced {
		generate.Voice = new(voice.Voice)
		if voice.Emotion != "" {
			generate.Emotion = new(voice.Emotion)
		}
		if voice.Speed > 0 {
			generate.Speed = new(voice.Speed)
		}
		if voice.Volume > 0 {
			generate.Volume = new(voice.Volume)
		}
		slog.Info("native_tts_voice", "request", requestTag(p.id), "route", route, "voice", voice.Voice, "emotion", voice.Emotion, "speed", voice.Speed, "volume", voice.Volume)
	}
	t := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TtsGenerate{TtsGenerate: generate}}}}
	b, err := (wire.Frame{Client: t, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err := n.up(p.ctx, websocket.MessageBinary, b); err != nil {
		return err
	}
	// Arm the watchdog only after the frame is on the wire, so a fast timer can
	// never retry an attempt whose generation request was not sent.
	if voiced {
		n.speechWatchdog(p)
	}
	return nil
}

// synthesisState reads the resolved speech settings and remembers the text, so
// a rejected voice can be retried without it. Fields are written under n.mu.
func (n *Native) synthesisState(p *nativeRequest, text string) (VoiceSettings, string, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	p.speechText = text
	return p.voice, p.route, p.ttsID
}

// speechRetry advances the voice chain after a rejected or silent attempt: the
// next configured candidate and then the default Alice voice. It returns the
// settings to synthesize with, and false when no retry is left. All state is
// guarded by n.mu. A negative attempt skips the generation check.
func (n *Native) speechRetry(p *nativeRequest, attempt int) (VoiceSettings, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if attempt >= 0 && p.speechAttempt != attempt {
		return VoiceSettings{}, false
	}
	if p.canceled || p.speechReady || p.voiceFallback || p.voice.Voice == "" {
		return VoiceSettings{}, false
	}
	if !p.voiceChainReady {
		p.voiceChainReady = true
		p.voiceQueue = n.Voice().candidatesFor(p.route)
	}
	delete(n.tts, p.ttsID)
	p.ttsID = newID()
	n.tts[p.ttsID] = p
	p.speechAttempt++
	if len(p.voiceQueue) > 0 {
		name := p.voiceQueue[0]
		p.voiceQueue = p.voiceQueue[1:]
		p.voice = VoiceSettings{Voice: name, Emotion: p.voice.Emotion, Speed: p.voice.Speed, Volume: p.voice.Volume}
		return p.voice, true
	}
	p.voiceFallback = true
	p.voice = VoiceSettings{}
	return p.voice, true
}

// retrySpeech advances the chain and re-sends the answer. The per-request lock
// keeps a watchdog from racing an EventException into a second synthesis: an
// event that finds the lock taken is either stale or already handled. The
// watchdog waits for the lock instead, so its retry is never dropped.
func (n *Native) retrySpeech(p *nativeRequest, attempt int, event, reason string, wait bool) bool {
	if wait {
		p.speechMu.Lock()
	} else if !p.speechMu.TryLock() {
		return true
	}
	defer p.speechMu.Unlock()
	next, retry := n.speechRetry(p, attempt)
	if !retry {
		return false
	}
	fields := []any{"request", requestTag(p.id), "voice", next.Voice, "default_voice", next.Voice == ""}
	if reason != "" {
		fields = append(fields, "reason", reason)
	}
	if event == "native_tts_retry" {
		slog.Info(event, fields...)
	} else {
		slog.Warn(event, fields...)
	}
	n.mu.Lock()
	text := p.speechText
	n.mu.Unlock()
	if err := n.synthesize(p, text); err != nil {
		slog.Warn("native_tts_retry_failed", "request", requestTag(p.id))
	}
	return true
}

// speechWatchdog falls back to the next voice when the synthesizer accepts the
// request but never starts speaking: a silent rejection would otherwise leave
// the station waiting for audio forever.
func (n *Native) speechWatchdog(p *nativeRequest) {
	n.mu.Lock()
	attempt := p.speechAttempt
	timeout := n.SpeechTimeout
	n.mu.Unlock()
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	time.AfterFunc(timeout, func() {
		n.retrySpeech(p, attempt, "native_tts_timeout", "", true)
	})
}
func requestTag(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum[:4])
}

// statError keeps the station's own wording next to the class: every error that
// is neither a timeout nor a cancellation collapses into "other", so without
// the text a failed turn cannot be explained afterwards. The string comes from
// the device, so control characters are flattened to spaces (a raw newline
// would split a JSONL record if the handler ever runs in text mode) and the
// length is bounded before it reaches the log the operator reads.
func statError(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = statErrorControl.Replace(raw)
	if runes := []rune(raw); len(runes) > 160 {
		raw = string(runes[:160]) + "…"
	}
	return raw
}

var statErrorControl = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

// Quasar uses Yandex TGUID spelling (8-8-8-8), not RFC UUID spelling.
func newID() string {
	return sound.NewStreamID()
}

func (n *Native) makeContinue(p *nativeRequest) ([]byte, error) {
	h := proto.Clone(p.header).(*header.TEventHeader)
	h.MessageId = newID()
	h.RefMessageId = ""
	h.StreamId = nil
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: h, Event: &client.TClientMessage_TEvent_ContinueRequest{ContinueRequest: &continuation.TContinueRequest{RequestMessageId: p.id}}}}}
	return (wire.Frame{Client: m, Prefix: []byte("AAPI")}).Marshal()
}

func (n *Native) ack(p *nativeRequest) error {
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id}, Event: &server.TServerMessage_TEvent_InputStartAck{InputStartAck: &ack.TInputStartAck{RequestStartTime: uint64(time.Now().UnixMilli())}}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.down(p.ctx, websocket.MessageBinary, b)
}

func canReplaceResponse(r *response.TAliceResponseProto) bool {
	for _, d := range r.GetResponse().GetDirectives() {
		if d.GetType() != "client_action" || d.GetName() != "tts_play_placeholder" {
			return false
		}
	}
	return true
}

func (n *Native) isRefusal(text string) bool {
	if detector, ok := n.resolver.(interface{ IsRefusal(string) bool }); ok {
		return detector.IsRefusal(text)
	}
	return IsRefusal(text)
}

func (n *Native) discardStale(p *nativeRequest) {
	n.mu.Lock()
	p.canceled = true
	p.cancel()
	n.mu.Unlock()
	_ = n.sendCancel(n.ctx, p.id)
}

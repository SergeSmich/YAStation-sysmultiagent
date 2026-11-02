package main

import (
	"context"
	stdjson "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/home"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/update"
	"quasarbridge/internal/wire"
)

type probeSession struct {
	*bridge.Native
	onClose   func(*bridge.Native)
	closeOnce sync.Once
}

func (s *probeSession) NativeSession() *bridge.Native { return s.Native }

func (s *probeSession) Close() {
	s.closeOnce.Do(func() {
		s.Native.Close()
		if s.onClose != nil {
			s.onClose(s.Native)
		}
	})
}

func main() {
	path := flag.String("config", "config.json", "configuration file")
	query := flag.String("query", "", "run one text query for backend diagnostics")
	check := flag.Bool("check-backends", false, "check LLM and configured Home Assistant connectivity")
	checkMCP := flag.Bool("check-mcp", false, "connect to configured MCP servers and list tools without executing them")
	route := flag.String("route", "", "explain a route without executing home actions or generating an answer")
	validate := flag.Bool("check-config", false, "validate configuration without contacting backends")
	speakerAction := flag.String("speaker-endpoint", "", "apply or rollback the on-device loopback endpoint (requires writable firmware)")
	speakerConfig := flag.String("speaker-config", "/system/vendor/quasar/quasar.cfg", "firmware configuration path")
	speakerBackup := flag.String("speaker-backup", "/data/opt/quasar-bridge/endpoint-backup.json", "persistent endpoint backup")
	speakerURL := flag.String("speaker-url", "ws://127.0.0.1:8443/uni.ws", "on-device bridge URL")
	panelPasswd := flag.Bool("panel-passwd", false, "set the LAN panel password (reads stdin; generates one on a terminal)")
	panelLogin := flag.String("panel-login", "admin", "panel login name used by -panel-passwd")
	flag.Parse()
	if *panelPasswd {
		panel := defaultPanelConfig()
		if loaded, err := readConfig(*path); err == nil {
			panel = loaded.Panel
		}
		files := panelPaths(*path, panel)
		if err := setPanelPassword(files.Auth, *panelLogin, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *speakerAction != "" {
		if err := setSpeakerEndpoint(*speakerConfig, *speakerBackup, *speakerURL, *speakerAction); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("Speaker endpoint updated; restart the voice service to apply")
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	cfg, err := readConfig(*path)
	if err != nil {
		slog.Error("configuration rejected", "error", err)
		os.Exit(1)
	}

	diagnostics := 0
	if *query != "" {
		diagnostics++
	}
	if *route != "" {
		diagnostics++
	}
	if *check {
		diagnostics++
	}
	if *checkMCP {
		diagnostics++
	}
	if *validate {
		diagnostics++
	}
	if diagnostics > 1 {
		slog.Error("diagnostic flags are mutually exclusive")
		os.Exit(2)
	}
	if *validate {
		fmt.Println("configuration is valid")
		return
	}
	if *checkMCP {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		status, err := assistant.CheckMCP(ctx, cfg.AssistantTools.MCPServers)
		data, _ := json.Marshal(status)
		fmt.Println(string(data))
		if err != nil {
			slog.Error("MCP check failed", "error", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var clientCount, serverCount atomic.Int64
	subscriptions := new(bridge.SubscriptionMonitor)
	var liveNativeMu sync.RWMutex
	var liveNative *bridge.Native
	voice := new(voiceState)
	voice.Store(cfg.LocalVoice)
	liveVoice := func() *bridge.Native {
		liveNativeMu.RLock()
		defer liveNativeMu.RUnlock()
		return liveNative
	}
	var proxy *bridge.Proxy
	proxy = &bridge.Proxy{Subscriptions: subscriptions, Upstream: cfg.Upstream, Observer: func(direction string, kind websocket.MessageType, data []byte) {
		d := wire.ClientToServer
		var count int64
		if direction == "server" {
			proxy.RecordUpstreamFrame()
			d = wire.ServerToClient
			count = serverCount.Add(1)
		} else {
			count = clientCount.Add(1)
		}
		frame := wire.Decode(data, d)
		name := frame.Kind()
		if (name != "opaque" && name != "AsrResult") || count <= 8 || count%100 == 0 {
			attrs := []any{"direction", direction, "transport_type", int(kind), "event", name, "bytes", len(data), "count", count}
			if len(frame.Prefix) == 4 {
				attrs = append(attrs, "prefix_tag", string(frame.Prefix))
			}
			if frame.Client != nil {
				e := frame.Client.GetEvent()
				if req := e.GetTextInput(); req != nil {
					attrs = append(attrs, "wait_client_approval", req.GetRequest().GetUniproxyOptions().GetWaitClientApproval())
				}
				if req := e.GetVoiceInput(); req != nil {
					attrs = append(attrs, "wait_client_approval", req.GetRequest().GetUniproxyOptions().GetWaitClientApproval())
				}
			}
			if name == "opaque" && count <= 8 {
				attrs = append(attrs, "framing", wire.Probe(data, d))
			}
			if name == "opaque" && kind == websocket.MessageText {
				// Keys only: never emit request values, audio or authentication.
				var obj map[string]jsontext.Value
				if json.Unmarshal(data, &obj) == nil {
					var keys []string
					for key := range obj {
						if len(key) < 40 && !strings.ContainsAny(key, "\n\r") {
							keys = append(keys, key)
						}
					}
					attrs = append(attrs, "json_keys", keys)
				}
			}
			slog.Info("frame", attrs...)
		}
	}}
	proxy.UpstreamPingInterval = time.Duration(cfg.UpstreamPing.IntervalMS) * time.Millisecond
	proxy.UpstreamPingTimeout = time.Duration(cfg.UpstreamPing.TimeoutMS) * time.Millisecond
	logs := new(journal.Writer)
	// Credentials live next to the configuration, never inside it: a role reads
	// its provider's key from here at build time.
	secrets := newSecretStore(*path)
	lc := &llm.Client{
		BaseURL: cfg.LLMBaseURL, Model: cfg.LLMModel, APIKey: os.Getenv("QUASAR_LLM_API_KEY"),
		API: cfg.LLMAPI, ReasoningEffort: cfg.LLMReasoningEffort,
		MaxOutputTokens: cfg.LLMMaxOutputTokens, HostedTools: cfg.LLMHostedTools,
		ChatParameters: cfg.LLMChatParameters,
	}
	engine := &bridge.Engine{LLM: lc, Dialogue: dialogue.New(cfg.DialogueContext)}
	var airPlay *airPlayControl
	cleanupAirPlay := sync.OnceFunc(func() { shutdownAirPlay(stop, airPlay) })
	defer cleanupAirPlay()
	if cfg.Mode == "hybrid" {
		airPlay, err = newAirPlayControl(ctx, cfg.AirPlay)
		if err != nil {
			slog.Error("AirPlay configuration rejected", "error", err)
			os.Exit(1)
		}
		if airPlay != nil {
			engine.AirPlay = airPlay
		}
	}
	var musicQueue *assistant.MusicQueue
	var tools *assistant.Tools
	var hc *home.Controller
	if endpoint := os.Getenv("QUASAR_HOME_MCP_URL"); endpoint != "" {
		hc = &home.Controller{Tools: &home.MCP{URL: endpoint}, Planner: lc}
		engine.Home = hc
	}
	if cfg.AssistantTools.Enabled {
		source := func() journal.Config {
			c := logs.Config()
			if c.Path == "" {
				return cfg.ConversationLog
			}
			return c
		}
		reader := assistant.NewWebReader(cfg.AssistantTools)
		if cfg.AssistantTools.PlaySound && cfg.AssistantTools.Web {
			if base := musicBaseURL(cfg); base != "" {
				var err error
				musicQueue, err = assistant.NewMusicQueue(base, reader)
				if err != nil {
					slog.Error("music queue configuration rejected", "error", err)
					os.Exit(1)
				}
			} else {
				slog.Warn("music playback needs assistant_tools.music_base_url reachable by the speaker")
			}
		}
		var musicPlayback *assistant.MusicPlayback
		if musicQueue != nil {
			musicPlayback = assistant.NewMusicPlayback(musicQueue)
			engine.Playback = musicPlayback
		}
		tools = &assistant.Tools{Updater: update.New(*path), Config: cfg.AssistantTools, JournalConfig: source, WebReader: reader, Playback: musicPlayback}
		if airPlay != nil {
			tools.AirPlay = airPlay
		}
		engine.Music = tools
		engine.Assistant = &assistant.Agent{Client: lc, Tools: tools, MaxRounds: cfg.AssistantTools.MaxRounds, MaxCalls: cfg.AssistantTools.MaxCalls}
	}
	if err := engine.SetRouting(cfg.Routing); err != nil {
		slog.Error("routing rejected", "error", err)
		os.Exit(1)
	}
	sysHub := NewSysadminHub(filepath.Dir(*path), nil)
	applyAgents := func(next config) error {
		built, err := buildAgents(next, tools, secrets, sysHub)
		if err != nil {
			return err
		}
		return engine.SetAgents(built)
	}
	if err := applyAgents(cfg); err != nil {
		slog.Error("agents rejected", "error", err)
		os.Exit(1)
	}
	// A role list is logged even when empty: operators need to see that the
	// section was read and that no role is answering.
	slog.Info("agents_ready", "agents", engine.AgentNames(), "providers", providerNames(cfg))
	tgRunner := newTelegramRunner(sysHub, engine, "https://api.telegram.org")
	sysHub.OnTelegramChange(tgRunner.Reload)
	wake := new(wakeState)
	applyWake := func(next config) error {
		wake.set(next.WakeWords)
		return nil
	}
	applyWake(cfg)
	slog.Info("wake_words_ready", "words", wake.load().size(), "dry_run", cfg.WakeWords.DryRun)
	// One hot-apply for the two operator-edited sections: the panel writes them
	// together, so the roles and the words that name them are replaced the same
	// way a restart would read them.
	applySettingsConfig := func(next config) error {
		if err := applyAgents(next); err != nil {
			return err
		}
		applyWake(next)
		// The routing section is compiled and swapped the same way a restart
		// compiles it, so a saved rule starts working without new frames.
		if err := engine.SetRouting(next.Routing); err != nil {
			return err
		}
		return nil
	}
	if *query != "" || *check || *route != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		if *route != "" {
			data, _ := json.Marshal(engine.Decide(ctx, *route))
			fmt.Println(string(data))
			return
		}
		if *check {
			_, err := lc.Complete(ctx, []llm.Message{{Role: "user", Content: "Ответь одним словом: связь"}}, false)
			if err != nil {
				slog.Error("LLM check failed")
				os.Exit(1)
			}
			if hc == nil {
				fmt.Println("LLM: ok; Home Assistant: disabled")
				return
			}
			entities, err := hc.Entities(ctx)
			if err != nil {
				slog.Error("Home Assistant check failed", "error", err)
				os.Exit(1)
			}
			fmt.Printf("LLM: ok; Home Assistant: %d entities\n", len(entities))
			return
		}
		r, err := engine.Resolve(ctx, *query)
		if err != nil {
			slog.Error("query failed", "route", r.Route, "error", err)
			os.Exit(1)
		}
		data, _ := json.Marshal(r)
		fmt.Println(string(data))
		return
	}
	if err := logs.Configure(cfg.ConversationLog); err != nil {
		slog.Error("conversation log rejected", "error", err)
		os.Exit(1)
	}
	defer logs.Close()
	// Restore the operator's level only on the serving path: the diagnostic runs
	// above finish before the bridge owns the speaker, and they must not move it.
	applySavedStationVolume(*path)
	proxy.Journal = logs
	if cfg.Mode == "hybrid" {
		if hc != nil && cfg.HomeWarmArea != "" {
			warmCtx, warmCancel := context.WithCancel(context.Background())
			defer warmCancel()
			go func() {
				for {
					ctx, cancel := context.WithTimeout(warmCtx, 90*time.Second)
					err := hc.Warm(ctx, cfg.HomeWarmArea)
					cancel()
					slog.Info("home_warm", "ready", err == nil)
					select {
					case <-warmCtx.Done():
						return
					case <-time.After(time.Minute):
					}
				}
			}()
		}

		proxy.NewSession = func(ctx context.Context, up, down bridge.Sender) bridge.Session {
			n := bridge.NewNative(ctx, up, down, engine)
			n.GateText = false
			n.VoiceGate = sysHub.VoiceGate(engine)
			n.ShouldListen = cfg.ListenAfterReply
			n.SetVoice(voice.Load())
			n.FirstFrameTimeout = time.Duration(cfg.FirstFrameMS) * time.Millisecond
			n.DeviceContext = cfg.DeviceContext.wire()
			// A role keeps the conversation for as long as the remembered topic
			// lives, so the two bounds never disagree; a disabled dialogue
			// falls back to the package default.
			n.HoldTTL = time.Duration(cfg.DialogueContext.TTLSeconds) * time.Second
			n.OnCloudStall = proxy.RecordUpstreamStall
			if cfg.LocalProgress.Enabled {
				n.ProgressAfter = time.Duration(cfg.LocalProgress.AfterMS) * time.Millisecond
				n.ProgressInterval = time.Duration(cfg.LocalProgress.IntervalMS) * time.Millisecond
			}

			voice.Attach(n)
			liveNativeMu.Lock()
			liveNative = n
			liveNativeMu.Unlock()
			return &probeSession{Native: n, onClose: func(closed *bridge.Native) {
				voice.Detach(closed)
				liveNativeMu.Lock()
				if liveNative == closed {
					// An overlapping newer session may still be open.
					liveNative = voice.Latest()
				}
				liveNativeMu.Unlock()
			}}
		}
	}
	mux := http.NewServeMux()
	if musicQueue != nil {
		mux.Handle("/music-stream/", musicQueue)
	}
	mcpConfigured := 0
	if cfg.Mode == "hybrid" && cfg.AssistantTools.Enabled {
		for _, server := range cfg.AssistantTools.MCPServers {
			if !server.Disabled {
				mcpConfigured++
			}
		}
	}
	mux.HandleFunc("GET /healthz", healthzHandler(cfg, engine, logs, hc, mcpConfigured, musicQueue, airPlay, subscriptions, proxy, voice, wake, &clientCount, &serverCount))
	voiceHandler := voiceConfigHandler(path, voice, liveVoice)
	mux.HandleFunc("GET /api/voice-config", voiceHandler)
	mux.HandleFunc("PUT /api/voice-config", voiceHandler)
	mux.HandleFunc("POST /voice-probe", func(w http.ResponseWriter, r *http.Request) {
		if !requestIsLoopback(r) {
			writeProbeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		var body struct {
			Cases     []bridge.VoiceProbeCase `json:"cases"`
			Names     []string                `json:"names"`
			PerCaseMS int64                   `json:"per_case_ms"`
		}
		decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&body); err != nil {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if len(body.Cases) > 0 && len(body.Names) > 0 {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "use cases or names, not both"})
			return
		}
		cases := body.Cases
		if len(body.Names) > 0 {
			cases = make([]bridge.VoiceProbeCase, 0, len(body.Names))
			for _, name := range body.Names {
				cases = append(cases, bridge.VoiceProbeCase{Voice: name})
			}
		}
		if len(cases) == 0 || len(cases) > 30 {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "cases must contain 1 to 30 entries"})
			return
		}
		liveNativeMu.RLock()
		native := liveNative
		liveNativeMu.RUnlock()
		if native == nil {
			writeProbeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no upstream session"})
			return
		}
		perCaseMS := body.PerCaseMS
		if perCaseMS > 5000 {
			perCaseMS = 5000
		}
		results, err := native.Probe(r.Context(), cases, time.Duration(perCaseMS)*time.Millisecond)
		if err != nil {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeProbeJSON(w, http.StatusOK, map[string]any{"results": results, "count": len(results)})
	})
	injector := func() textInjector {
		liveNativeMu.RLock()
		defer liveNativeMu.RUnlock()
		if liveNative == nil {
			return nil
		}
		return liveNative
	}
	mux.HandleFunc("POST /ask", askHandler(injector))
	mux.HandleFunc("POST /api/wake", wakeHandler(wake.load, injector))
	// The daemon has no callback: the bridge watches its hit list instead.
	poller := newWakePoller(wakeBaseURL(), wakePollInterval(), wake.load, injector)
	mux.Handle("/uni.ws", proxy)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.AllowedIPs) > 0 {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			allowed := false
			for _, candidate := range cfg.AllowedIPs {
				if ip == candidate {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	var panelHTTP *http.Server
	var wakeHTTP *http.Server
	var panelCert, panelKey string
	var metrics *panelMetrics
	if cfg.Panel.Enabled {
		files := panelPaths(*path, cfg.Panel)
		auth, err := newPanelAuthStore(files.Auth)
		if err != nil {
			slog.Error("panel rejected", "error", err)
			os.Exit(1)
		}
		panelCert, panelKey, err = ensurePanelTLS(files.TLS, cfg.Panel.Hosts)
		if err != nil {
			slog.Error("panel certificate failed", "error", err)
			os.Exit(1)
		}
		panelCfg := cfg.Panel
		panelCfg.LogPath = files.Log
		panelCfg.AuditFile = files.Audit
		panelCfg.MetricsFile = files.Metrics
		metrics = newPanelMetrics(files.Metrics)
		snapshot := func() map[string]any {
			return healthSnapshot(cfg, engine, logs, hc, mcpConfigured, musicQueue, airPlay, subscriptions, proxy, voice, wake, &clientCount, &serverCount)
		}
		panel, err := newPanelServer(panelCfg, panelDeps{
			ConfigPath: *path,
			AuthPath:   files.Auth,
			CAPath:     filepath.Join(files.TLS, panelCAName),
			Auth:       auth,
			Snapshot:   snapshot,
			Voice:      voice,
			Journal:    logs.Config,
			Live:       liveVoice,
			Metrics:    metrics,
			Settings:   settingsDeps{Voice: voice, Live: liveVoice, Apply: applySettingsConfig, Secrets: secrets},
			Decide:     engine.Decide,
			SysHub:     sysHub,
		})
		if err != nil {
			slog.Error("panel setup failed", "error", err)
			os.Exit(1)
		}
		if sysHub != nil && panel != nil {
			sysHub.SetAuditor(panelAuditAuditor{a: panel.audit})
		}
		panelHTTP = &http.Server{Addr: cfg.Panel.Listen, Handler: panel, ReadHeaderTimeout: 10 * time.Second}
		// The wake-word training page is served on its own listener with the
		// same certificate and session; the daemon itself is loopback-only.
		if wakePage := panel.WakePageHandler(); wakePage != nil {
			wakeHTTP = &http.Server{Addr: cfg.Panel.WakeProxyListen, Handler: wakePage, ReadHeaderTimeout: 10 * time.Second}
		}
	}
	tgRunner.Reload()
	go poller.run(ctx)
	if metrics != nil {
		go metrics.start(ctx)
	}
	reloads := make(chan os.Signal, 1)
	signal.Notify(reloads, syscall.SIGHUP)
	defer signal.Stop(reloads)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloads:
				hooks := reloadHooks{applyAgents: applyAgents, applyWake: applyWake, logs: logs}
				if err := reloadRouting(*path, cfg, engine, hooks); err != nil {
					slog.Warn("routing_reload_rejected", "error", err)
				} else {
					state := engine.RoutingStatus()
					slog.Info("routing_reloaded", "revision", state.Revision, "active_rules", state.ActiveRules, "agents", engine.AgentNames(), "wake_words", wake.load().size())
				}
				// The voice section is independent of routing: apply it even when
				// the routing part of the file was rejected.
				if _, err := voice.reloadVoice(*path, liveVoice); err != nil {
					slog.Warn("local_voice_reload_failed", "error", err)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if panelHTTP != nil {
			_ = panelHTTP.Shutdown(shutdownCtx)
		}
		if wakeHTTP != nil {
			_ = wakeHTTP.Shutdown(shutdownCtx)
		}
		tgRunner.Stop()
		_ = server.Shutdown(shutdownCtx)
	}()
	if wakeHTTP != nil {
		go func() {
			slog.Info("wake page listening", "address", wakeHTTP.Addr, "upstream", wakeBaseURL(), "tls", true)
			if err := wakeHTTP.ListenAndServeTLS(panelCert, panelKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// The training page is auxiliary: the panel keeps working, and
				// the page offers the SSH tunnel instead.
				slog.Error("wake page listener failed", "error", err)
			}
		}()
	}
	if panelHTTP != nil {
		go func() {
			slog.Info("panel listening", "address", cfg.Panel.Listen, "tls", true)
			if err := panelHTTP.ListenAndServeTLS(panelCert, panelKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// The panel is auxiliary: losing it must not stop the voice bridge.
				slog.Error("panel listener failed", "error", err)
			}
		}()
	}
	slog.Info("listening", "address", cfg.Listen, "mode", cfg.Mode, "routing_revision", engine.RoutingStatus().Revision)
	err = serveBridgeHTTP(server, cfg.TLSCert, cfg.TLSKey, cleanupAirPlay)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("listener failed")
		os.Exit(1)
	}
	// Save synchronously on the way out: the sampler goroutine wakes on ctx.Done
	// as well, but the process must not exit before the last points reach the
	// disk, so the charts survive a restart.
	if metrics != nil {
		metrics.save(true)
	}
}

// healthSnapshot is the single description of the live bridge state: /healthz
// serves it as JSON and the panel shows the same values, so the two views can
// never drift apart.
func healthSnapshot(cfg config, engine *bridge.Engine, logs *journal.Writer, hc *home.Controller, mcpConfigured int, musicQueue *assistant.MusicQueue, airPlay *airPlayControl, subscriptions *bridge.SubscriptionMonitor, proxy *bridge.Proxy, voice *voiceState, wake *wakeState, clientCount, serverCount *atomic.Int64) map[string]any {
	state := engine.RoutingStatus()
	return map[string]any{"status": "ok", "mode": cfg.Mode, "airplay": airPlayHealth(airPlay), "client_frames": clientCount.Load(), "server_frames": serverCount.Load(), "routing_revision": state.Revision, "routing_rules": state.ActiveRules, "agents": engine.AgentNames(), "llm_providers": len(cfg.LLMProviders), "wake_words": wake.status(), "conversation_log_enabled": logs.Enabled(), "conversation_log_failures": logs.Failures(), "assistant_tools_enabled": cfg.AssistantTools.Enabled, "dialogue_context_enabled": cfg.DialogueContext.Enabled && cfg.Mode == "hybrid", "home_assistant_enabled": hc != nil && cfg.Mode == "hybrid", "mcp_servers_configured": mcpConfigured, "music_playback_enabled": musicQueue != nil, "speaker_subscriptions": subscriptions.Snapshot(), "upstream_stalls_total": proxy.UpstreamStallsTotal(), "upstream_ping_failures_total": proxy.UpstreamPingFailuresTotal(), "upstream_last_frame_ms": proxy.UpstreamLastFrameMS(), "local_voice": voice.Load(), "local_voice_revision": voice.Revision()}
}

func healthzHandler(cfg config, engine *bridge.Engine, logs *journal.Writer, hc *home.Controller, mcpConfigured int, musicQueue *assistant.MusicQueue, airPlay *airPlayControl, subscriptions *bridge.SubscriptionMonitor, proxy *bridge.Proxy, voice *voiceState, wake *wakeState, clientCount, serverCount *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(healthSnapshot(cfg, engine, logs, hc, mcpConfigured, musicQueue, airPlay, subscriptions, proxy, voice, wake, clientCount, serverCount))
		_, _ = w.Write(data)
	}
}

func requestIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeProbeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = stdjson.NewEncoder(w).Encode(value)
}

// textInjector is the slice of *bridge.Native that /ask needs: a synthetic frame
// goes through the same Client entry point as a frame from the station.
type textInjector interface {
	Client(context.Context, websocket.MessageType, []byte) error
}

// askHandler lets the station ask a question on its own: it injects a synthetic
// TextInput into the live session, so cloud turns can be exercised without
// speaking. It is loopback-only, because the frame speaks with the station's
// upstream credentials.
func askHandler(native func() textInjector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requestIsLoopback(r) {
			writeProbeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		var body struct {
			Text string `json:"text"`
		}
		decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		if err := decoder.Decode(&body); err != nil {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		frame, id, err := bridge.SyntheticTextInput(body.Text)
		if err != nil {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
		if err := injector.Client(ctx, websocket.MessageBinary, frame); err != nil {
			slog.Error("ask_injected_failed", "request", id, "error", err)
			writeProbeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		slog.Info("ask_injected", "request", id, "text_bytes", len(strings.TrimSpace(body.Text)))
		writeProbeJSON(w, http.StatusOK, map[string]any{"ok": true, "request": id})
	}
}

// Keep resource cleanup inside the serving call: main may exit after a listener
// failure, and os.Exit does not run main's deferred functions.
func serveBridgeHTTP(server *http.Server, cert, key string, cleanup func()) error {
	defer cleanup()
	if cert != "" && key != "" {
		return server.ListenAndServeTLS(cert, key)
	}
	return server.ListenAndServe()
}

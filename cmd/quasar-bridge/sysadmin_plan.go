package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/sysadmin"
)

// One approval per task. The agent states the task as a short plan; the person
// says yes once, and the steps of that plan on that host then run without more
// questions. Anything destructive, sensitive or forbidden still asks.

const (
	planTTL       = 10 * time.Minute
	saveAskTTL    = 2 * time.Minute
	maxActivity   = 8
	maxPlanRunes  = 300
	toolSysPlan   = "sysadmin_plan"
	toolSysLaunch = "sysadmin_launch"
	toolSysFind   = "sysadmin_find_app"
	toolSysSave   = "sysadmin_session_save"
	toolSysLast   = "sysadmin_session_last"
)

type taskPlan struct {
	Host    string
	Summary string
	Until   time.Time
}

func (h *sysadminHub) activatePlan(host, summary string) {
	h.planMu.Lock()
	defer h.planMu.Unlock()
	h.plan = taskPlan{Host: host, Summary: summary, Until: time.Now().Add(planTTL)}
	h.topic, h.topicHost = summary, host
	h.touched = true
}

// planCovers reports whether an approved plan is active for the host.
func (h *sysadminHub) planCovers(host string) bool {
	h.planMu.Lock()
	defer h.planMu.Unlock()
	return h.plan.Host != "" && h.plan.Host == host && time.Now().Before(h.plan.Until)
}

func (h *sysadminHub) planSummary() string {
	h.planMu.Lock()
	defer h.planMu.Unlock()
	return h.plan.Summary
}

func (h *sysadminHub) endPlan() {
	h.planMu.Lock()
	h.plan = taskPlan{}
	h.planMu.Unlock()
}

// touch records that the agent changed something (for the saved session).
func (h *sysadminHub) touch(host, what string) {
	if lw := strings.ToLower(what); isSensitive(what) || strings.ContainsAny(lw, "=") ||
		strings.Contains(lw, "password") || strings.Contains(lw, "passwd") || strings.Contains(lw, "secret") ||
		strings.Contains(lw, "token") || strings.Contains(lw, "api_key") || strings.Contains(lw, "apikey") || strings.Contains(lw, "bearer") {
		what = "(действие с закрытыми данными)"
	}
	h.planMu.Lock()
	defer h.planMu.Unlock()
	h.touched = true
	if h.topic == "" {
		h.topic, h.topicHost = what, host
	}
	h.activity = append(h.activity, host+": "+clip(strings.Join(strings.Fields(what), " "), 90))
	if len(h.activity) > maxActivity {
		h.activity = h.activity[len(h.activity)-maxActivity:]
	}
}

// resetConversation forgets the state of the conversation that ended.
func (h *sysadminHub) resetConversation() {
	h.planMu.Lock()
	h.plan = taskPlan{}
	h.touched = false
	h.saveAsk = time.Time{}
	h.activity, h.topic, h.topicHost = nil, "", ""
	h.planMu.Unlock()
}

// saveConversation stores what was done in this conversation as a session the
// agent can resume later. Called only after the person said yes.
func (h *sysadminHub) saveConversation() error {
	h.planMu.Lock()
	topic, host, act := h.topic, h.topicHost, strings.Join(h.activity, "; ")
	h.planMu.Unlock()
	if topic == "" {
		topic = "Работа с системой"
	}
	if act == "" {
		act = topic
	}
	_, err := h.sessions.Save(topic, act, host, "")
	return err
}

// DialogueEnd takes part when a held sysadmin conversation ends: after changes
// were made, «хватит» first asks whether to save it for a return.
func (h *sysadminHub) DialogueEnd(engine *bridge.Engine) func(ctx context.Context, route, text string, ending bool) (string, bool, bool) {
	return func(ctx context.Context, route, text string, ending bool) (string, bool, bool) {
		if !h.isSysadminRoute(engine, route) {
			return "", false, false
		}
		h.planMu.Lock()
		asked := !h.saveAsk.IsZero() && time.Since(h.saveAsk) < saveAskTTL
		h.saveAsk = time.Time{}
		touched := h.touched
		h.planMu.Unlock()
		if asked {
			yes, ok := sysadmin.ParseYesNo(text)
			if !ok {
				return "", false, false // not an answer: the conversation goes on
			}
			if !yes {
				return "Хорошо, закрываю.", true, true
			}
			if err := h.saveConversation(); err != nil {
				slog.Warn("sysadmin_session_save_failed", "err", err)
				return "Не получилось сохранить. Закрываю.", true, true
			}
			return "Сохранила. Позовите меня, и продолжим.", true, true
		}
		if ending && touched && !h.voiceRequestsWaiting(route) {
			h.planMu.Lock()
			h.saveAsk = time.Now()
			h.planMu.Unlock()
			return "Сохранить, чтобы потом вернуться к этому?", false, true
		}
		return "", false, false
	}
}

// OnRoleReleased closes what the conversation left open: requests nobody
// answered by voice and the approved plan.
func (h *sysadminHub) OnRoleReleased(engine *bridge.Engine) func(route, reason string) {
	return func(route, reason string) {
		if !h.isSysadminRoute(engine, route) {
			return
		}
		name, _ := bridge.ParseAgentRoute(route)
		if reason == "silence" || reason == "ttl" {
			// The person went quiet, perhaps deciding on a request: let it
			// live out its own time instead of dropping it from under them.
			if h.voiceRequestsWaiting(route) {
				return
			}
		}
		n := h.queue.CancelPending(func(it sysadmin.Item) bool {
			return it.ChatID == 0 && voiceRoleOf(it.Origin) == name
		}, "сеанс закончен")
		h.resetConversation()
		if n > 0 {
			slog.Info("sysadmin_pending_cancelled", "count", n, "reason", reason)
		}
	}
}

// voiceRequestsWaiting tells whether this role has unanswered voice requests:
// then «хватит» closes them instead of asking about saving.
func (h *sysadminHub) voiceRequestsWaiting(route string) bool {
	name, _ := bridge.ParseAgentRoute(route)
	for _, it := range h.queue.Pending() {
		if it.ChatID == 0 && voiceRoleOf(it.Origin) == name {
			return true
		}
	}
	return false
}

func (h *sysadminHub) isSysadminRoute(engine *bridge.Engine, route string) bool {
	if engine == nil {
		return false
	}
	name, ok := bridge.ParseAgentRoute(route)
	if !ok {
		return false
	}
	ag, ok := engine.Agent(name)
	return ok && ag.Sysadmin
}

// planCoversStep tells whether an approved plan lets this step run unasked.
func (s *sysadminToolset) planCoversStep(host, command string, destructive, sensitive bool) bool {
	return !destructive && !sensitive && sysadmin.PlanSafe(command) && s.hub.planCovers(host)
}

func (s *sysadminToolset) plan(ctx context.Context, origin, host, summary string) (any, error) {
	if err := s.hostExists(host); err != nil {
		return nil, err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" || len([]rune(summary)) > maxPlanRunes {
		return nil, errors.New("план: от 1 до 300 символов, одной понятной фразой")
	}
	if s.hub.planCovers(host) && s.hub.planSummary() == summary {
		return map[string]string{"status": "already_approved", "note": "план уже одобрен, выполняй шаги"}, nil
	}
	s.hub.Audit("sysadmin_plan", map[string]any{"host": host, "summary": clip(summary, 200), "origin": origin})
	return s.submit(ctx, origin, sysadmin.Action{Tool: "plan", Host: host, Summary: "План: " + summary,
		Run: func(context.Context) (string, error) {
			s.hub.activatePlan(host, summary)
			return "план одобрен: выполняй шаги без вопросов, кроме опасных", nil
		}}), nil
}

func (s *sysadminToolset) launch(ctx context.Context, origin, host, target string) (any, error) {
	if err := s.hostExists(host); err != nil {
		return nil, err
	}
	if !s.hub.HostIsWindows(host) {
		return nil, errors.New("запуск программ работает только на хостах с Windows")
	}
	l, err := sysadmin.ParseLaunch(target)
	if err != nil {
		return nil, err
	}
	pol := s.policy()
	explicit := pol.HasRule("launch", l.Subject)
	mode := pol.Decide("launch", l.Subject)
	if mode == sysadmin.ModeAsk && !explicit {
		switch {
		case l.Safe:
			mode = sysadmin.ModeAllow
			origin += " (список разрешённых)"
		case s.hub.SessionAllowed("launch", host, l.Subject):
			mode = sysadmin.ModeAllow
			origin += " (разрешено в сессии)"
		}
	}
	s.hub.Audit("sysadmin_launch", map[string]any{"host": host, "target": clip(target, 200), "mode": string(mode), "origin": origin})
	run := func(ctx context.Context) (string, error) {
		res, err := s.hub.Exec().Exec(ctx, host, l.LaunchPS(), 30*time.Second)
		if err != nil {
			return "", err
		}
		out := formatExec(res)
		if strings.Contains(out, "NO_INTERACTIVE_USER") {
			return "", errors.New("на ПК никто не вошёл в Windows: открывать программу негде")
		}
		return out, nil
	}
	switch mode {
	case sysadmin.ModeDeny:
		return nil, errors.New("запуск этой программы запрещён политикой")
	case sysadmin.ModeAllow:
		out, err := run(ctx)
		if err != nil {
			return nil, err
		}
		s.hub.touch(host, "запуск "+l.Subject)
		return hostData(out), nil
	}
	res := s.submit(ctx, origin, sysadmin.Action{Tool: "launch", Host: host, Summary: "запустить " + target,
		PolicyTool: "launch", Subject: l.Subject, Run: run})
	if m, ok := res.(map[string]any); ok {
		m["note"] = "Программы нет в списке разрешённых. Спроси человека коротко: «Запустить? И добавить в список, чтобы больше не спрашивать?» " +
			"(ответ «всегда» добавляет в список). Ответ обработает станция сама."
	}
	return res, nil
}

func (s *sysadminToolset) findApp(ctx context.Context, host, query string) (any, error) {
	if err := s.hostExists(host); err != nil {
		return nil, err
	}
	if !s.hub.HostIsWindows(host) {
		return nil, errors.New("поиск программ работает только на хостах с Windows")
	}
	ps, err := sysadmin.FindAppPS(query)
	if err != nil {
		return nil, err
	}
	res, err := s.hub.Exec().Exec(ctx, host, ps, 20*time.Second)
	if err != nil {
		return nil, err
	}
	return hostData(formatExec(res)), nil
}

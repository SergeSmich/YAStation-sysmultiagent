package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/sysadmin"
)

const (
	voiceGateMaxWords = 6
	voiceResultWait   = 25 * time.Second
)

// voiceRoleOf extracts the role name from an item origin such as
// "role:сисадмин (разрешено в сессии)".
func voiceRoleOf(origin string) string {
	name, ok := strings.CutPrefix(origin, "role:")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(name, " \t"); i >= 0 {
		name = name[:i]
	}
	return name
}

// newestVoiceItem picks the latest pending request that nobody is answering in
// Telegram: those are the ones the speaker can decide by voice.
func newestVoiceItem(items []sysadmin.Item) (sysadmin.Item, bool) {
	var best sysadmin.Item
	found := false
	for _, it := range items {
		if it.ChatID != 0 || it.Status != "pending" {
			continue
		}
		if !found || it.Created.After(best.Created) {
			best, found = it, true
		}
	}
	return best, found
}

func countVoiceItems(items []sysadmin.Item) int {
	n := 0
	for _, it := range items {
		if it.ChatID == 0 && it.Status == "pending" {
			n++
		}
	}
	return n
}

// VoiceGate answers a short spoken phrase that decides a pending request. It
// returns ok=false for everything else, so ordinary speech is routed as usual.
func (h *sysadminHub) VoiceGate(engine *bridge.Engine) func(ctx context.Context, text string) (string, string, bool) {
	return func(ctx context.Context, text string) (string, string, bool) {
		pending := h.queue.Pending()
		it, found := newestVoiceItem(pending)
		if !found || len(strings.Fields(text)) > voiceGateMaxWords {
			return "", "", false
		}
		scope, ok := sysadmin.ParseVoiceDecision(text)
		if !ok {
			return "", "", false
		}
		role := voiceRoleOf(it.Origin)
		route := ""
		if role != "" {
			route = bridge.AgentRoute(role)
		}
		if countVoiceItems(pending) > 1 {
			// One short word cannot say which of several requests it means.
			return "Ждёт несколько заявок. Решите их в панели или в Telegram.", route, true
		}
		done, err := h.queue.DecideScope(it.ID, scope, "voice")
		if err != nil {
			slog.Warn("voice_decide_failed", "err", err)
			return "Не получилось: заявка уже закрыта или истекла.", route, true
		}
		if scope == sysadmin.ScopeDeny {
			return "Хорошо, не буду.", route, true
		}
		fin, ok := h.waitFinished(ctx, done.ID, voiceResultWait)
		if !ok {
			return "Выполняю, это займёт время. Результат пришлю в панель.", route, true
		}
		if engine != nil && role != "" {
			actx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			if res, aerr := engine.AskAgent(actx, role, followupText(fin), nil); aerr == nil && strings.TrimSpace(res) != "" {
				return res, route, true
			}
		}
		if fin.Err != "" {
			return "Не получилось: " + fin.Err, route, true
		}
		return "Готово.", route, true
	}
}

// waitFinished polls the queue until the request is done or failed.
func (h *sysadminHub) waitFinished(ctx context.Context, id string, limit time.Duration) (sysadmin.Item, bool) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		if it, ok := h.queue.Result(id); ok && (it.Status == "done" || it.Status == "failed") {
			return it, true
		}
		select {
		case <-ctx.Done():
			return sysadmin.Item{}, false
		case <-deadline.C:
			return sysadmin.Item{}, false
		case <-tick.C:
		}
	}
}

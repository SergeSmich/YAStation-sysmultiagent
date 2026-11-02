package main

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/sysadmin"
)

const (
	voiceGateMaxWords = 6
	voiceResultWait   = 25 * time.Second
	voiceFreshAge     = 5 * time.Second // «разрешить все» skips requests newer than this unless they were read out
	voiceListTTL      = 2 * time.Minute
	voiceHeardMax     = 3 // how many requests the gate reads out by number
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

// voiceItems returns the requests the speaker can decide, oldest first.
func voiceItems(items []sysadmin.Item) []sysadmin.Item {
	var out []sysadmin.Item
	for _, it := range items {
		if it.ChatID == 0 && it.Status == "pending" {
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func clipSpoken(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

// voiceSeveral answers when more than one request waits. «Все» decides all of
// them, but only «разрешить» (once) or «отклонить»: a lasting grant must name its
// request. An ordinal («вторую») decides one. Anything else makes the gate list
// the requests and say how to answer.
func (h *sysadminHub) voiceSeveral(ctx context.Context, engine *bridge.Engine, items []sysadmin.Item, text string, scope sysadmin.Scope, role, route string) (string, string, bool) {
	target := sysadmin.ParseVoiceTarget(text)
	if target.Bad {
		return "Не поняла, о какой заявке речь. Скажите «разрешить все» или «отклонить все».", route, true
	}
	var chosen []sysadmin.Item
	switch {
	case target.All:
		if scope != sysadmin.ScopeOnce && scope != sysadmin.ScopeDeny {
			return "Сразу для всех — только «разрешить все» или «отклонить все». «Всегда» и «в сессии» — по одной заявке.", route, true
		}
		if !sysadmin.HasVoiceVerb(text) {
			return "Скажите «разрешить все» или «отклонить все».", route, true
		}
		if scope == sysadmin.ScopeOnce {
			// Everything allowed must have been read out, or have waited long
			// enough for the speaker to see it in the panel.
			for _, it := range items {
				if !h.voiceWasListed(it.ID) && time.Since(it.Created) < voiceFreshAge {
					return h.voiceAnnounce(items, "Появились новые заявки. "), route, true
				}
			}
		}
		chosen = items
	case target.Index != 0:
		ids := h.voiceHeard()
		if len(ids) == 0 {
			return h.voiceAnnounce(items, ""), route, true
		}
		idx := target.Index
		if idx == -1 {
			idx = len(ids)
		}
		if idx > len(ids) {
			return "Такой заявки нет или я её не называла. Остальные — в панели.", route, true
		}
		found := false
		for _, it := range items {
			if it.ID == ids[idx-1] {
				chosen, found = []sysadmin.Item{it}, true
				break
			}
		}
		if !found {
			return h.voiceAnnounce(items, "Список изменился. "), route, true
		}
	default:
		return h.voiceAnnounce(items, ""), route, true
	}

	var decided []sysadmin.Item
	failed := 0
	for _, it := range chosen {
		done, err := h.queue.DecideScope(it.ID, scope, "voice")
		if err != nil {
			slog.Warn("voice_decide_failed", "err", err)
			failed++
			continue
		}
		decided = append(decided, done)
	}
	if len(decided) == 0 {
		return "Не получилось: заявки уже закрыты или истекли.", route, true
	}
	tail := ""
	if failed > 0 {
		tail += " Закрыто раньше: " + strconv.Itoa(failed) + "."
	}
	if h.telegramWaiting() {
		tail += " Заявки из Telegram ждут там."
	}
	if scope == sysadmin.ScopeDeny {
		return "Хорошо, не буду: отклонено " + strconv.Itoa(len(decided)) + "." + tail, route, true
	}
	// One shared wait: the requests run side by side.
	waitCtx, cancel := context.WithTimeout(ctx, voiceResultWait)
	defer cancel()
	var fins []sysadmin.Item
	for _, d := range decided {
		if fin, ok := h.waitFinished(waitCtx, d.ID, voiceResultWait); ok {
			fins = append(fins, fin)
		}
	}
	if len(fins) < len(decided) {
		return "Выполняю, это займёт время. Результат пришлю в панель.", route, true
	}
	errs := 0
	// One follow-up per role: each agent hears only about its own requests.
	var roles []string
	texts := map[string][]string{}
	for _, f := range fins {
		if f.Err != "" {
			errs++
		}
		r := voiceRoleOf(f.Origin)
		if _, seen := texts[r]; !seen {
			roles = append(roles, r)
		}
		texts[r] = append(texts[r], followupText(f))
	}
	if engine != nil {
		actx, acancel := context.WithTimeout(ctx, 45*time.Second)
		defer acancel()
		var replies []string
		for _, r := range roles {
			if r == "" {
				continue
			}
			if res, aerr := engine.AskAgent(actx, r, strings.Join(texts[r], "\n\n"), nil); aerr == nil && strings.TrimSpace(res) != "" {
				replies = append(replies, strings.TrimSpace(res))
			}
		}
		if len(replies) > 0 {
			return strings.Join(replies, " ") + tail, route, true
		}
	}
	msg := "Готово: " + strconv.Itoa(len(fins)-errs) + " из " + strconv.Itoa(len(fins))
	if errs > 0 {
		msg += ", ошибок " + strconv.Itoa(errs)
	}
	return msg + "." + tail, route, true
}

// voiceAnnounce reads the waiting requests aloud and remembers which ones the
// speaker heard, so that «вторую» and «все» refer to exactly those.
func (h *sysadminHub) voiceAnnounce(items []sysadmin.Item, prefix string) string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	h.voiceMu.Lock()
	h.voiceListed, h.voiceListedAt = ids, time.Now()
	h.voiceMu.Unlock()
	var b strings.Builder
	b.WriteString(prefix + "Ждёт заявок: " + strconv.Itoa(len(items)) + ". ")
	for i, it := range items {
		if i == voiceHeardMax {
			b.WriteString("И другие, они в панели. ")
			break
		}
		b.WriteString(strconv.Itoa(i+1) + ": " + clipSpoken(it.Host+" "+it.Summary, 70) + ". ")
	}
	b.WriteString("Скажите «разрешить все», «отклонить все» или «разрешить первую».")
	return b.String()
}

func (h *sysadminHub) voiceFresh() bool {
	return time.Since(h.voiceListedAt) < voiceListTTL
}

// voiceHeard returns the IDs the speaker can name by number (those read out).
func (h *sysadminHub) voiceHeard() []string {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()
	if !h.voiceFresh() {
		return nil
	}
	ids := h.voiceListed
	if len(ids) > voiceHeardMax {
		ids = ids[:voiceHeardMax]
	}
	return append([]string(nil), ids...)
}

func (h *sysadminHub) voiceWasListed(id string) bool {
	h.voiceMu.Lock()
	defer h.voiceMu.Unlock()
	if !h.voiceFresh() {
		return false
	}
	for _, x := range h.voiceListed {
		if x == id {
			return true
		}
	}
	return false
}

// telegramWaiting reports requests only Telegram can decide.
func (h *sysadminHub) telegramWaiting() bool {
	for _, it := range h.queue.Pending() {
		if it.ChatID != 0 && it.Status == "pending" {
			return true
		}
	}
	return false
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
		if items := voiceItems(pending); len(items) > 1 {
			return h.voiceSeveral(ctx, engine, items, text, scope, role, route)
		}
		if target := sysadmin.ParseVoiceTarget(text); target.Bad || target.Index > 1 {
			return "Такой заявки нет, ждёт одна.", route, true
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

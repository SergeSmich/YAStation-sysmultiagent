package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Одно одобрение на задачу: sysadmin_plan и покрытие шагов.

func callPlan(t *testing.T, ts *sysadminToolset, ctx context.Context, host, summary string) map[string]any {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"host": host, "summary": summary})
	res, err := ts.Call(ctx, toolSysPlan, string(payload))
	if err != nil {
		t.Fatalf("sysadmin_plan: %v", err)
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("ответ плана не разобрать: %v (%s)", err, b)
	}
	return m
}

// approvePlan ставит заявку плана и одобряет её, как это делает человек.
func approvePlan(t *testing.T, ts *sysadminToolset, h *sysadminHub, ctx context.Context, host, summary string) string {
	t.Helper()
	first := callPlan(t, ts, ctx, host, summary)
	if first["status"] != "awaiting_approval" {
		t.Fatalf("план должен ждать одобрения: %v", first)
	}
	id, _ := first["id"].(string)
	if id == "" {
		t.Fatalf("у заявки плана нет id: %v", first)
	}
	it, ok := h.Queue().Result(id)
	if !ok {
		t.Fatal("заявка плана не попала в очередь")
	}
	if it.Tool != "plan" || it.Host != host || it.Status != "pending" {
		t.Fatalf("заявка плана: %+v", it)
	}
	if !strings.Contains(it.Summary, summary) {
		t.Fatalf("суть плана потеряна: %q", it.Summary)
	}
	if _, err := h.Queue().Decide(id, true, "t"); err != nil {
		t.Fatalf("одобрение плана: %v", err)
	}
	if done := waitFinished(t, h, id); done.Status != "done" {
		t.Fatalf("план не применился: %+v", done)
	}
	if !h.planCovers(host) {
		t.Fatal("после одобрения план не действует")
	}
	return id
}

func TestPlanStepRunsWithoutNewRequest(t *testing.T) {
	var runs atomic.Int32
	ts, h := newToolset(t, execServer(t, &runs), "pi")
	ctx := context.Background()
	approvePlan(t, ts, h, ctx, "pi", "создам заметку в /tmp")
	if h.planCovers("pc") {
		t.Fatal("план для pi покрыл другой хост")
	}
	step := callExec(t, ts, ctx, "touch /tmp/qb-plan-note.txt")
	if step["status"] == "awaiting_approval" {
		t.Fatalf("шаг плана снова спросил разрешения: %v", step)
	}
	if _, ok := step["data"]; !ok {
		t.Fatalf("шаг не выполнен, ответ без данных хоста: %v", step)
	}
	if runs.Load() != 1 {
		t.Fatalf("команд выполнено %d, ожидалась 1", runs.Load())
	}
	if pending := h.Queue().Pending(); len(pending) != 0 {
		t.Fatalf("появились лишние заявки: %+v", pending)
	}
}

func TestPlanDoesNotCoverDestructiveOrSensitive(t *testing.T) {
	var runs atomic.Int32
	ts, h := newToolset(t, execServer(t, &runs), "pi")
	ctx := context.Background()
	approvePlan(t, ts, h, ctx, "pi", "наведу порядок в /tmp")

	// Обычный шаг идёт сразу.
	if step := callExec(t, ts, ctx, "touch /tmp/qb-plan-a.txt"); step["status"] == "awaiting_approval" {
		t.Fatalf("обычный шаг спросил: %v", step)
	}
	// Удаление всё равно спрашивает.
	destructive := callExec(t, ts, ctx, "rm /tmp/qb-plan-a.txt")
	if destructive["status"] != "awaiting_approval" {
		t.Fatalf("удаление прошло по плану: %v", destructive)
	}
	if item, _ := h.Queue().Result(destructive["id"].(string)); item.Tool != "ssh_exec" {
		t.Fatalf("заявка удаления: %+v", item)
	}
	// Секретный путь всё равно спрашивает.
	sensitive := callExec(t, ts, ctx, "cat /etc/shadow")
	if sensitive["status"] != "awaiting_approval" {
		t.Fatalf("чтение секрета прошло по плану: %v", sensitive)
	}
	if runs.Load() != 1 {
		t.Fatalf("выполнено %d команд, ожидалась только обычная (1)", runs.Load())
	}
}

func TestPlanWithoutPlanStillAsks(t *testing.T) {
	var runs atomic.Int32
	ts, h := newToolset(t, execServer(t, &runs), "pi")
	ctx := context.Background()
	res := callExec(t, ts, ctx, "touch /tmp/qb-no-plan.txt")
	if res["status"] != "awaiting_approval" {
		t.Fatalf("без плана изменяющая команда должна спрашивать: %v", res)
	}
	if runs.Load() != 0 {
		t.Fatal("команда выполнилась без одобрения")
	}
	if h.planCovers("pi") {
		t.Fatal("план не одобряли, а он действует")
	}
}

func TestPlanExpiresByTTL(t *testing.T) {
	var runs atomic.Int32
	ts, h := newToolset(t, execServer(t, &runs), "pi")
	ctx := context.Background()
	approvePlan(t, ts, h, ctx, "pi", "создам файл")
	// Состариваем план.
	h.planMu.Lock()
	h.plan.Until = time.Now().Add(-time.Second)
	h.planMu.Unlock()
	if h.planCovers("pi") {
		t.Fatal("истёкший план продолжает действовать")
	}
	res := callExec(t, ts, ctx, "touch /tmp/qb-after-ttl.txt")
	if res["status"] != "awaiting_approval" {
		t.Fatalf("после истечения плана нужна новая заявка: %v", res)
	}
	if runs.Load() != 0 {
		t.Fatal("команда выполнилась по истёкшему плану")
	}
}

func TestPlanResetEndsCoverage(t *testing.T) {
	var runs atomic.Int32
	ts, h := newToolset(t, execServer(t, &runs), "pi")
	ctx := context.Background()
	approvePlan(t, ts, h, ctx, "pi", "создам файл")
	if step := callExec(t, ts, ctx, "touch /tmp/qb-plan-before-reset.txt"); step["status"] == "awaiting_approval" {
		t.Fatalf("до сброса шаг спросил: %v", step)
	}
	h.resetConversation()
	if h.planCovers("pi") {
		t.Fatal("resetConversation не сбросил план")
	}
	res := callExec(t, ts, ctx, "touch /tmp/qb-plan-after-reset.txt")
	if res["status"] != "awaiting_approval" {
		t.Fatalf("после сброса сеанса нужна новая заявка: %v", res)
	}
	if runs.Load() != 1 {
		t.Fatalf("выполнено %d команд, ожидалась одна (до сброса)", runs.Load())
	}
}

func TestPlanAlreadyApproved(t *testing.T) {
	var runs atomic.Int32
	ts, h := newToolset(t, execServer(t, &runs), "pi")
	ctx := context.Background()
	approvePlan(t, ts, h, ctx, "pi", "создам файл")
	res := callPlan(t, ts, ctx, "pi", "создам файл")
	if res["status"] != "already_approved" {
		t.Fatalf("повторный план: %v", res)
	}
	if pending := h.Queue().Pending(); len(pending) != 0 {
		t.Fatalf("повторный план создал заявку: %+v", pending)
	}
	// A different plan is a different task: it asks again.
	res = callPlan(t, ts, ctx, "pi", "сделаю что-то другое")
	if res["status"] != "awaiting_approval" {
		t.Fatalf("другой план должен спрашивать: %v", res)
	}
}

func TestPlanValidatesSummaryAndHost(t *testing.T) {
	var runs atomic.Int32
	ts, h := newToolset(t, execServer(t, &runs), "pi")
	ctx := context.Background()
	for _, summary := range []string{"", "   ", strings.Repeat("я", maxPlanRunes+1)} {
		if _, err := ts.Call(ctx, toolSysPlan, `{"host":"pi","summary":`+mustJSON(t, summary)+`}`); err == nil {
			t.Fatalf("пустой/длинный план (%d символов) должен быть отклонён", len([]rune(summary)))
		}
	}
	if _, err := ts.Call(ctx, toolSysPlan, `{"host":"nope","summary":"проверю"}`); err == nil {
		t.Fatal("неизвестный хост должен быть отклонён")
	}
	if pending := h.Queue().Pending(); len(pending) != 0 {
		t.Fatalf("неудачный план попал в очередь: %+v", pending)
	}
	// Ровно 300 символов принимается.
	res := callPlan(t, ts, ctx, "pi", strings.Repeat("я", maxPlanRunes))
	if res["status"] != "awaiting_approval" {
		t.Fatalf("план из 300 символов: %v", res)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

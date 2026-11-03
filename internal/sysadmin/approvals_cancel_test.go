package sysadmin

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CancelPending: при конце сеанса закрываются только «свои» заявки без Telegram.

func okRun(context.Context) (string, error) { return "ok", nil }

func TestCancelPendingOnlyMatching(t *testing.T) {
	q := NewQueue(time.Minute, 20)
	voice := q.Submit(Action{Tool: "t", Origin: "role:сисадмин", Run: okRun})
	plan := q.Submit(Action{Tool: "t", Origin: "role:сисадмин (по плану)", Run: okRun})
	panel := q.Submit(Action{Tool: "t", Origin: "panel", Run: okRun})
	telegram := q.Submit(Action{Tool: "t", Origin: "role:сисадмин", ChatID: 4242, Run: okRun})
	other := q.Submit(Action{Tool: "t", Origin: "role:дом", Run: okRun})

	n := q.CancelPending(func(it Item) bool {
		return it.ChatID == 0 && strings.HasPrefix(it.Origin, "role:сисадмин")
	}, "сеанс закончен")
	if n != 2 {
		t.Fatalf("отменено %d заявок, ожидалось 2", n)
	}
	reason := "сеанс закончен"
	for _, id := range []string{voice, plan} {
		it, ok := q.Result(id)
		if !ok {
			t.Fatalf("%s пропала из очереди", id)
		}
		if it.Status != "denied" {
			t.Fatalf("%s: статус %q, ожидался denied", id, it.Status)
		}
		if it.Note != reason || it.Err != reason {
			t.Fatalf("%s: note=%q err=%q", id, it.Note, it.Err)
		}
	}
	for _, id := range []string{panel, telegram, other} {
		if it, _ := q.Result(id); it.Status != "pending" {
			t.Fatalf("%s: статус %q, должна остаться pending", id, it.Status)
		}
	}
	// Отменённую заявку уже нельзя одобрить.
	if _, err := q.Decide(voice, true, "op"); err == nil {
		t.Fatal("отменённую заявку удалось одобрить")
	}
	// Повторная отмена с тем же фильтром ничего не находит.
	if again := q.CancelPending(func(it Item) bool {
		return it.ChatID == 0 && strings.HasPrefix(it.Origin, "role:сисадмин")
	}, "ещё раз"); again != 0 {
		t.Fatalf("повторная отмена: %d", again)
	}
}

func TestCancelPendingNilMatchCancelsEverything(t *testing.T) {
	q := NewQueue(time.Minute, 20)
	ids := []string{
		q.Submit(Action{Tool: "t", Origin: "role:сисадмин", Run: okRun}),
		q.Submit(Action{Tool: "t", Origin: "panel", Run: okRun}),
		q.Submit(Action{Tool: "t", Origin: "role:сисадмин", ChatID: 7, Run: okRun}),
	}
	if n := q.CancelPending(nil, "общий конец"); n != len(ids) {
		t.Fatalf("отменено %d, ожидалось %d", n, len(ids))
	}
	for _, id := range ids {
		if it, _ := q.Result(id); it.Status != "denied" || it.Note != "общий конец" {
			t.Fatalf("%s: %+v", id, it)
		}
	}
	if pending := q.Pending(); len(pending) != 0 {
		t.Fatalf("остались ожидающие: %+v", pending)
	}
}

func TestCancelPendingLeavesDecidedAlone(t *testing.T) {
	q := NewQueue(time.Minute, 20)
	allowed := q.Submit(Action{Tool: "t", Run: okRun})
	if _, err := q.Decide(allowed, true, "op"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if it, _ := q.Result(allowed); it.Status == "done" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	denied := q.Submit(Action{Tool: "t", Run: okRun})
	if _, err := q.Decide(denied, false, "op"); err != nil {
		t.Fatal(err)
	}
	if n := q.CancelPending(nil, "конец"); n != 0 {
		t.Fatalf("отменено %d уже решённых заявок", n)
	}
	if it, _ := q.Result(allowed); it.Status != "done" || it.Note != "" {
		t.Fatalf("одобренная заявка изменена: %+v", it)
	}
	if it, _ := q.Result(denied); it.Status != "denied" || it.Note != "" || it.Err != "" {
		t.Fatalf("отклонённая заявка изменена: %+v", it)
	}
}

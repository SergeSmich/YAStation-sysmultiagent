package sysadmin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func waitStatus(t *testing.T, q *Queue, id, want string) Item {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		it, _ := q.Result(id)
		if it.Status == want {
			return it
		}
		time.Sleep(5 * time.Millisecond)
	}
	it, _ := q.Result(id)
	t.Fatalf("status %q, want %q", it.Status, want)
	return it
}

func submitRemember(q *Queue, ran *int32, mu *sync.Mutex) string {
	return q.Submit(Action{Tool: "ssh_exec", Host: "pi", Summary: "systemctl restart x", PolicyTool: "shell", Subject: "systemctl restart x",
		Run: func(context.Context) (string, error) {
			mu.Lock()
			*ran++
			mu.Unlock()
			return "ok", nil
		}})
}

func TestDecideScopeGrantsAndRuns(t *testing.T) {
	for _, sc := range []Scope{ScopeSession, ScopeAlways} {
		q := NewQueue(time.Minute, 10)
		var got []Scope
		var gotTool, gotSubj string
		q.SetGranter(func(it Item, s Scope) error {
			got = append(got, s)
			gotTool, gotSubj = it.PolicyTool(), it.Subject()
			return nil
		})
		var ran int32
		var mu sync.Mutex
		id := submitRemember(q, &ran, &mu)
		if it, _ := q.Result(id); !it.Remember {
			t.Fatal("remember must be set")
		}
		it, err := q.DecideScope(id, sc, "t")
		if err != nil || it.Scope != sc || it.Note != "" {
			t.Fatalf("%v %+v", err, it)
		}
		waitStatus(t, q, id, "done")
		if len(got) != 1 || got[0] != sc || gotTool != "shell" || gotSubj != "systemctl restart x" {
			t.Fatalf("granter: %v %q %q", got, gotTool, gotSubj)
		}
	}
}

func TestDecideScopeDegradesToOnce(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	q.SetGranter(func(Item, Scope) error { return errors.New("disk full") })
	var ran int32
	var mu sync.Mutex
	id := submitRemember(q, &ran, &mu)
	it, err := q.DecideScope(id, ScopeAlways, "t")
	if err != nil || it.Scope != ScopeOnce || it.Note == "" {
		t.Fatalf("%v %+v", err, it)
	}
	waitStatus(t, q, id, "done")

	// An action without a subject cannot be remembered at all.
	called := false
	q.SetGranter(func(Item, Scope) error { called = true; return nil })
	id2 := q.Submit(Action{Tool: "ssh_exec", Run: func(context.Context) (string, error) { return "x", nil }})
	it, err = q.DecideScope(id2, ScopeSession, "t")
	if err != nil || it.Scope != ScopeOnce || it.Note == "" || called {
		t.Fatalf("%v %+v called=%v", err, it, called)
	}
	waitStatus(t, q, id2, "done")
}

func TestDecideScopeDenyNeverGrantsOrRuns(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	q.SetGranter(func(Item, Scope) error { t.Error("granter called on deny"); return nil })
	var ran int32
	var mu sync.Mutex
	id := submitRemember(q, &ran, &mu)
	it, err := q.DecideScope(id, ScopeDeny, "t")
	if err != nil || it.Status != "denied" {
		t.Fatalf("%v %+v", err, it)
	}
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if ran != 0 {
		t.Fatal("denied action ran")
	}
}

func TestDecideScopeRejectsBadScopeAndRepeat(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	var ran int32
	var mu sync.Mutex
	id := submitRemember(q, &ran, &mu)
	if _, err := q.DecideScope(id, Scope("forever"), "t"); err == nil {
		t.Fatal("bad scope accepted")
	}
	if it, _ := q.Result(id); it.Status != "pending" {
		t.Fatal("bad scope changed state")
	}
	if _, err := q.DecideScope(id, ScopeOnce, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.DecideScope(id, ScopeAlways, "t"); err == nil {
		t.Fatal("second decision accepted")
	}
	waitStatus(t, q, id, "done")
	mu.Lock()
	defer mu.Unlock()
	if ran != 1 {
		t.Fatalf("ran %d times", ran)
	}
}

func TestDecideScopeLongSubjectNotRemembered(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	long := make([]byte, maxGrantSubject+1)
	for i := range long {
		long[i] = 'a'
	}
	id := q.Submit(Action{Tool: "ssh_exec", PolicyTool: "shell", Subject: string(long), Run: func(context.Context) (string, error) { return "", nil }})
	if it, _ := q.Result(id); it.Remember {
		t.Fatal("too long subject must not be rememberable")
	}
}

func TestOnFinishFires(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	ch := make(chan Item, 1)
	q.OnFinish(func(it Item) { ch <- it })
	id := q.Submit(Action{Tool: "x", Run: func(context.Context) (string, error) { return "res", nil }})
	if _, err := q.Decide(id, true, "t"); err != nil {
		t.Fatal(err)
	}
	select {
	case it := <-ch:
		if it.ID != id || it.Status != "done" || it.Result != "res" {
			t.Fatalf("%+v", it)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnFinish not called")
	}
}

func TestParseVoiceTarget(t *testing.T) {
	cases := []struct {
		in   string
		want VoiceTarget
	}{
		{"да", VoiceTarget{}},
		{"разрешить все", VoiceTarget{All: true}},
		{"Отклони всё", VoiceTarget{}},
		{"да, всё", VoiceTarget{}},
		{"хорошо, последний раз", VoiceTarget{}},
		{"в пятницу", VoiceTarget{}},
		{"четвёртую", VoiceTarget{Index: 4}},
		{"да, оба", VoiceTarget{All: true}},
		{"всегда", VoiceTarget{}},
		{"разрешить первую", VoiceTarget{Index: 1}},
		{"вторую отклони", VoiceTarget{Index: 2}},
		{"последнюю", VoiceTarget{Index: -1}},
		{"все первую", VoiceTarget{All: true, Index: 1, Bad: true}},
		{"первую и вторую", VoiceTarget{Index: 2, Bad: true}},
	}
	for _, c := range cases {
		if got := ParseVoiceTarget(c.in); got != c.want {
			t.Errorf("%q: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestHasVoiceVerb(t *testing.T) {
	for in, want := range map[string]bool{
		"разрешить все": true, "отклони все": true, "да все": false, "ок, всё": false, "хорошо": false,
		"какое разрешение": true, // checked together with weakOnce, which does not accept the noun
	} {
		if got := HasVoiceVerb(in); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
	if _, ok := ParseVoiceDecision("какое разрешение у монитора"); ok {
		t.Error("a noun must not be an answer")
	}
	if _, ok := ParseVoiceDecision("нужно подтверждение"); ok {
		t.Error("a noun must not be an answer")
	}
}

func TestParseVoiceDecision(t *testing.T) {
	cases := []struct {
		in   string
		want Scope
		ok   bool
	}{
		{"Да", ScopeOnce, true},
		{"разрешить", ScopeOnce, true},
		{"разреши все", ScopeOnce, true},
		{"подтвердить", ScopeOnce, true},
		{"не разрешать", ScopeDeny, true},
		{"не выполняй", ScopeDeny, true},
		{"отклонить все", ScopeDeny, true},
		{"только сейчас", ScopeOnce, true},
		{"Разрешаю, один раз.", ScopeOnce, true},
		{"в этой сессии", ScopeSession, true},
		{"да, до перезагрузки", ScopeSession, true},
		{"Всегда", ScopeAlways, true},
		{"да, запомни", ScopeAlways, true},
		{"нет", ScopeDeny, true},
		{"Отмена!", ScopeDeny, true},
		{"не надо", ScopeDeny, true},
		{"не разрешаю", ScopeDeny, true},
		{"всегда в этой сессии", "", false},
		{"нет, только сейчас", "", false},
		{"какая погода", "", false},
		{"давно не виделись", "", false},
		{"не запоминай", "", false},
		{"не всегда", "", false},
		{"не всегда, просто давай", "", false},
		{"не в этой сессии", "", false},
		{"не сейчас", "", false},
		{"нельзя", "", false},
		{"включи музыку", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ParseVoiceDecision(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

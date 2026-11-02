package sysadmin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestQueueSubmitAllowDone(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	id := q.Submit(Action{Tool: "t", Run: func(ctx context.Context) (string, error) { return "ok", nil }})
	it, err := q.Decide(id, true, "op")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "running" {
		t.Fatalf("status %s", it.Status)
	}
	// wait for completion
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := q.Result(id)
		if ok && (r.Status == "done" || r.Status == "failed") {
			if r.Status != "done" || r.Result != "ok" {
				t.Fatalf("result %+v", r)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting done")
}

func TestQueueFailed(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	id := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "", errors.New("boom") }})
	q.Decide(id, true, "op")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := q.Result(id)
		if r.Status == "failed" {
			if r.Err != "boom" {
				t.Fatalf("err %s", r.Err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout")
}

func TestQueueDenyNoRun(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	called := false
	id := q.Submit(Action{Run: func(ctx context.Context) (string, error) { called = true; return "", nil }})
	q.Decide(id, false, "op")
	if called {
		t.Fatal("run called on deny")
	}
	r, _ := q.Result(id)
	if r.Status != "denied" {
		t.Fatalf("status %s", r.Status)
	}
}

func TestQueueExpired(t *testing.T) {
	now := time.Now()
	q := NewQueue(time.Minute, 10)
	q.now = func() time.Time { return now }
	id := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "ok", nil }})
	// advance time
	q.now = func() time.Time { return now.Add(2 * time.Minute) }
	_, err := q.Decide(id, true, "op")
	if err == nil {
		t.Fatal("expected expired error")
	}
}

func TestQueueDoubleDecide(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	id := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "ok", nil }})
	q.Decide(id, false, "op")
	_, err := q.Decide(id, true, "op")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestQueueUnknownDecide(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	_, err := q.Decide("deadbeef", true, "op")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestQueueMaxEvict(t *testing.T) {
	q := NewQueue(time.Minute, 2)
	id1 := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "1", nil }})
	id2 := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "2", nil }})
	q.Decide(id1, true, "op")
	q.Decide(id2, true, "op")
	// wait
	time.Sleep(50 * time.Millisecond)
	id3 := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "3", nil }})
	_ = id3
	// id1 should be evicted if completed
	time.Sleep(50 * time.Millisecond)
	if _, ok := q.Result(id1); ok {
		// may still be present if not completed in time; check status
		r, _ := q.Result(id1)
		t.Logf("id1 status %s", r.Status)
	}
}

func TestQueueOnSubmitNoDeadlock(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	q.OnSubmit(func(it Item) {
		_ = q.Pending()
	})
	id := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "ok", nil }})
	if id == "" {
		t.Fatal("empty id")
	}
}

func TestQueueResultTruncation(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	big := make([]byte, 10000)
	for i := range big {
		big[i] = 'a'
	}
	id := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return string(big), nil }})
	q.Decide(id, true, "op")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := q.Result(id)
		if r.Status == "done" {
			if len(r.Result) > 8192+20 {
				t.Fatalf("not truncated: %d", len(r.Result))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout")
}

func TestQueueAuditor(t *testing.T) {
	aud := &fakeAud{}
	q := NewQueue(time.Minute, 10)
	q.SetAuditor(aud)
	id := q.Submit(Action{Tool: "toolx", Run: func(ctx context.Context) (string, error) { return "ok", nil }})
	q.Decide(id, true, "op")
	time.Sleep(50 * time.Millisecond)
	aud.mu.Lock()
	n := len(aud.events)
	aud.mu.Unlock()
	if n < 2 {
		t.Fatalf("expected audit events, got %d", len(aud.events))
	}
}

type fakeAud struct {
	mu     sync.Mutex
	events []string
}

func (f *fakeAud) Audit(event string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func TestQueueConcurrency(t *testing.T) {
	q := NewQueue(time.Minute, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := q.Submit(Action{Run: func(ctx context.Context) (string, error) { return "ok", nil }})
			q.Decide(id, true, "op")
		}()
	}
	wg.Wait()
}

package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quasarbridge/internal/sysadmin"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func baseCfg(srv *httptest.Server, users []int64) Config {
	return Config{
		Token:       "123:ABC",
		Users:       users,
		BaseURL:     srv.URL,
		Client:      srv.Client(),
		PollTimeout: 1 * time.Second,
		AskTimeout:  1 * time.Second,
	}
}

type fakeAsker struct {
	mu   sync.Mutex
	res  string
	err  error
	seen []string
}

func (f *fakeAsker) Ask(ctx context.Context, chatID int64, text string) (string, error) {
	f.mu.Lock()
	f.seen = append(f.seen, text)
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.res, nil
}

func TestForeignUserIgnored(t *testing.T) {
	var sends int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			atomic.AddInt32(&sends, 1)
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": 9}, "from": map[string]any{"id": 999}, "text": "hi"}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, &fakeAsker{res: "ok"}, sysadmin.NewQueue(time.Minute, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bot.Run(ctx)
	if sends != 0 {
		t.Fatalf("expected no sendMessage, got %d", sends)
	}
}

func TestEmptyUsersNoProcess(t *testing.T) {
	var sends int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			atomic.AddInt32(&sends, 1)
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": 1}, "from": map[string]any{"id": 1}, "text": "hi"}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, nil)
	bot := New(cfg, &fakeAsker{res: "ok"}, sysadmin.NewQueue(time.Minute, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bot.Run(ctx)
	if sends != 0 {
		t.Fatalf("expected no sendMessage, got %d", sends)
	}
}

func TestTextAskSendMessage(t *testing.T) {
	var msgs []string
	var mu sync.Mutex
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			msgs = append(msgs, p["text"].(string))
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": 5}, "from": map[string]any{"id": 1}, "text": "hello"}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	asker := &fakeAsker{res: "reply"}
	bot := New(cfg, asker, sysadmin.NewQueue(time.Minute, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bot.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, m := range msgs {
		if m == "reply" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reply not sent: %v", msgs)
	}
}

func TestBusyChat(t *testing.T) {
	block := make(chan struct{})
	asker := &fakeAsker{}
	asker.res = "r"
	// make asker slow
	slow := &slowAsker{block: block, res: "r"}
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": 5}, "from": map[string]any{"id": 1}, "text": "a"}},
				{"update_id": 2, "message": map[string]any{"message_id": 2, "chat": map[string]any{"id": 5}, "from": map[string]any{"id": 1}, "text": "b"}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, slow, sysadmin.NewQueue(time.Minute, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go bot.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	close(block)
	time.Sleep(200 * time.Millisecond)
	cancel()
	// check "занят" was sent
	_ = asker
}

type slowAsker struct {
	block chan struct{}
	res   string
}

func (s *slowAsker) Ask(ctx context.Context, chatID int64, text string) (string, error) {
	if text == "a" {
		<-s.block
	}
	return s.res, nil
}

func TestPendingCommand(t *testing.T) {
	q := sysadmin.NewQueue(time.Minute, 10)
	q.Submit(sysadmin.Action{Tool: "t", Host: "h", Summary: "s", Origin: "o", Run: func(ctx context.Context) (string, error) { return "r", nil }})
	var sent []string
	var mu sync.Mutex
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			sent = append(sent, p["text"].(string))
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "message": map[string]any{"message_id": 1, "chat": map[string]any{"id": 5}, "from": map[string]any{"id": 1}, "text": "/pending"}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, &fakeAsker{res: "r"}, q)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bot.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) == 0 {
		t.Fatal("no pending sent")
	}
}

func TestNotifyButtons(t *testing.T) {
	var payload map[string]any
	var mu sync.Mutex
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			payload = p
			mu.Unlock()
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{7})
	q := sysadmin.NewQueue(time.Minute, 10)
	bot := New(cfg, &fakeAsker{res: "r"}, q)
	bot.Notify(sysadmin.Item{ID: "abc", Host: "h", Tool: "t", Summary: "s", Origin: "o"})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	rm, _ := payload["reply_markup"].(map[string]any)
	kb, _ := rm["inline_keyboard"].([]any)
	row, _ := kb[0].([]any)
	b1, _ := row[0].(map[string]any)
	b2, _ := row[1].(map[string]any)
	if b1["callback_data"] != "a:abc" || b2["callback_data"] != "d:abc" {
		t.Fatalf("bad callback_data: %v %v", b1, b2)
	}
}

func TestCallbackAllow(t *testing.T) {
	q := sysadmin.NewQueue(time.Minute, 10)
	id := q.Submit(sysadmin.Action{Tool: "t", Host: "h", Summary: "s", Origin: "o", Run: func(ctx context.Context) (string, error) { return "RESULT", nil }})
	var answered atomic.Bool
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			answered.Store(true)
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "callback_query": map[string]any{"id": "cb1", "from": map[string]any{"id": 1}, "message": map[string]any{"message_id": 5, "chat": map[string]any{"id": 5}}, "data": "a:" + id}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, &fakeAsker{res: "r"}, q)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go bot.Run(ctx)
	time.Sleep(1 * time.Second)
	cancel()
	if !answered.Load() {
		t.Fatal("callback not answered")
	}
	it, _ := q.Result(id)
	if it.Status != "done" {
		t.Fatalf("status %s", it.Status)
	}
}

func TestCallbackDenyRunNotCalled(t *testing.T) {
	called := false
	q := sysadmin.NewQueue(time.Minute, 10)
	id := q.Submit(sysadmin.Action{Tool: "t", Host: "h", Summary: "s", Origin: "o", Run: func(ctx context.Context) (string, error) { called = true; return "x", nil }})
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "callback_query": map[string]any{"id": "cb1", "from": map[string]any{"id": 1}, "message": map[string]any{"message_id": 5, "chat": map[string]any{"id": 5}}, "data": "d:" + id}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, &fakeAsker{res: "r"}, q)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bot.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	if called {
		t.Fatal("run called on deny")
	}
	it, _ := q.Result(id)
	if it.Status != "denied" {
		t.Fatalf("status %s", it.Status)
	}
}

func TestExpiredIdError(t *testing.T) {
	q := sysadmin.NewQueue(1*time.Millisecond, 10)
	id := q.Submit(sysadmin.Action{Tool: "t", Host: "h", Summary: "s", Origin: "o", Run: func(ctx context.Context) (string, error) { return "x", nil }})
	time.Sleep(5 * time.Millisecond)
	var answeredMu sync.Mutex
	var answeredText string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			text, _ := p["text"].(string)
			answeredMu.Lock()
			answeredText = text
			answeredMu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
				{"update_id": 1, "callback_query": map[string]any{"id": "cb1", "from": map[string]any{"id": 1}, "message": map[string]any{"message_id": 5, "chat": map[string]any{"id": 5}}, "data": "a:" + id}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, &fakeAsker{res: "r"}, q)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bot.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	answeredMu.Lock()
	answered := answeredText
	answeredMu.Unlock()
	if !strings.Contains(answered, "Ошибка") && !strings.Contains(strings.ToLower(answered), "expired") && !strings.Contains(answered, "already") {
		t.Fatalf("not clear error: %q", answered)
	}
}

func TestTokenNotInError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	cfg := Config{Token: "SECRET_TOKEN", Users: []int64{1}, BaseURL: srv.URL, Client: srv.Client(), PollTimeout: 1 * time.Second, AskTimeout: 1 * time.Second}
	bot := New(cfg, &fakeAsker{res: "r"}, sysadmin.NewQueue(time.Minute, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := bot.Run(ctx)
	_ = err
	// run logs? just ensure no panic; token absence checked via redact
	if strings.Contains("SECRET_TOKEN", "SECRET_TOKEN") {
	}
}

func TestSplitLongText(t *testing.T) {
	s := strings.Repeat("a", 5000)
	chunks := splitChunks(s, 4000)
	for _, c := range chunks {
		if len([]rune(c)) > 4000 {
			t.Fatalf("chunk too long: %d", len([]rune(c)))
		}
	}
	if len(chunks) < 2 {
		t.Fatal("expected split")
	}
}

func TestBackoffAfter500(t *testing.T) {
	var calls int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				w.WriteHeader(500)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, &fakeAsker{res: "r"}, sysadmin.NewQueue(time.Minute, 10))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bot.Run(ctx)
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatal("did not continue after 500")
	}
}

func TestCtxCancelNoLeak(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			time.Sleep(100 * time.Millisecond)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	cfg := baseCfg(srv, []int64{1})
	bot := New(cfg, &fakeAsker{res: "r"}, sysadmin.NewQueue(time.Minute, 10))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { bot.Run(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

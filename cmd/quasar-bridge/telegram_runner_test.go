package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
)

func TestChatHistoryStoreTrim20(t *testing.T) {
	s := newChatHistoryStore()
	for i := 0; i < 25; i++ {
		s.add(1, "user", "m")
	}
	if len(s.get(1)) != 20 {
		t.Fatalf("len=%d", len(s.get(1)))
	}
}

func TestChatHistoryStoreEvictOldestChat(t *testing.T) {
	s := newChatHistoryStore()
	for i := 0; i < 20; i++ {
		s.add(int64(i+1), "user", "m")
	}
	s.add(999, "user", "m")
	if len(s.entries) > 20 {
		t.Fatalf("chats=%d", len(s.entries))
	}
}

func TestChatHistoryStoreTTL(t *testing.T) {
	s := newChatHistoryStore()
	s.add(1, "user", "m")
	s.mu.Lock()
	s.last[1] = time.Now().Add(-2 * time.Hour)
	s.mu.Unlock()
	if len(s.get(1)) != 0 {
		t.Fatalf("ttl not applied")
	}
}

func TestTelegramAskerRoleError(t *testing.T) {
	engine := new(bridge.Engine)
	hub := newHub(t)
	r := newTelegramRunner(hub, engine, "")
	a := &telegramAsker{runner: r, agentName: "sysadmin"}
	if _, err := a.Ask(context.Background(), 1, "hi"); err == nil {
		t.Fatal("expected roleError")
	}
	engine2 := new(bridge.Engine)
	agents := map[string]*bridge.Agent{"sysadmin": {Name: "sysadmin", Answerer: stubCompleter{}, Sysadmin: false}}
	_ = engine2.SetAgents(agents)
	r2 := newTelegramRunner(hub, engine2, "")
	a2 := &telegramAsker{runner: r2, agentName: "sysadmin"}
	if _, err := a2.Ask(context.Background(), 1, "hi"); err == nil {
		t.Fatal("expected roleError for non-sysadmin")
	}
}

func TestTelegramRunnerReloadStartsBot(t *testing.T) {
	var active int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bottoken/getUpdates" {
			atomic.AddInt32(&active, 1)
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	hub := newHub(t)
	_ = hub.SetTelegram(true, []int64{1}, "sysadmin", "123:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", false)
	engine := new(bridge.Engine)
	agents := map[string]*bridge.Agent{"sysadmin": {Name: "sysadmin", Answerer: stubCompleter{}, Sysadmin: true}}
	_ = engine.SetAgents(agents)
	r := newTelegramRunner(hub, engine, srv.URL)
	r.Reload()
	time.Sleep(200 * time.Millisecond)
	if atomic.LoadInt32(&active) > 1 {
		t.Fatalf("active=%d", active)
	}
	r.Reload()
	time.Sleep(200 * time.Millisecond)
	r.Stop()
}

type stubCompleter struct{}

func (stubCompleter) Complete(ctx context.Context, msgs []llm.Message, b bool) (string, error) {
	return "ok", nil
}

func TestTelegramRunnerStopDisablesPolling(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bottoken/getUpdates" {
			atomic.AddInt32(&count, 1)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	hub := newHub(t)
	_ = hub.SetTelegram(true, []int64{1}, "sysadmin", "123:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", false)
	engine := new(bridge.Engine)
	agents := map[string]*bridge.Agent{"sysadmin": {Name: "sysadmin", Answerer: stubCompleter{}, Sysadmin: true}}
	_ = engine.SetAgents(agents)
	r := newTelegramRunner(hub, engine, srv.URL)
	r.Reload()
	time.Sleep(200 * time.Millisecond)
	hub.SetTelegram(false, []int64{1}, "sysadmin", "", true)
	r.Reload()
	time.Sleep(300 * time.Millisecond)
	before := atomic.LoadInt32(&count)
	time.Sleep(200 * time.Millisecond)
	after := atomic.LoadInt32(&count)
	if after-before > 2 {
		t.Fatalf("still polling: before=%d after=%d", before, after)
	}
}

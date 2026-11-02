package main

import (
	"context"
	"sync"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/sysadmin"
	"quasarbridge/internal/telegram"
)

type historyEntry struct {
	role    string
	content string
}

type chatHistoryStore struct {
	mu      sync.Mutex
	entries map[int64][]historyEntry
	last    map[int64]time.Time
}

func newChatHistoryStore() *chatHistoryStore {
	return &chatHistoryStore{entries: map[int64][]historyEntry{}, last: map[int64]time.Time{}}
}

func (s *chatHistoryStore) get(chatID int64) []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.last[chatID]; ok && time.Since(t) > time.Hour {
		delete(s.entries, chatID)
		delete(s.last, chatID)
	}
	out := make([]llm.Message, 0, len(s.entries[chatID]))
	for _, e := range s.entries[chatID] {
		out = append(out, llm.Message{Role: e.role, Content: e.content})
	}
	return out
}

func (s *chatHistoryStore) add(chatID int64, role, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[chatID]; !ok && len(s.entries) >= 20 {
		var oldest int64
		var oldestT time.Time
		first := true
		for k, t := range s.last {
			if first || t.Before(oldestT) {
				oldest = k
				oldestT = t
				first = false
			}
		}
		delete(s.entries, oldest)
		delete(s.last, oldest)
	}
	s.entries[chatID] = append(s.entries[chatID], historyEntry{role: role, content: content})
	if len(s.entries[chatID]) > 20 {
		s.entries[chatID] = s.entries[chatID][len(s.entries[chatID])-20:]
	}
	s.last[chatID] = time.Now()
}

type telegramRunner struct {
	hub     *sysadminHub
	engine  *bridge.Engine
	baseURL string

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	bot    *telegram.Bot
	hist   *chatHistoryStore
}

func newTelegramRunner(hub *sysadminHub, engine *bridge.Engine, baseURL string) *telegramRunner {
	return &telegramRunner{hub: hub, engine: engine, baseURL: baseURL, hist: newChatHistoryStore()}
}

func (r *telegramRunner) Reload() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
	if r.hub == nil {
		return
	}
	tv := r.hub.Telegram()
	token := r.hub.TelegramToken()
	if !tv.Enabled || token == "" || len(tv.Users) == 0 {
		return
	}
	agentName := tv.Agent
	if agentName == "" {
		agentName = "сисадмин"
	}
	asker := &telegramAsker{runner: r, agentName: agentName}
	bot := telegram.New(telegram.Config{Token: token, Users: tv.Users, BaseURL: r.baseURL}, asker, r.hub.Queue())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.bot = bot
	r.cancel = cancel
	r.done = done
	r.hub.Queue().OnSubmit(func(it sysadmin.Item) {
		r.mu.Lock()
		b := r.bot
		r.mu.Unlock()
		if b != nil {
			b.Notify(it)
		}
	})
	go func() {
		defer close(done)
		_ = bot.Run(ctx)
	}()
}

// stopLocked cancels the running bot and waits for it to leave getUpdates, so two
// pollers never hold the same token at once (Telegram answers 409 to the second).
func (r *telegramRunner) stopLocked() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	done := r.done
	r.cancel, r.bot, r.done = nil, nil, nil
	if done != nil {
		select {
		case <-done:
		case <-time.After(35 * time.Second):
		}
	}
}

func (r *telegramRunner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

type telegramAsker struct {
	runner    *telegramRunner
	agentName string
}

func (a *telegramAsker) Ask(ctx context.Context, chatID int64, text string) (string, error) {
	// validate role exists and has sysadmin flag by checking engine agent
	ag, ok := a.runner.engine.Agent(a.agentName)
	if !ok || ag.Answerer == nil || !ag.Sysadmin {
		return "", &roleError{a.agentName}
	}
	hist := a.runner.hist.get(chatID)
	res, err := a.runner.engine.AskAgent(ctx, a.agentName, text, hist)
	if err != nil {
		return "", err
	}
	a.runner.hist.add(chatID, "user", text)
	a.runner.hist.add(chatID, "assistant", res)
	return res, nil
}

type roleError struct{ name string }

func (e *roleError) Error() string {
	return "роль " + e.name + " не настроена или без флага сисадмина"
}

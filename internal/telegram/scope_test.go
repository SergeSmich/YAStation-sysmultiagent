package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"quasarbridge/internal/sysadmin"
)

func TestApprovalKeyboardShapes(t *testing.T) {
	buttons := func(it sysadmin.Item) map[string]string {
		out := map[string]string{}
		rows, _ := approvalKeyboard(it)["inline_keyboard"].([][]map[string]string)
		for _, row := range rows {
			for _, b := range row {
				out[b["callback_data"]] = b["text"]
			}
		}
		return out
	}
	plain := buttons(sysadmin.Item{ID: "x1"})
	if len(plain) != 2 || plain["a:x1"] == "" || plain["d:x1"] == "" {
		t.Fatalf("a request that cannot be remembered gets allow/deny only: %v", plain)
	}
	full := buttons(sysadmin.Item{ID: "x1", Remember: true})
	for _, k := range []string{"a:x1", "s:x1", "w:x1", "d:x1"} {
		if full[k] == "" {
			t.Fatalf("missing button %s: %v", k, full)
		}
	}
}

type followAsker struct {
	fakeAsker
	mu2   sync.Mutex
	items []sysadmin.Item
}

func (f *followAsker) Followup(ctx context.Context, chatID int64, it sysadmin.Item) (string, error) {
	f.mu2.Lock()
	f.items = append(f.items, it)
	f.mu2.Unlock()
	return "ИТОГ-АГЕНТА", nil
}

func runCallback(t *testing.T, q *sysadmin.Queue, asker Asker, data string) []string {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	delivered := false
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			mu.Lock()
			first := !delivered
			delivered = true
			mu.Unlock()
			if first {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{
					{"update_id": 1, "callback_query": map[string]any{"id": "cb1", "from": map[string]any{"id": 1}, "message": map[string]any{"message_id": 5, "chat": map[string]any{"id": 5}}, "data": data}},
				}})
				return
			}
			time.Sleep(50 * time.Millisecond)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
			return
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			text, _ := p["text"].(string)
			mu.Lock()
			sent = append(sent, text)
			mu.Unlock()
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	})
	bot := New(baseCfg(srv, []int64{1}), asker, q)
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	bot.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), sent...)
}

func TestCallbackScopeSessionAndFollowup(t *testing.T) {
	q := sysadmin.NewQueue(time.Minute, 10)
	var granted []sysadmin.Scope
	q.SetGranter(func(it sysadmin.Item, s sysadmin.Scope) error { granted = append(granted, s); return nil })
	id := q.Submit(sysadmin.Action{Tool: "ssh_exec", Host: "h", Summary: "uptime", Origin: "o", PolicyTool: "shell", Subject: "uptime", ChatID: 5,
		Run: func(ctx context.Context) (string, error) { return "RAW-OUTPUT", nil }})
	asker := &followAsker{}
	sent := runCallback(t, q, asker, "s:"+id)
	it, _ := q.Result(id)
	if it.Status != "done" || it.Scope != sysadmin.ScopeSession {
		t.Fatalf("status=%s scope=%q", it.Status, it.Scope)
	}
	asker.mu2.Lock()
	defer asker.mu2.Unlock()
	if len(asker.items) != 1 || asker.items[0].Result != "RAW-OUTPUT" {
		t.Fatalf("agent was not asked to continue: %+v", asker.items)
	}
	joined := strings.Join(sent, "|")
	if !strings.Contains(joined, "ИТОГ-АГЕНТА") {
		t.Fatalf("agent summary not sent: %q", joined)
	}
	if strings.Contains(joined, "RAW-OUTPUT") {
		t.Fatalf("raw output must not replace the agent summary: %q", joined)
	}
}

func TestCallbackForeignChatGetsRawResult(t *testing.T) {
	q := sysadmin.NewQueue(time.Minute, 10)
	id := q.Submit(sysadmin.Action{Tool: "t", Host: "h", Summary: "s", Origin: "o", ChatID: 777,
		Run: func(ctx context.Context) (string, error) { return "RAW-OUTPUT", nil }})
	asker := &followAsker{}
	sent := runCallback(t, q, asker, "a:"+id)
	asker.mu2.Lock()
	n := len(asker.items)
	asker.mu2.Unlock()
	if n != 0 {
		t.Fatal("a request from another chat must not be continued in this one")
	}
	if !strings.Contains(strings.Join(sent, "|"), "RAW-OUTPUT") {
		t.Fatalf("raw result expected: %v", sent)
	}
}

func TestCallbackUnknownKindIgnored(t *testing.T) {
	q := sysadmin.NewQueue(time.Minute, 10)
	called := false
	id := q.Submit(sysadmin.Action{Tool: "t", Host: "h", Summary: "s", Origin: "o",
		Run: func(ctx context.Context) (string, error) { called = true; return "x", nil }})
	runCallback(t, q, &fakeAsker{res: "r"}, "z:"+id)
	if called {
		t.Fatal("unknown callback kind must not run anything")
	}
	if it, _ := q.Result(id); it.Status != "pending" {
		t.Fatalf("status=%s", it.Status)
	}
}

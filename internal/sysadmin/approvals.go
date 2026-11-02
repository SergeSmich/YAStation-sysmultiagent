package sysadmin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Scope says how long an approval holds.
type Scope string

const (
	ScopeOnce    Scope = "once"    // only this request
	ScopeSession Scope = "session" // same tool+host+subject until the bridge restarts
	ScopeAlways  Scope = "always"  // an allow rule is added to the stored policy
	ScopeDeny    Scope = "deny"    // refuse
)

// ParseScope returns the scope for a user-supplied name.
func ParseScope(s string) (Scope, bool) {
	switch Scope(s) {
	case ScopeOnce, ScopeSession, ScopeAlways, ScopeDeny:
		return Scope(s), true
	}
	return "", false
}

// maxGrantSubject bounds what may be remembered as a rule.
const maxGrantSubject = 500

type Action struct {
	Tool    string
	Host    string
	Summary string
	Run     func(ctx context.Context) (string, error)
	Origin  string
	// PolicyTool and Subject describe what a session/always approval would
	// remember (the policy tool name and the exact subject). Empty Subject
	// means the action can only be approved once.
	PolicyTool string
	Subject    string
	// ChatID is the Telegram chat the request came from (0 for voice/panel).
	ChatID int64
}

type Item struct {
	ID       string    `json:"id"`
	Tool     string    `json:"tool"`
	Host     string    `json:"host"`
	Summary  string    `json:"summary"`
	Origin   string    `json:"origin"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	Status   string    `json:"status"`
	Result   string    `json:"result,omitempty"`
	Err      string    `json:"error,omitempty"`
	Remember bool      `json:"remember"`        // session/always are available
	Scope    Scope     `json:"scope,omitempty"` // how it was approved
	Note     string    `json:"note,omitempty"`  // e.g. why a grant was not stored
	ChatID   int64     `json:"chat_id,omitempty"`

	policyTool string
	subject    string
}

// PolicyTool and Subject expose the remembered target to the granter.
func (it Item) PolicyTool() string { return it.policyTool }
func (it Item) Subject() string    { return it.subject }

type Auditor interface {
	Audit(event string, fields map[string]any)
}

type Queue struct {
	ttl   time.Duration
	max   int
	mu    sync.Mutex
	items map[string]*Item
	runs  map[string]func(context.Context) (string, error)
	order []string
	now   func() time.Time
	onSub func(Item)
	audit Auditor
	grant func(Item, Scope) error
	onEnd func(Item)
}

func NewQueue(ttl time.Duration, max int) *Queue {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	if max <= 0 {
		max = 100
	}
	return &Queue{ttl: ttl, max: max, items: map[string]*Item{}, runs: map[string]func(context.Context) (string, error){}, now: time.Now}
}

func (q *Queue) SetAuditor(a Auditor) { q.audit = a }
func (q *Queue) OnSubmit(f func(Item)) { q.onSub = f }

// SetGranter installs the function that stores a session/always approval.
func (q *Queue) SetGranter(f func(Item, Scope) error) { q.grant = f }

// OnFinish registers a callback fired when an approved action completes.
func (q *Queue) OnFinish(f func(Item)) { q.onEnd = f }

func (q *Queue) nowTime() time.Time { return q.now() }

// genID returns an unpredictable 8-hex-digit identifier.
func genID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

func (q *Queue) expireLocked() {
	now := q.nowTime()
	for _, it := range q.items {
		if it.Status == "pending" && now.After(it.Expires) {
			it.Status = "expired"
		}
	}
}

func (q *Queue) Submit(a Action) string {
	q.mu.Lock()
	q.expireLocked()
	id := genID()
	for q.items[id] != nil {
		id = genID()
	}
	now := q.nowTime()
	it := &Item{ID: id, Tool: a.Tool, Host: a.Host, Summary: a.Summary, Origin: a.Origin, Created: now, Expires: now.Add(q.ttl), Status: "pending", ChatID: a.ChatID}
	if a.PolicyTool != "" && a.Subject != "" && len(a.Subject) <= maxGrantSubject {
		it.Remember, it.policyTool, it.subject = true, a.PolicyTool, a.Subject
	}
	q.items[id] = it
	q.runs[id] = a.Run
	q.order = append(q.order, id)
	q.trimLocked()
	cp := *it
	q.mu.Unlock()
	if q.onSub != nil {
		q.onSub(cp)
	}
	if q.audit != nil {
		q.audit.Audit("submit", map[string]any{"id": id, "tool": a.Tool})
	}
	return id
}

func (q *Queue) trimLocked() {
	for len(q.order) > q.max {
		// remove oldest completed item anywhere
		idx := -1
		for i, id := range q.order {
			it := q.items[id]
			if it != nil && (it.Status == "done" || it.Status == "failed" || it.Status == "denied" || it.Status == "expired") {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		id := q.order[idx]
		delete(q.items, id)
		delete(q.runs, id)
		q.order = append(q.order[:idx], q.order[idx+1:]...)
	}
}

func (q *Queue) Pending() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked()
	var out []Item
	for _, id := range q.order {
		it := q.items[id]
		if it == nil {
			continue
		}
		if it.Status == "pending" {
			out = append(out, *it)
		}
	}
	return out
}

// Recent returns up to n decided (non-pending) items, newest first.
func (q *Queue) Recent(n int) []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked()
	var out []Item
	for i := len(q.order) - 1; i >= 0 && len(out) < n; i-- {
		it := q.items[q.order[i]]
		if it != nil && it.Status != "pending" {
			out = append(out, *it)
		}
	}
	return out
}

// Decide approves (once) or refuses a pending item.
func (q *Queue) Decide(id string, allow bool, by string) (Item, error) {
	if allow {
		return q.DecideScope(id, ScopeOnce, by)
	}
	return q.DecideScope(id, ScopeDeny, by)
}

// DecideScope resolves a pending item. For session and always the granter
// stores the permission first; if the item cannot be remembered, or storing
// fails, the approval silently degrades to once and Item.Note says why.
func (q *Queue) DecideScope(id string, scope Scope, by string) (Item, error) {
	if _, ok := ParseScope(string(scope)); !ok {
		return Item{}, fmt.Errorf("unknown scope")
	}
	allow := scope != ScopeDeny
	q.mu.Lock()
	q.expireLocked()
	it, ok := q.items[id]
	if !ok {
		q.mu.Unlock()
		return Item{}, fmt.Errorf("not found")
	}
	if it.Status != "pending" {
		q.mu.Unlock()
		return Item{}, fmt.Errorf("already decided")
	}
	run := q.runs[id]
	if allow {
		it.Status = "running"
	} else {
		it.Status = "denied"
	}
	it.Scope = scope
	// The item is no longer pending, so nobody else can decide it while the
	// granter runs outside the lock.
	snap := *it
	q.mu.Unlock()
	var note string
	if allow && (scope == ScopeSession || scope == ScopeAlways) {
		switch {
		case !snap.Remember || q.grant == nil:
			note = "запомнить это действие нельзя: выполнено один раз"
		default:
			if err := q.grant(snap, scope); err != nil {
				note = "не удалось запомнить (" + err.Error() + "): выполнено один раз"
			}
		}
		if note != "" {
			scope = ScopeOnce
			q.mu.Lock()
			it.Scope, it.Note = ScopeOnce, note
			q.mu.Unlock()
		}
	}
	q.mu.Lock()
	cp := *it
	q.mu.Unlock()
	if q.audit != nil {
		q.audit.Audit("decide", map[string]any{"id": id, "allow": allow, "scope": string(scope), "by": by, "note": note})
	}
	if allow {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := run(ctx)
			if len(res) > 8192 {
				res = res[:8192] + "…усечено"
			}
			q.mu.Lock()
			defer q.mu.Unlock()
			it2 := q.items[id]
			if it2 == nil {
				return
			}
			if err != nil {
				it2.Status = "failed"
				it2.Err = err.Error()
			} else {
				it2.Status = "done"
			}
			it2.Result = res
			fin := *it2
			if q.audit != nil {
				q.audit.Audit("done", map[string]any{"id": id})
			}
			if q.onEnd != nil {
				go q.onEnd(fin)
			}
		}()
	}
	return cp, nil
}

func (q *Queue) Result(id string) (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expireLocked()
	it, ok := q.items[id]
	if !ok {
		return Item{}, false
	}
	return *it, true
}

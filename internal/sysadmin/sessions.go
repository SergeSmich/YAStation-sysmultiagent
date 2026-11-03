package sysadmin

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Session is a saved sysadmin conversation: a few lines the agent wrote itself
// so that a later call can pick the thread up again. Nothing is saved unless
// the person asked for it.
type Session struct {
	ID      string    `json:"id"`
	Topic   string    `json:"topic"`
	Summary string    `json:"summary"`
	Host    string    `json:"host,omitempty"`
	Open    string    `json:"open,omitempty"` // what is left to do
	Saved   time.Time `json:"saved"`
}

const (
	maxSessions       = 5
	maxSessionTopic   = 120
	maxSessionSummary = 600
	maxSessionOpen    = 300
)

// Sessions keeps the saved conversations in a small JSON file.
type Sessions struct {
	mu   sync.Mutex
	path string
	list []Session
	now  func() time.Time
}

// NewSessions loads path (a missing file is an empty list).
func NewSessions(path string) *Sessions {
	s := &Sessions{path: path, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.list)
	}
	return s
}

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Save stores a session; the oldest one is dropped beyond the limit.
func (s *Sessions) Save(topic, summary, host, open string) (Session, error) {
	topic, summary = clipRunes(topic, maxSessionTopic), clipRunes(summary, maxSessionSummary)
	if topic == "" || summary == "" {
		return Session{}, errors.New("нужны тема и краткая суть")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	sess := Session{ID: genID(), Topic: topic, Summary: summary, Host: clipRunes(host, 64), Open: clipRunes(open, maxSessionOpen), Saved: now}
	s.list = append(s.list, sess)
	if len(s.list) > maxSessions {
		s.list = append([]Session(nil), s.list[len(s.list)-maxSessions:]...)
	}
	return sess, s.persistLocked()
}

// Last returns the newest saved session.
func (s *Sessions) Last() (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) == 0 {
		return Session{}, false
	}
	return s.list[len(s.list)-1], true
}

// List returns the saved sessions, newest first.
func (s *Sessions) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.list))
	for i := len(s.list) - 1; i >= 0; i-- {
		out = append(out, s.list[i])
	}
	return out
}

// Drop removes a session by id.
func (s *Sessions) Drop(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.list {
		if x.ID == id {
			s.list = append(s.list[:i:i], s.list[i+1:]...)
			_ = s.persistLocked()
			return true
		}
	}
	return false
}

func (s *Sessions) persistLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.Marshal(s.list)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

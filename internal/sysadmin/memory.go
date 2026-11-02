package sysadmin

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type Fact struct {
	ID    string    `json:"id"`
	Topic string    `json:"topic"`
	Text  string    `json:"text"`
	Time  time.Time `json:"time"`
}

type memoryData struct {
	Passport map[string]string `json:"passport"`
	Facts    []Fact            `json:"facts"`
}

type Memory struct {
	path string
	mu   sync.Mutex
	data memoryData
}

func NewMemory(path string) *Memory {
	m := &Memory{path: path, data: memoryData{Passport: map[string]string{}}}
	_ = m.Load()
	return m
}



func (m *Memory) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			m.data = memoryData{Passport: map[string]string{}}
			return nil
		}
		return err
	}
	var d memoryData
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	if d.Passport == nil {
		d.Passport = map[string]string{}
	}
	m.data = d
	return nil
}

func validText(s string, maxRunes int, allowNewline bool) bool {
	if s == "" {
		return false
	}
	if !utf8.ValidString(s) {
		return false
	}
	n := 0
	for _, r := range s {
		n++
		if n > maxRunes {
			return false
		}
		if r == '\n' {
			if !allowNewline {
				return false
			}
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	// UTF-8 validity is ensured by Go strings; check bytes? We rely on range.
	return true
}

func (m *Memory) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(m.data)
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func (m *Memory) SetPassport(section, text string) error {
	if !validText(section, 64, false) {
		return fmt.Errorf("invalid section")
	}
	if !validText(text, 4000, true) {
		return fmt.Errorf("invalid text")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data.Passport == nil {
		m.data.Passport = map[string]string{}
	}
	if _, ok := m.data.Passport[section]; !ok && len(m.data.Passport) >= 20 {
		return fmt.Errorf("too many passport sections")
	}
	m.data.Passport[section] = text
	return m.saveLocked()
}

func (m *Memory) Passport() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.data.Passport))
	for k, v := range m.data.Passport {
		out[k] = v
	}
	return out
}

func (m *Memory) Remember(topic, text string) (Fact, error) {
	if !validText(topic, 64, false) {
		return Fact{}, fmt.Errorf("invalid topic")
	}
	if !validText(text, 1000, true) {
		return Fact{}, fmt.Errorf("invalid text")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f := Fact{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Topic: topic, Text: text, Time: time.Now().UTC()}
	m.data.Facts = append(m.data.Facts, f)
	if len(m.data.Facts) > 500 {
		m.data.Facts = m.data.Facts[len(m.data.Facts)-500:]
	}
	if err := m.saveLocked(); err != nil {
		return Fact{}, err
	}
	return f, nil
}

func (m *Memory) Forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, f := range m.data.Facts {
		if f.ID == id {
			m.data.Facts = append(m.data.Facts[:i], m.data.Facts[i+1:]...)
			_ = m.saveLocked()
			return true
		}
	}
	return false
}

func (m *Memory) Recall(query string, limit int) []Fact {
	q := strings.ToLower(query)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Fact
	// new first
	for i := len(m.data.Facts) - 1; i >= 0; i-- {
		f := m.data.Facts[i]
		if q == "" || strings.Contains(strings.ToLower(f.Topic+" "+f.Text), q) {
			out = append(out, f)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

func (m *Memory) Summary(maxRunes int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for k, v := range m.data.Passport {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\n")
	}
	if len(m.data.Facts) > 0 {
		b.WriteString("Facts:\n")
		start := 0
		if len(m.data.Facts) > 5 {
			start = len(m.data.Facts) - 5
		}
		for _, f := range m.data.Facts[start:] {
			b.WriteString("- ")
			b.WriteString(f.Topic)
			b.WriteString(": ")
			b.WriteString(f.Text)
			b.WriteString("\n")
		}
	}
	s := b.String()
	runes := []rune(s)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return s
}

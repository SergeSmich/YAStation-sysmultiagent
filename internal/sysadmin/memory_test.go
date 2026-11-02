package sysadmin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestMemoryLimits(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(filepath.Join(dir, "mem.json"))
	if err := m.SetPassport(strings.Repeat("a", 65), "x"); err == nil {
		t.Fatal("section too long")
	}
	if err := m.SetPassport("sec", strings.Repeat("b", 4001)); err == nil {
		t.Fatal("text too long")
	}
	if err := m.SetPassport("sec", "hi"); err != nil {
		t.Fatal(err)
	}
	if len(m.Passport()) != 1 {
		t.Fatal("passport not stored")
	}
	// 21st section rejected
	for i := 0; i < 19; i++ {
		if err := m.SetPassport("s"+string(rune('0'+i)), "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.SetPassport("extra", "x"); err == nil {
		t.Fatal("expected too many sections")
	}
}

func TestMemoryEviction(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(filepath.Join(dir, "mem.json"))
	for i := 0; i < 501; i++ {
		if _, err := m.Remember("t", "fact"); err != nil {
			t.Fatal(err)
		}
	}
	// count facts via recall all
	all := m.Recall("", 10000)
	if len(all) != 500 {
		t.Fatalf("expected 500, got %d", len(all))
	}
}

func TestMemoryForget(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(filepath.Join(dir, "mem.json"))
	f, _ := m.Remember("t", "hello")
	if !m.Forget(f.ID) {
		t.Fatal("forget failed")
	}
	if m.Forget(f.ID) {
		t.Fatal("double forget")
	}
	if len(m.Recall("", 10)) != 0 {
		t.Fatal("still present")
	}
}

func TestMemoryRecall(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(filepath.Join(dir, "mem.json"))
	m.Remember("Topic", "Hello World")
	m.Remember("other", "nothing")
	res := m.Recall("hello", 10)
	if len(res) != 1 {
		t.Fatalf("expected 1, got %d", len(res))
	}
	// case insensitive
	res2 := m.Recall("HELLO", 10)
	if len(res2) != 1 {
		t.Fatalf("case-insensitive fail")
	}
	// new first
	f1, _ := m.Remember("t", "first")
	f2, _ := m.Remember("t", "second")
	r := m.Recall("", 1)
	if len(r) != 1 || r[0].ID != f2.ID {
		t.Fatalf("order wrong: %v", r)
	}
	_ = f1
}

func TestMemorySummary(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(filepath.Join(dir, "mem.json"))
	m.SetPassport("s", strings.Repeat("a", 100))
	s := m.Summary(50)
	runes := []rune(s)
	if len(runes) > 51 {
		t.Fatalf("not truncated: %d", len(runes))
	}
}

func TestMemoryReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mem.json")
	m := NewMemory(p)
	m.Remember("t", "persisted")
	m2 := NewMemory(p)
	all := m2.Recall("", 10)
	if len(all) != 1 {
		t.Fatalf("reload failed: %d", len(all))
	}
}

func TestMemoryCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mem.json")
	os.WriteFile(p, []byte("{bad"), 0600)
	m := NewMemory(p)
	if err := m.Load(); err == nil {
		t.Fatal("expected error")
	}
	// file not overwritten
	b, _ := os.ReadFile(p)
	if string(b) != "{bad" {
		t.Fatal("file overwritten")
	}
}

func TestMemoryFilePerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skip perm check on windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "mem.json")
	m := NewMemory(p)
	m.Remember("t", "x")
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("perm %v", info.Mode().Perm())
	}
}

func TestMemoryConcurrency(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(filepath.Join(dir, "mem.json"))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Remember("t", "concurrent")
		}()
	}
	wg.Wait()
}

func TestMemoryInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	m := NewMemory(filepath.Join(dir, "mem.json"))
	if _, err := m.Remember("t", string([]byte{0xff, 0xfe})); err == nil {
		t.Fatal("expected invalid utf8 rejection")
	}
	if _, err := m.Remember("t", "a\x01b"); err == nil {
		t.Fatal("expected control char rejection")
	}
}

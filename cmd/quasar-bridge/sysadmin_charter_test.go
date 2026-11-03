package main

import (
	"strings"
	"testing"
)

func TestCharterDefaultCustomReset(t *testing.T) {
	h := NewSysadminHub(t.TempDir(), nil)
	if txt, custom := h.Charter(); custom || txt != defaultCharter {
		t.Fatalf("default expected, custom=%v", custom)
	}
	if err := h.SetCharter("Говори коротко."); err != nil {
		t.Fatal(err)
	}
	if txt, custom := h.Charter(); !custom || txt != "Говори коротко." {
		t.Fatalf("custom expected, got %q %v", txt, custom)
	}
	if !strings.Contains(h.CharterPrompt(), "Говори коротко.") {
		t.Fatal("prompt lacks charter")
	}
	if err := h.SetCharter(strings.Repeat("я", maxCharterRunes+1)); err == nil {
		t.Fatal("too long charter accepted")
	}
	if err := h.SetCharter(""); err != nil {
		t.Fatal(err)
	}
	if _, custom := h.Charter(); custom {
		t.Fatal("reset did not restore default")
	}
}

func TestWindowsHostClassification(t *testing.T) {
	h := NewSysadminHub(t.TempDir(), nil)
	if h.HostIsWindows("nope") {
		t.Fatal("unknown host is not windows")
	}
}

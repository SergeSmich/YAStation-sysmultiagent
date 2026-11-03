package assistant

import (
	"strings"
	"testing"
)

func TestLoggedArgumentOnlyForHostTools(t *testing.T) {
	if got := loggedArgument("sysadmin_exec", `{"host":"pc","command":"Get-Process  | Where Name -like '*metro*'"}`); got != "Get-Process | Where Name -like '*metro*'" {
		t.Fatalf("exec argument = %q", got)
	}
	if got := loggedArgument("sysadmin_remember", `{"topic":"x","text":"личное"}`); got != "" {
		t.Fatalf("memory tool logged %q", got)
	}
	if got := loggedArgument("sysadmin_exec", `not json`); got != "" {
		t.Fatalf("bad arguments logged %q", got)
	}
	long := loggedArgument("sysadmin_exec", `{"command":"`+strings.Repeat("a", 500)+`"}`)
	if len([]rune(long)) > 161 {
		t.Fatalf("argument not cut: %d runes", len([]rune(long)))
	}
}

func TestLoggedResultKeepsSecretsOut(t *testing.T) {
	if loggedResultAllowed("sysadmin_read_file", "C:/x.txt") {
		t.Fatal("file contents must not be logged")
	}
	if loggedResultAllowed("sysadmin_exec", "cat /etc/shadow") {
		t.Fatal("secret command result must not be logged")
	}
	if !loggedResultAllowed("sysadmin_exec", "Get-Process | Where Name -like '*metro*'") {
		t.Fatal("process listing should be logged")
	}
}

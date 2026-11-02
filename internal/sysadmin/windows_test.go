package sysadmin

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

func windowsExecutor(t *testing.T, handler func(cmd string, ch ssh.Channel)) (*Executor, func()) {
	t.Helper()
	srv := startSSHServer(t, "secret", handler)
	host, port := splitAddr(srv.addr())
	kh := NewKnownHosts(filepath.Join(t.TempDir(), "kh.json"))
	ex := NewExecutor(func() map[string]Host {
		return map[string]Host{"w": {Addr: host, Port: port, User: "u", Password: "secret", OS: "windows"}}
	}, kh)
	return ex, srv.close
}

func TestExecInvalidUTF8IsCleaned(t *testing.T) {
	// A DOS code page byte, and a multibyte rune cut by the size limit.
	payload := "\x8f\xe0\xa8\xa2\xa5\xe2 " + strings.Repeat("я", 9000)
	srv := startSSHServer(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte(payload))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer srv.close()
	host, port := splitAddr(srv.addr())
	kh := NewKnownHosts(filepath.Join(t.TempDir(), "kh.json"))
	ex := NewExecutor(func() map[string]Host {
		return map[string]Host{"h": {Addr: host, Port: port, User: "u", Password: "secret"}}
	}, kh)
	res, err := ex.Exec(context.Background(), "h", "x", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(res.Stdout) {
		t.Fatalf("stdout is not valid UTF-8")
	}
	if !strings.HasSuffix(res.Stdout, "…усечено") || len(res.Stdout) > 16384+len("…усечено") {
		t.Fatalf("not truncated: %d bytes", len(res.Stdout))
	}
}

func TestTruncateRunes(t *testing.T) {
	s := strings.Repeat("я", 10) // 20 bytes
	for n := 0; n <= 22; n++ {
		if got := truncateRunes(s, n); !utf8.ValidString(got) || len(got) > n {
			t.Fatalf("n=%d got %q", n, got)
		}
	}
}

func TestWindowsExecPrefix(t *testing.T) {
	var mu sync.Mutex
	var seen string
	ex, closeFn := windowsExecutor(t, func(cmd string, ch ssh.Channel) {
		mu.Lock()
		seen = cmd
		mu.Unlock()
		ch.Write([]byte("ok"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer closeFn()
	if _, err := ex.Exec(context.Background(), "w", "hostname", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(seen, "[Console]::OutputEncoding=[Text.Encoding]::UTF8;") || !strings.HasSuffix(seen, "hostname") {
		t.Fatalf("cmd=%q", seen)
	}
}

func TestReadFileWindows(t *testing.T) {
	var mu sync.Mutex
	var seen string
	ex, closeFn := windowsExecutor(t, func(cmd string, ch ssh.Channel) {
		mu.Lock()
		seen = cmd
		mu.Unlock()
		ch.Write([]byte("content"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer closeFn()
	for _, bad := range []string{"rel", "/etc/hosts", "C:", "1:/x", "C:x", `\\evil\share\x`, `\\.\PhysicalDrive0`, `\\?\C:\x`, `C:\a|b`, `C:\a"b`, `C:\a*`, `C:\x:stream`} {
		if _, err := ex.ReadFile(context.Background(), "w", bad, 10); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	out, err := ex.ReadFile(context.Background(), "w", `C:\Users\Admin\it's.txt`, 100)
	if err != nil || out != "content" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(seen, `Open('C:\Users\Admin\it''s.txt','Open','Read','ReadWrite')`) || !strings.Contains(seen, "New-Object byte[] 100") || strings.Contains(seen, "ReadAllBytes") {
		t.Fatalf("cmd=%q", seen)
	}
}

func TestPSQuote(t *testing.T) {
	if got := psQuote("a'b\u2019c"); got != "'a''b\u2019\u2019c'" {
		t.Fatalf("got %q", got)
	}
}

func TestForbiddenWindows(t *testing.T) {
	for _, c := range []string{"Stop-Computer -Force", "Format-Volume -DriveLetter D", "diskpart", "restart-computer", "bcdedit /set x"} {
		if ClassifyShell(c) != RiskForbidden {
			t.Fatalf("%q not forbidden", c)
		}
	}
	if ClassifyShell("Get-Process") == RiskRead {
		t.Fatal("PowerShell must not be auto-read")
	}
}

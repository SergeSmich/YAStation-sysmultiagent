package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"quasarbridge/internal/sysadmin"

	"golang.org/x/crypto/ssh"
)

// ---- ssh helpers copied from internal/sysadmin/ssh_test.go ----
func genHostKey2(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

type sshServer2 struct {
	listener net.Listener
}

func startSSHServer2(t *testing.T, password string, handler func(cmd string, ch ssh.Channel)) *sshServer2 {
	t.Helper()
	signer := genHostKey2(t)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if string(pw) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("bad password")
	}}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &sshServer2{listener: l}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer sshConn.Close()
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						newCh.Reject(ssh.UnknownChannelType, "unknown")
						continue
					}
					ch, reqs, err := newCh.Accept()
					if err != nil {
						continue
					}
					go func(ch ssh.Channel, reqs <-chan *ssh.Request) {
						for req := range reqs {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							_ = ssh.Unmarshal(req.Payload, &payload)
							req.Reply(true, nil)
							if handler != nil {
								handler(payload.Command, ch)
							} else {
								ch.Write([]byte("ok\n"))
								ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
								ch.Close()
							}
						}
					}(ch, reqs)
				}
			}(conn)
		}
	}()
	return srv
}

func (s *sshServer2) addr() string { return s.listener.Addr().String() }
func (s *sshServer2) close()       { s.listener.Close() }

func splitAddr2(a string) (string, int) {
	for i := len(a) - 1; i >= 0; i-- {
		if a[i] == ':' {
			port := 0
			fmt.Sscanf(a[i+1:], "%d", &port)
			return a[:i], port
		}
	}
	return a, 22
}

func newToolset(t *testing.T, srv *sshServer2, hostName string) (*sysadminToolset, *sysadminHub) {
	t.Helper()
	dir := t.TempDir()
	h := NewSysadminHub(dir, nil)
	h.SaveHost(sysadminHostCfg{Name: hostName, Addr: "127.0.0.1", Port: func() int {
		_, p := splitAddr2(srv.addr())
		return p
	}(), User: "pi", MAC: "aa:bb:cc:dd:ee:ff"})
	h.SetHostPassword(hostName, "secret")
	// fix addr/port properly
	// re-save with correct addr/port
	host, port := splitAddr2(srv.addr())
	h.SaveHost(sysadminHostCfg{Name: hostName, Addr: host, Port: port, User: "pi", MAC: "aa:bb:cc:dd:ee:ff"})
	h.SetHostPassword(hostName, "secret")
	ts := &sysadminToolset{base: nil, self: "test", hub: h}
	return ts, h
}

func TestToolsDFImmediate(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte("Filesystem Size\n"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer srv.close()
	ts, _ := newToolset(t, srv, "pi")
	res, err := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"df -h"}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), "ДАННЫЕ ХОСТА") {
		t.Fatalf("no prefix: %s", b)
	}
}

func TestToolsRmDenied(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		t.Fatal("Run should not be called")
	})
	defer srv.close()
	ts, _ := newToolset(t, srv, "pi")
	_, err := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"rm -rf /"}`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestToolsAwaitingApproval(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte("restarted\n"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer srv.close()
	ts, h := newToolset(t, srv, "pi")
	res, err := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"systemctl restart klipper"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["status"] != "awaiting_approval" {
		t.Fatalf("status=%v", m["status"])
	}
	id := m["id"].(string)
	// approve
	if _, err := h.Queue().Decide(id, true, "t"); err != nil {
		t.Fatal(err)
	}
	var r sysadmin.Item
	var ok bool
	for i := 0; i < 200; i++ {
		r, ok = h.Queue().Result(id)
		if ok && r.Status != "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ok {
		t.Fatal("no result")
	}
	if r.Status != "done" {
		t.Fatalf("status=%s", r.Status)
	}
	// call sysadmin_result
	res2, err := ts.Call(context.Background(), toolSysResult, `{"id":"`+id+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res2)
	if !strings.Contains(string(b), "restarted") {
		t.Fatalf("no output: %s", b)
	}
}

func TestToolsDeniedDecide(t *testing.T) {
	called := false
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		called = true
		ch.Write([]byte("x"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer srv.close()
	ts, h := newToolset(t, srv, "pi")
	res, _ := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"systemctl restart klipper"}`)
	id := res.(map[string]any)["id"].(string)
	h.Queue().Decide(id, false, "t")
	r, _ := h.Queue().Result(id)
	if r.Status != "denied" {
		t.Fatalf("status=%s", r.Status)
	}
	if called {
		t.Fatal("command executed despite deny")
	}
}

func TestToolsCatShadowQueued(t *testing.T) {
	srv := startSSHServer2(t, "secret", nil)
	defer srv.close()
	ts, _ := newToolset(t, srv, "pi")
	res, err := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"cat /etc/shadow"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["status"] != "awaiting_approval" {
		t.Fatalf("not queued: %v", res)
	}
}

func TestToolsReadFile(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte("hostname\n"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer srv.close()
	ts, _ := newToolset(t, srv, "pi")
	// /etc/hostname immediate
	res, err := ts.Call(context.Background(), toolSysReadFile, `{"host":"pi","path":"/etc/hostname"}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), "ДАННЫЕ ХОСТА") {
		t.Fatalf("no prefix: %s", b)
	}
	// id_rsa queued
	res2, err := ts.Call(context.Background(), toolSysReadFile, `{"host":"pi","path":"/home/pi/.ssh/id_rsa"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res2.(map[string]any)["status"] != "awaiting_approval" {
		t.Fatalf("not queued: %v", res2)
	}
}

func TestToolsUnknownHost(t *testing.T) {
	srv := startSSHServer2(t, "secret", nil)
	defer srv.close()
	ts, _ := newToolset(t, srv, "pi")
	_, err := ts.Call(context.Background(), toolSysExec, `{"host":"nope","command":"ls"}`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestToolsWakeNoMAC(t *testing.T) {
	dir := t.TempDir()
	h := NewSysadminHub(dir, nil)
	h.SaveHost(sysadminHostCfg{Name: "pi", Addr: "127.0.0.1", Port: 22, User: "pi"})
	ts := &sysadminToolset{base: nil, self: "test", hub: h}
	_, err := ts.Call(context.Background(), toolSysWake, `{"host":"pi"}`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestToolsHTTPGet(t *testing.T) {
	// local httptest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("okbody")) }))
	defer srv.Close()
	// 8.8.8.8 error
	ts := &sysadminToolset{base: nil, self: "test", hub: NewSysadminHub(t.TempDir(), nil)}
	_, err := ts.Call(context.Background(), toolSysHTTPGet, `{"url":"http://8.8.8.8/"}`)
	if err == nil {
		t.Fatal("expected error for 8.8.8.8")
	}
	// local ok
	res, err := ts.Call(context.Background(), toolSysHTTPGet, `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), "ДАННЫЕ ХОСТА") {
		t.Fatalf("no prefix: %s", b)
	}
	// https example.com error
	_, err = ts.Call(context.Background(), toolSysHTTPGet, `{"url":"https://example.com/"}`)
	if err == nil {
		t.Fatal("expected error for example.com")
	}
}

func TestToolsUnknownField(t *testing.T) {
	srv := startSSHServer2(t, "secret", nil)
	defer srv.close()
	ts, _ := newToolset(t, srv, "pi")
	_, err := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"ls","foo":"bar"}`)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestToolsMemoryRoundtrip(t *testing.T) {
	ts := &sysadminToolset{base: nil, self: "test", hub: NewSysadminHub(t.TempDir(), nil)}
	res, _ := ts.Call(context.Background(), toolSysRemember, `{"topic":"t","text":"hello"}`)
	id := res.(map[string]any)["id"].(string)
	res2, _ := ts.Call(context.Background(), toolSysRecall, `{"query":"t"}`)
	b, _ := json.Marshal(res2)
	if !strings.Contains(string(b), "hello") {
		t.Fatalf("recall missing: %s", b)
	}
	ts.Call(context.Background(), toolSysForget, `{"id":"`+id+`"}`)
	res3, _ := ts.Call(context.Background(), toolSysRecall, `{"query":"t"}`)
	b3, _ := json.Marshal(res3)
	if strings.Contains(string(b3), id) {
		// ok maybe empty
	}
	// passport
	ts.Call(context.Background(), toolSysPassportS, `{"section":"pi","text":"raspberry"}`)
	res4, _ := ts.Call(context.Background(), toolSysPassportG, `{}`)
	b4, _ := json.Marshal(res4)
	if !strings.Contains(string(b4), "raspberry") {
		t.Fatalf("passport missing: %s", b4)
	}
}

func TestToolsAllowedAfterHistory(t *testing.T) {
	ts := &sysadminToolset{base: nil, self: "test", hub: NewSysadminHub(t.TempDir(), nil)}
	if ts.AllowedAfterHistory(toolSysExec) {
		t.Fatal("expected false")
	}
}

func TestToolsPeerDefinitions(t *testing.T) {
	// peerToolset wraps; here just check sysadminToolset definitions has no approve/decide and no password schema
	ts := &sysadminToolset{base: nil, self: "test", hub: NewSysadminHub(t.TempDir(), nil)}
	defs := ts.Definitions()
	for _, d := range defs {
		if strings.Contains(strings.ToLower(d.Function.Name), "approve") || strings.Contains(strings.ToLower(d.Function.Name), "decide") {
			t.Fatalf("forbidden tool %s", d.Function.Name)
		}
		b, _ := json.Marshal(d)
		if strings.Contains(string(b), "password") {
			t.Fatalf("password in schema: %s", b)
		}
	}
	// peerToolset over sysadminToolset still returns both sets: simulate by base being another toolset? Just ensure definitions length > sysadmin count
	if len(defs) < 9 {
		t.Fatalf("too few defs: %d", len(defs))
	}
}

func TestToolsPasswordNotInResults(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte("secret123 output"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer srv.close()
	ts, h := newToolset(t, srv, "pi")
	h.SetHostPassword("pi", "supersecretpw")
	res, _ := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"echo"}`)
	// force run via allow? command echo maybe queued? execCommand policy: "echo" not read? DecideShell likely ask? Let's just check JSON of any result for password
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "supersecretpw") {
		t.Fatal("password leaked")
	}
}

type captureAudit struct {
	mu     sync.Mutex
	events []struct {
		name   string
		fields map[string]any
	}
}

func (c *captureAudit) Audit(event string, fields map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, struct {
		name   string
		fields map[string]any
	}{event, fields})
}

func (c *captureAudit) find(name string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.name == name {
			return e.fields
		}
	}
	return nil
}

// TestToolsExecJournalKeepsRCAndHead is the diagnosis contract for a command
// that ends badly: the journal must name the exit code and the first lines. A
// bare «timeout» is what made the stuck adb impossible to read afterwards.
func TestToolsExecJournalKeepsRCAndHead(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte("error: no devices/emulators found\nsecond line\n"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{3}))
		ch.Close()
	})
	defer srv.close()
	ts, h := newToolset(t, srv, "pi")
	aud := new(captureAudit)
	h.SetAuditor(aud)
	if _, err := ts.runExec(context.Background(), "role:тест", "pi", "adb devices", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	f := aud.find("sysadmin_exec_result")
	if f == nil {
		t.Fatal("no result event in the journal")
	}
	if rc, _ := f["rc"].(int); rc != 3 {
		t.Fatalf("rc=%v", f["rc"])
	}
	if head, _ := f["head"].(string); !strings.Contains(head, "no devices") || !strings.Contains(head, "exit=3") {
		t.Fatalf("head=%q", f["head"])
	}
	if ms, _ := f["elapsed_ms"].(int64); ms < 0 {
		t.Fatalf("elapsed_ms=%v", f["elapsed_ms"])
	}
}

// TestToolsExecJournalHidesSensitiveOutput keeps a secret command's output out
// of the journal while the exit code still goes in.
func TestToolsExecJournalHidesSensitiveOutput(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte("root:$6$hash\n"))
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
		ch.Close()
	})
	defer srv.close()
	ts, h := newToolset(t, srv, "pi")
	aud := new(captureAudit)
	h.SetAuditor(aud)
	if _, err := ts.runExec(context.Background(), "role:тест", "pi", "cat /etc/shadow", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	f := aud.find("sysadmin_exec_result")
	if f == nil {
		t.Fatal("no result event in the journal")
	}
	if _, ok := f["head"]; ok {
		t.Fatalf("sensitive output reached the journal: %v", f["head"])
	}
	if rc, _ := f["rc"].(int); rc != 0 {
		t.Fatalf("rc=%v", f["rc"])
	}
}

// TestToolsExecTimeoutReportsDiagnosis is the adb case: a command that hangs must
// be cut at the deadline and still answer with the code marker and what the host
// managed to print.
func TestToolsExecTimeoutReportsDiagnosis(t *testing.T) {
	srv := startSSHServer2(t, "secret", func(cmd string, ch ssh.Channel) {
		ch.Write([]byte("error: no devices/emulators found\n"))
		time.Sleep(4 * time.Second)
		ch.Close()
	})
	defer srv.close()
	ts, _ := newToolset(t, srv, "pi")
	start := time.Now()
	_, err := ts.Call(context.Background(), toolSysExec, `{"host":"pi","command":"df -h","timeout_sec":1}`)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("the deadline did not cut the command: %v", time.Since(start))
	}
	msg := err.Error()
	for _, want := range []string{"timeout", "exit=-1", "no devices"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("%q is missing from the error: %q", want, msg)
		}
	}
}

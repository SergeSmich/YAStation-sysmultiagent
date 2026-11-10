package sysadmin

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

type Host struct {
	Name     string
	Addr     string
	Port     int
	User     string
	Password string
	KeyPath  string
	// OS is "windows" for a host whose SSH shell is PowerShell; anything else
	// is treated as a POSIX shell.
	OS string
}

// IsWindows reports whether commands for the host are PowerShell.
func (h Host) IsWindows() bool { return strings.EqualFold(h.OS, "windows") }

// powershellUTF8 makes PowerShell write UTF-8 instead of the OEM code page.
// chcp switches native programs (ipconfig, ping) to UTF-8 as well.
const powershellUTF8 = "[Console]::OutputEncoding=[Text.Encoding]::UTF8; chcp 65001 > $null; $ProgressPreference='SilentlyContinue'; "

// truncateRunes cuts s to at most n bytes without splitting a rune.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut says the command was stopped at the deadline and the host never
	// reported an exit status (ExitCode is -1). What the command printed before
	// the deadline is kept: it is the only diagnosis of a hang, and «timeout»
	// alone made a stuck adb undebuggable.
	TimedOut bool
}

// outputLimit is how much of a command's output is kept in total. It also caps
// the buffer at run time, so a chatty or hung command cannot grow the bridge's
// memory while the deadline is not yet reached.
const outputLimit = 16384

// cappedBuffer is the writer for command output. The timeout path reads it while
// the copy goroutines may still be writing, so every access is locked, and the
// buffer stops growing past its limit.
type cappedBuffer struct {
	mu    sync.Mutex
	b     strings.Builder
	limit int
	cut   bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	if limit <= 0 {
		limit = outputLimit
	}
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if left := c.limit - c.b.Len(); left > 0 {
		if len(p) > left {
			c.b.Write(p[:left])
			c.cut = true
		} else {
			c.b.Write(p)
		}
	} else if len(p) > 0 {
		c.cut = true
	}
	return len(p), nil
}

// String returns the collected text without splitting a rune, with a marker when
// the limit was reached.
func (c *cappedBuffer) String() string {
	c.mu.Lock()
	raw := c.b.String()
	cut := c.cut
	c.mu.Unlock()
	out := string([]rune(strings.ToValidUTF8(raw, "\uFFFD")))
	if cut {
		out += "…усечено"
	}
	return out
}

type KnownHosts struct {
	path string
	mu   sync.Mutex
	data map[string]string
}

func NewKnownHosts(path string) *KnownHosts {
	kh := &KnownHosts{path: path, data: map[string]string{}}
	_ = kh.load()
	return kh
}

func (k *KnownHosts) load() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, err := os.ReadFile(k.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &k.data)
}

func (k *KnownHosts) save() error {
	b, err := json.Marshal(k.data)
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}

func (k *KnownHosts) Check(host, fp string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if v, ok := k.data[host]; ok {
		if v != fp {
			return fmt.Errorf("отпечаток хоста изменился")
		}
		return nil
	}
	k.data[host] = fp
	b, err := json.Marshal(k.data)
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}

type Dialer interface {
	Dial(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error)
}

type defaultDialer struct{}

func (defaultDialer) Dial(network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	return ssh.Dial(network, addr, config)
}

type Executor struct {
	hosts  func() map[string]Host
	known  *KnownHosts
	dialer Dialer
}

func NewExecutor(hosts func() map[string]Host, known *KnownHosts) *Executor {
	return &Executor{hosts: hosts, known: known, dialer: defaultDialer{}}
}

func (e *Executor) SetDialer(d Dialer) { e.dialer = d }

func (e *Executor) Exec(ctx context.Context, hostName, command string, timeout time.Duration) (ExecResult, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}
	h, ok := e.hosts()[hostName]
	if !ok {
		return ExecResult{}, fmt.Errorf("unknown host")
	}
	port := h.Port
	if port == 0 {
		port = 22
	}
	addr := fmt.Sprintf("%s:%d", h.Addr, port)
	config := &ssh.ClientConfig{
		User:    h.User,
		Auth:    authMethods(h),
		Timeout: timeout,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			if e.known != nil {
				return e.known.Check(hostName+" "+addr, fp)
			}
			return nil
		},
	}
	client, err := e.dialer.Dial("tcp", addr, config)
	if err != nil {
		return ExecResult{}, err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer session.Close()
	var stdout = newCappedBuffer(outputLimit)
	var stderr = newCappedBuffer(outputLimit)
	session.Stdout = stdout
	session.Stderr = stderr
	done := make(chan error, 1)
	runCmd := command
	if h.IsWindows() {
		runCmd = powershellUTF8 + command
	}
	go func() { done <- session.Run(runCmd) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return collectExec(stdout, stderr, -1, true), fmt.Errorf("timeout: %w", ctx.Err())
	case <-time.After(timeout):
		_ = session.Signal(ssh.SIGKILL)
		return collectExec(stdout, stderr, -1, true), fmt.Errorf("timeout: команда не ответила за %s", timeout)
	case err := <-done:
		code := 0
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				code = exitErr.ExitStatus()
			} else {
				return collectExec(stdout, stderr, -1, false), err
			}
		}
		return collectExec(stdout, stderr, code, false), nil
	}
}

// collectExec turns the collected output into a result. The output is copied at
// most once, and the combined text keeps the size limit of a tool answer.
func collectExec(out, errs *cappedBuffer, code int, timedOut bool) ExecResult {
	o, e := out.String(), errs.String()
	if len(o)+len(e) > outputLimit {
		total := truncateRunes(o+e, outputLimit) + "…усечено"
		o, e = total, ""
	}
	return ExecResult{Stdout: o, Stderr: e, ExitCode: code, TimedOut: timedOut}
}

func authMethods(h Host) []ssh.AuthMethod {
	var methods []ssh.AuthMethod
	if h.Password != "" {
		methods = append(methods, ssh.Password(h.Password))
		methods = append(methods, ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
			ans := make([]string, len(questions))
			for i := range ans {
				ans[i] = h.Password
			}
			return ans, nil
		}))
	}
	if h.KeyPath != "" {
		if key, err := os.ReadFile(h.KeyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(key); err == nil {
				methods = append(methods, ssh.PublicKeys(signer))
			}
		}
	}
	return methods
}

func (e *Executor) ReadFile(ctx context.Context, host, pathStr string, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		maxBytes = 65536
	}
	if strings.Contains(pathStr, "\x00") || strings.Contains(pathStr, "\n") || strings.Contains(pathStr, "\r") {
		return "", fmt.Errorf("invalid path")
	}
	var cmd string
	if h, ok := e.hosts()[host]; ok && h.IsWindows() {
		if !isWindowsAbsPath(pathStr) {
			return "", fmt.Errorf("path must be absolute (C:/dir/file)")
		}
		// Read at most maxBytes: a huge file (pagefile, VHDX) must not be loaded whole.
		cmd = fmt.Sprintf("$f=[IO.File]::Open(%s,'Open','Read','ReadWrite'); try { $b=New-Object byte[] %d; $t=0; "+
			"while ($t -lt %d) { $r=$f.Read($b,$t,%d-$t); if ($r -le 0) { break }; $t+=$r }; "+
			"[Text.Encoding]::UTF8.GetString($b,0,$t) } finally { $f.Close() }",
			psQuote(pathStr), maxBytes, maxBytes, maxBytes)
	} else {
		if !strings.HasPrefix(pathStr, "/") {
			return "", fmt.Errorf("path must be absolute")
		}
		cmd = fmt.Sprintf("head -c %d -- %s", maxBytes, shellQuote(pathStr))
	}
	res, err := e.Exec(ctx, host, cmd, 30*time.Second)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s", res.Stderr)
	}
	return res.Stdout, nil
}

// isWindowsAbsPath accepts only local drive paths: C:\dir or C:/dir. UNC and
// device paths are refused (an SMB request would leak the PC's credentials),
// as are characters that are not valid in a Windows path.
func isWindowsAbsPath(p string) bool {
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
		return false
	}
	if c := p[0]; !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	return !strings.ContainsAny(p[2:], "\"<>|*?:")
}

// psQuote makes a PowerShell single-quoted literal. PowerShell also treats the
// typographic single quotes as quotes, so each of them is doubled as well.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '\u2018', '\u2019', '\u201A', '\u201B':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (e *Executor) WakeOnLAN(mac, broadcast string) error {
	packet, err := BuildMagicPacket(mac)
	if err != nil {
		return err
	}
	if broadcast == "" {
		broadcast = "255.255.255.255"
	}
	conn, err := net.Dial("udp", broadcast+":9")
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(packet)
	return err
}

func BuildMagicPacket(mac string) ([]byte, error) {
	mac = strings.ReplaceAll(mac, ":", "")
	mac = strings.ReplaceAll(mac, "-", "")
	if len(mac) != 12 {
		return nil, fmt.Errorf("invalid mac")
	}
	b, err := hexDecode(mac)
	if err != nil {
		return nil, err
	}
	packet := make([]byte, 102)
	for i := 0; i < 6; i++ {
		packet[i] = 0xFF
	}
	for i := 0; i < 16; i++ {
		copy(packet[6+i*6:], b)
	}
	return packet, nil
}

func hexDecode(s string) ([]byte, error) {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var v byte
		_, err := fmt.Sscanf(s[i:i+2], "%02x", &v)
		if err != nil {
			return nil, err
		}
		b[i/2] = v
	}
	return b, nil
}

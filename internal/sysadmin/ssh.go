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

	"golang.org/x/crypto/ssh"
)

type Host struct {
	Name     string
	Addr     string
	Port     int
	User     string
	Password string
	KeyPath  string
}

type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
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
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return ExecResult{}, ctx.Err()
	case <-time.After(timeout):
		_ = session.Signal(ssh.SIGKILL)
		return ExecResult{}, fmt.Errorf("timeout")
	case err := <-done:
		out := stdout.String()
		errs := stderr.String()
		if len(out)+len(errs) > 16384 {
			total := out + errs
			total = total[:16384] + "…усечено"
			out = total
			errs = ""
		}
		code := 0
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				code = exitErr.ExitStatus()
			} else {
				return ExecResult{Stdout: out, Stderr: errs}, err
			}
		}
		return ExecResult{Stdout: out, Stderr: errs, ExitCode: code}, nil
	}
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
	if !strings.HasPrefix(pathStr, "/") {
		return "", fmt.Errorf("path must be absolute")
	}
	if strings.Contains(pathStr, "\x00") || strings.Contains(pathStr, "\n") || strings.Contains(pathStr, "\r") {
		return "", fmt.Errorf("invalid path")
	}
	cmd := fmt.Sprintf("head -c %d -- %s", maxBytes, shellQuote(pathStr))
	res, err := e.Exec(ctx, host, cmd, 30*time.Second)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%s", res.Stderr)
	}
	return res.Stdout, nil
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

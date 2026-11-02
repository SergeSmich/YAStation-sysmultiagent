package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"sort"
	"strings"
	"sync"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/sysadmin"
)

// Sysadmin settings live in their own files next to config.json so that an
// older binary (which rejects unknown config members) keeps working after a
// rollback. Host passwords are kept in a separate 0600 file.

type sysadminHostCfg struct {
	Name    string `json:"name"`
	Addr    string `json:"addr"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	KeyPath string `json:"key_path"`
	MAC     string `json:"mac"`
	// OS is "windows" (PowerShell over SSH) or "" / "linux" (POSIX shell).
	OS string `json:"os,omitempty"`
}

type sysadminRuleCfg struct {
	Tool  string `json:"tool"`
	Match string `json:"match"`
	Mode  string `json:"mode"`
}

type sysadminPolicyCfg struct {
	Default string            `json:"default"`
	Rules   []sysadminRuleCfg `json:"rules"`
}

type sysadminTelegramCfg struct {
	Enabled bool    `json:"enabled"`
	Users   []int64 `json:"users"`
	Agent   string  `json:"agent"`
}

type sysadminCfg struct {
	Hosts    []sysadminHostCfg   `json:"hosts"`
	Policy   sysadminPolicyCfg   `json:"policy"`
	Telegram sysadminTelegramCfg `json:"telegram"`
}

type sysadminSecretsCfg struct {
	Hosts         map[string]string `json:"hosts"`
	TelegramToken string            `json:"telegram_token"`
}

// hostView is the panel/model-safe view of a host (never contains a password).
type hostView struct {
	Name        string `json:"name"`
	Addr        string `json:"addr"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	KeyPath     string `json:"key_path"`
	MAC         string `json:"mac"`
	OS          string `json:"os,omitempty"`
	HasPassword bool   `json:"has_password"`
}

type telegramView struct {
	Enabled  bool    `json:"enabled"`
	Users    []int64 `json:"users"`
	Agent    string  `json:"agent"`
	HasToken bool    `json:"has_token"`
}

type fileStamp struct {
	mod  time.Time
	size int64
	ok   bool
}

type sysadminHub struct {
	dir   string
	mem   *sysadmin.Memory
	queue *sysadmin.Queue
	known *sysadmin.KnownHosts
	exec  *sysadmin.Executor

	mu               sync.Mutex
	auditor          sysadmin.Auditor
	cfg              sysadminCfg
	cfgStamp         fileStamp
	secrets          sysadminSecretsCfg
	secStamp         fileStamp
	loadErr          error
	onTelegramChange func()

	sessMu  sync.Mutex
	session map[string]struct{}

	// The list of requests the voice gate last read out, oldest first: «вторую»
	// and «все» refer to what the speaker heard, not to a queue that moved.
	voiceMu       sync.Mutex
	voiceListed   []string
	voiceListedAt time.Time
}

// NewSysadminHub creates the long-lived sysadmin state rooted at dir.
func NewSysadminHub(dir string, aud sysadmin.Auditor) *sysadminHub {
	h := &sysadminHub{dir: dir, auditor: aud}
	h.mem = sysadmin.NewMemory(filepath.Join(dir, "sysadmin_memory.json"))
	_ = h.mem.Load()
	h.queue = sysadmin.NewQueue(2*time.Minute, 50)
	h.queue.SetAuditor(aud)
	h.queue.SetGranter(h.grant)
	h.known = sysadmin.NewKnownHosts(filepath.Join(dir, "sysadmin_known_hosts.json"))
	h.exec = sysadmin.NewExecutor(h.execHosts, h.known)
	return h
}

func (h *sysadminHub) cfgPath() string { return filepath.Join(h.dir, "sysadmin.json") }
func (h *sysadminHub) secPath() string { return filepath.Join(h.dir, "sysadmin_secrets.json") }

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fileStamp{}, nil
		}
		return fileStamp{}, err
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size(), ok: true}, nil
}

// loadLocked refreshes cfg and secrets when the files changed on disk.
func (h *sysadminHub) loadLocked() error {
	cs, err := stampOf(h.cfgPath())
	if err != nil {
		h.loadErr = err
		return err
	}
	if cs != h.cfgStamp || h.loadErr != nil {
		var c sysadminCfg
		if cs.ok {
			b, err := os.ReadFile(h.cfgPath())
			if err != nil {
				h.loadErr = err
				return err
			}
			if err := json.Unmarshal(b, &c); err != nil {
				h.loadErr = fmt.Errorf("sysadmin.json: %w", err)
				return h.loadErr
			}
		}
		h.cfg = c
		h.cfgStamp = cs
	}
	ss, err := stampOf(h.secPath())
	if err != nil {
		h.loadErr = err
		return err
	}
	if ss != h.secStamp || h.loadErr != nil {
		s := sysadminSecretsCfg{}
		if ss.ok {
			b, err := os.ReadFile(h.secPath())
			if err != nil {
				h.loadErr = err
				return err
			}
			if err := json.Unmarshal(b, &s); err != nil {
				h.loadErr = fmt.Errorf("sysadmin_secrets.json: %w", err)
				return h.loadErr
			}
		}
		if s.Hosts == nil {
			s.Hosts = map[string]string{}
		}
		h.secrets = s
		h.secStamp = ss
	}
	if h.secrets.Hosts == nil {
		h.secrets.Hosts = map[string]string{}
	}
	h.loadErr = nil
	return nil
}

// Err reports the current configuration load error, if any.
func (h *sysadminHub) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loadLocked()
}

// execHosts feeds the SSH executor; passwords never leave this process.
func (h *sysadminHub) execHosts() map[string]sysadmin.Host {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]sysadmin.Host{}
	if h.loadLocked() != nil {
		return out
	}
	for _, hc := range h.cfg.Hosts {
		out[hc.Name] = sysadmin.Host{Name: hc.Name, Addr: hc.Addr, Port: hc.Port, User: hc.User,
			Password: h.secrets.Hosts[hc.Name], KeyPath: hc.KeyPath, OS: hc.OS}
	}
	return out
}

func (h *sysadminHub) Hosts() []hostView {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loadLocked() != nil {
		return nil
	}
	out := make([]hostView, 0, len(h.cfg.Hosts))
	for _, hc := range h.cfg.Hosts {
		out = append(out, hostView{Name: hc.Name, Addr: hc.Addr, Port: hc.Port, User: hc.User,
			KeyPath: hc.KeyPath, MAC: hc.MAC, OS: hc.OS, HasPassword: h.secrets.Hosts[hc.Name] != ""})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HostMAC returns the configured MAC for Wake-on-LAN.
func (h *sysadminHub) HostMAC(name string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loadLocked() != nil {
		return "", false
	}
	for _, hc := range h.cfg.Hosts {
		if hc.Name == name && hc.MAC != "" {
			return hc.MAC, true
		}
	}
	return "", false
}

func validHostName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

func validateHost(hc *sysadminHostCfg) error {
	if !validHostName(hc.Name) {
		return errors.New("имя хоста: латиница, цифры, - и _, до 32 символов")
	}
	hc.Addr = strings.TrimSpace(hc.Addr)
	if hc.Addr == "" || strings.ContainsAny(hc.Addr, " /\\\t\r\n") {
		return errors.New("адрес хоста пуст или содержит недопустимые символы")
	}
	if hc.Port == 0 {
		hc.Port = 22
	}
	if hc.Port < 1 || hc.Port > 65535 {
		return errors.New("порт вне диапазона 1-65535")
	}
	if strings.TrimSpace(hc.User) == "" || strings.ContainsAny(hc.User, " \t\r\n") {
		return errors.New("пользователь пуст или содержит пробелы")
	}
	hc.OS = strings.ToLower(strings.TrimSpace(hc.OS))
	if hc.OS != "" && hc.OS != "linux" && hc.OS != "windows" {
		return errors.New("ОС хоста: linux или windows")
	}
	if hc.MAC != "" {
		if _, err := sysadmin.BuildMagicPacket(hc.MAC); err != nil {
			return errors.New("MAC-адрес некорректен")
		}
	}
	if hc.KeyPath != "" && !filepath.IsAbs(hc.KeyPath) && !strings.HasPrefix(hc.KeyPath, "/") {
		return errors.New("путь к ключу должен быть абсолютным")
	}
	if net.ParseIP(hc.Addr) == nil && len(hc.Addr) > 253 {
		return errors.New("адрес слишком длинный")
	}
	return nil
}

// SaveHost inserts or replaces a host definition.
func (h *sysadminHub) SaveHost(hc sysadminHostCfg) error {
	if err := validateHost(&hc); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	replaced := false
	for i := range h.cfg.Hosts {
		if h.cfg.Hosts[i].Name == hc.Name {
			// The panel form does not send the OS; keep what is configured.
			if hc.OS == "" {
				hc.OS = h.cfg.Hosts[i].OS
			}
			h.cfg.Hosts[i] = hc
			replaced = true
		}
	}
	if !replaced {
		if len(h.cfg.Hosts) >= 32 {
			return errors.New("слишком много хостов (максимум 32)")
		}
		h.cfg.Hosts = append(h.cfg.Hosts, hc)
	}
	return h.saveCfgLocked()
}

func (h *sysadminHub) DeleteHost(name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	kept := make([]sysadminHostCfg, 0, len(h.cfg.Hosts))
	for _, hc := range h.cfg.Hosts {
		if hc.Name != name {
			kept = append(kept, hc)
		}
	}
	h.cfg.Hosts = kept
	if _, had := h.secrets.Hosts[name]; had {
		delete(h.secrets.Hosts, name)
		if err := h.saveSecretsLocked(); err != nil {
			return err
		}
	}
	return h.saveCfgLocked()
}

// SetHostPassword stores (or with an empty value clears) the host password.
func (h *sysadminHub) SetHostPassword(name, pw string) error {
	if strings.ContainsAny(pw, "\r\n\x00") || len(pw) > 256 {
		return errors.New("пароль содержит недопустимые символы или слишком длинный")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	known := false
	for _, hc := range h.cfg.Hosts {
		if hc.Name == name {
			known = true
		}
	}
	if !known {
		return errors.New("хост не найден")
	}
	if pw == "" {
		delete(h.secrets.Hosts, name)
	} else {
		h.secrets.Hosts[name] = pw
	}
	return h.saveSecretsLocked()
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (h *sysadminHub) saveCfgLocked() error {
	b, err := json.MarshalIndent(h.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(h.cfgPath(), b, 0o600); err != nil {
		return err
	}
	h.cfgStamp, _ = stampOf(h.cfgPath())
	return nil
}

func (h *sysadminHub) saveSecretsLocked() error {
	b, err := json.MarshalIndent(h.secrets, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(h.secPath(), b, 0o600); err != nil {
		return err
	}
	h.secStamp, _ = stampOf(h.secPath())
	return nil
}

func (h *sysadminHub) Policy() sysadmin.Policy {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := sysadmin.Policy{Default: sysadmin.ModeAsk}
	if h.loadLocked() != nil {
		return p
	}
	if h.cfg.Policy.Default != "" {
		p.Default = sysadmin.Mode(h.cfg.Policy.Default)
	}
	for _, r := range h.cfg.Policy.Rules {
		p.Rules = append(p.Rules, sysadmin.Rule{Tool: r.Tool, Match: r.Match, Mode: sysadmin.Mode(r.Mode)})
	}
	if p.Validate() != nil {
		// A broken stored policy falls back to the strictest sane default.
		return sysadmin.Policy{Default: sysadmin.ModeAsk}
	}
	return p
}

func (h *sysadminHub) SetPolicy(p sysadmin.Policy) error {
	if p.Default == "" {
		p.Default = sysadmin.ModeAsk
	}
	if err := p.Validate(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	h.cfg.Policy = sysadminPolicyCfg{Default: string(p.Default)}
	for _, r := range p.Rules {
		h.cfg.Policy.Rules = append(h.cfg.Policy.Rules, sysadminRuleCfg{Tool: r.Tool, Match: r.Match, Mode: string(r.Mode)})
	}
	return h.saveCfgLocked()
}

func (h *sysadminHub) Memory() *sysadmin.Memory { return h.mem }
func (h *sysadminHub) Queue() *sysadmin.Queue   { return h.queue }
func (h *sysadminHub) Exec() *sysadmin.Executor { return h.exec }

// Audit forwards an event to the configured auditor (nil-safe).
func (h *sysadminHub) Audit(event string, fields map[string]any) {
	h.mu.Lock()
	a := h.auditor
	h.mu.Unlock()
	if a != nil {
		a.Audit(event, fields)
	}
}

// Telegram returns a model-safe view of the Telegram settings (never the token).
func (h *sysadminHub) Telegram() telegramView {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loadLocked() != nil {
		return telegramView{}
	}
	v := telegramView{
		Enabled:  h.cfg.Telegram.Enabled,
		Users:    h.cfg.Telegram.Users,
		Agent:    h.cfg.Telegram.Agent,
		HasToken: h.secrets.TelegramToken != "",
	}
	if v.Users == nil {
		v.Users = []int64{}
	}
	return v
}

func (h *sysadminHub) TelegramToken() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.loadLocked()
	return h.secrets.TelegramToken
}

func (h *sysadminHub) OnTelegramChange(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onTelegramChange = fn
}

func (h *sysadminHub) SetTelegram(enabled bool, users []int64, agent, token string, clearToken bool) error {
	if len(users) > 10 {
		return errors.New("слишком много пользователей (максимум 10)")
	}
	for _, u := range users {
		if u <= 0 {
			return errors.New("user_id должен быть положительным")
		}
	}
	if agent != "" && !bridge.ValidAgentName(agent) {
		return errors.New("некорректное имя роли")
	}
	if token != "" {
		if !strings.Contains(token, ":") {
			return errors.New("токен имеет неверный формат")
		}
	}
	h.mu.Lock()
	if err := h.loadLocked(); err != nil {
		h.mu.Unlock()
		return err
	}
	h.cfg.Telegram.Enabled = enabled
	h.cfg.Telegram.Users = users
	h.cfg.Telegram.Agent = agent
	if clearToken {
		h.secrets.TelegramToken = ""
	}
	if token != "" {
		h.secrets.TelegramToken = token
	}
	if err := h.saveCfgLocked(); err != nil {
		h.mu.Unlock()
		return err
	}
	if token != "" || clearToken {
		if err := h.saveSecretsLocked(); err != nil {
			h.mu.Unlock()
			return err
		}
	}
	cb := h.onTelegramChange
	h.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

// SetAuditor installs the auditor once the panel (which owns the audit log) exists.
func (h *sysadminHub) SetAuditor(a sysadmin.Auditor) {
	h.mu.Lock()
	h.auditor = a
	h.mu.Unlock()
	h.queue.SetAuditor(a)
}

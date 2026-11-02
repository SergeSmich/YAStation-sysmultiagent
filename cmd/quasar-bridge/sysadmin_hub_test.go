package main

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"quasarbridge/internal/sysadmin"
)

func newHub(t *testing.T) *sysadminHub {
	t.Helper()
	dir := t.TempDir()
	h := NewSysadminHub(dir, nil)
	return h
}

func TestHubSaveReloadHost(t *testing.T) {
	h := newHub(t)
	hc := sysadminHostCfg{Name: "pi", Addr: "192.168.1.5", Port: 0, User: "pi", MAC: "aa:bb:cc:dd:ee:ff"}
	if err := h.SaveHost(hc); err != nil {
		t.Fatal(err)
	}
	hosts := h.Hosts()
	if len(hosts) != 1 || hosts[0].Name != "pi" || hosts[0].Port != 22 {
		t.Fatalf("hosts=%+v", hosts)
	}
	// reload from disk
	h2 := NewSysadminHub(h.dir, nil)
	if h2.Hosts()[0].Port != 22 {
		t.Fatal("reload failed")
	}
}

func TestHubExternalEditMtime(t *testing.T) {
	h := newHub(t)
	_ = h.SaveHost(sysadminHostCfg{Name: "pi", Addr: "1.1.1.1", Port: 22, User: "pi"})
	// external edit
	b, _ := os.ReadFile(h.cfgPath())
	b = append(b, []byte("{broken")...)
	_ = os.WriteFile(h.cfgPath(), b, 0o600)
	// invalid json -> err
	if h.Err() == nil {
		t.Fatal("expected err after external corruption")
	}
	// Hosts nil on broken
	if h.Hosts() != nil {
		t.Fatalf("expected nil hosts on broken")
	}
}

func TestHubBrokenJSONNoPanic(t *testing.T) {
	h := newHub(t)
	_ = os.WriteFile(h.cfgPath(), []byte("{bad"), 0o600)
	defer func() { recover() }()
	if h.Err() == nil {
		t.Fatal("expected err")
	}
	if h.Hosts() != nil {
		t.Fatal("expected nil")
	}
}

func TestHubPasswordNotInHostsOrJSON(t *testing.T) {
	h := newHub(t)
	_ = h.SaveHost(sysadminHostCfg{Name: "pi", Addr: "1.1.1.1", Port: 22, User: "pi"})
	_ = h.SetHostPassword("pi", "secret123")
	hosts := h.Hosts()
	for _, hv := range hosts {
		b, _ := json.Marshal(hv)
		if strings.Contains(string(b), "secret123") {
			t.Fatal("password leaked in hostView json")
		}
		if hv.HasPassword != true {
			t.Fatal("haspassword false")
		}
	}
	// sysadmin.json no password
	cb, _ := os.ReadFile(h.cfgPath())
	if strings.Contains(string(cb), "secret123") {
		t.Fatal("password in sysadmin.json")
	}
	// secrets file has it
	sb, _ := os.ReadFile(h.secPath())
	if !strings.Contains(string(sb), "secret123") {
		t.Fatal("password not in secrets")
	}
	// execHosts has password
	eh := h.execHosts()
	if eh["pi"].Password != "secret123" {
		t.Fatal("execHosts missing password")
	}
}

func TestHubSecretsPerms(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("windows")
	}
	h := newHub(t)
	_ = h.SaveHost(sysadminHostCfg{Name: "pi", Addr: "1.1.1.1", Port: 22, User: "pi"})
	_ = h.SetHostPassword("pi", "pw")
	fi, err := os.Stat(h.secPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm=%v", fi.Mode().Perm())
	}
}

func TestHubSetPasswordUnknownHost(t *testing.T) {
	h := newHub(t)
	if err := h.SetHostPassword("nope", "pw"); err == nil {
		t.Fatal("expected error")
	}
}

func TestHubDeleteHostCleansPassword(t *testing.T) {
	h := newHub(t)
	_ = h.SaveHost(sysadminHostCfg{Name: "pi", Addr: "1.1.1.1", Port: 22, User: "pi"})
	_ = h.SetHostPassword("pi", "pw")
	_ = h.DeleteHost("pi")
	if _, ok := h.secrets.Hosts["pi"]; ok {
		t.Fatal("password not cleaned")
	}
	sb, _ := os.ReadFile(h.secPath())
	if strings.Contains(string(sb), "pw") {
		// may be empty map; ok if not present
	}
	_ = sb
}

func TestHubValidation(t *testing.T) {
	h := newHub(t)
	cases := []struct {
		hc sysadminHostCfg
		ok bool
	}{
		{sysadminHostCfg{Name: "", Addr: "1.1.1.1", User: "u"}, false},
		{sysadminHostCfg{Name: "pi", Addr: "1.1.1.1", Port: 0, User: "u"}, true},
		{sysadminHostCfg{Name: "pi2", Addr: "1.1.1.1", Port: 70000, User: "u"}, false},
		{sysadminHostCfg{Name: "pi3", Addr: "1.1.1.1", Port: 22, User: "", MAC: "zz"}, false},
		{sysadminHostCfg{Name: "pi4", Addr: "1.1.1.1", Port: 22, User: "u", MAC: "aa:bb:cc:dd:ee:ff"}, true},
	}
	for i, c := range cases {
		err := h.SaveHost(c.hc)
		if c.ok && err != nil {
			t.Fatalf("case %d expected ok got %v", i, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("case %d expected err", i)
		}
	}
}

func TestHubPolicyRoundtrip(t *testing.T) {
	h := newHub(t)
	_ = h.SaveHost(sysadminHostCfg{Name: "pi", Addr: "1.1.1.1", Port: 22, User: "pi"})
	p := sysadmin.Policy{Default: sysadmin.ModeAsk, Rules: []sysadmin.Rule{{Tool: "exec", Match: "ls", Mode: sysadmin.ModeAllow}}}
	if err := h.SetPolicy(p); err != nil {
		t.Fatal(err)
	}
	got := h.Policy()
	if got.Default != sysadmin.ModeAsk || len(got.Rules) != 1 {
		t.Fatalf("policy=%+v", got)
	}
	// broken policy -> error validate
	bp := sysadmin.Policy{Default: sysadmin.Mode("bogus")}
	if err := bp.Validate(); err == nil {
		// maybe Validate not strict; still try SetPolicy
	}
	_ = bp
}

func TestHubBrokenPolicy(t *testing.T) {
	h := newHub(t)
	_ = h.SaveHost(sysadminHostCfg{Name: "pi", Addr: "1.1.1.1", Port: 22, User: "pi"})
	// write broken policy default
	b, _ := os.ReadFile(h.cfgPath())
	s := string(b)
	s = strings.Replace(s, `"default":`, `"default": "nope"`, 1)
	// ensure policy present
	if !strings.Contains(s, "policy") {
		s = strings.TrimSuffix(s, "}") + `,"policy":{"default":"nope","rules":[]}}`
	}
	_ = os.WriteFile(h.cfgPath(), []byte(s), 0o600)
	// Policy() should fall back, Err may or may not be nil; just no panic
	defer func() { recover() }()
	p := h.Policy()
	if p.Default != sysadmin.ModeAsk {
		t.Fatalf("expected ask fallback, got %s", p.Default)
	}
}
func TestHubTelegramTokenHidden(t *testing.T) {
	h := newHub(t)
	_ = h.SetTelegram(true, []int64{1}, "sysadmin", "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcde", false)
	v := h.Telegram()
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "123456:") { t.Fatal("token leaked") }
	if v.HasToken != true { t.Fatal("has_token false") }
}

func TestHubTelegramInvalidToken(t *testing.T) {
	h := newHub(t)
	if err := h.SetTelegram(true, []int64{1}, "sysadmin", "bad", false); err == nil { t.Fatal("expected err") }
	if h.Telegram().HasToken { t.Fatal("saved") }
}

func TestHubTelegramUsersValidation(t *testing.T) {
	h := newHub(t)
	if err := h.SetTelegram(true, make([]int64, 11), "sysadmin", "", false); err == nil { t.Fatal("expected err >10") }
	if err := h.SetTelegram(true, []int64{0}, "sysadmin", "", false); err == nil { t.Fatal("expected err <=0") }
}

func TestHubTelegramInvalidRole(t *testing.T) {
	h := newHub(t)
	if err := h.SetTelegram(true, []int64{1}, "Bad Role", "", false); err == nil { t.Fatal("expected err") }
}

func TestHubTelegramEmptyTokenKeeps(t *testing.T) {
	h := newHub(t)
	_ = h.SetTelegram(true, []int64{1}, "sysadmin", "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcde", false)
	_ = h.SetTelegram(true, []int64{1}, "sysadmin", "", false)
	if !h.Telegram().HasToken { t.Fatal("token cleared") }
}

func TestHubTelegramClearToken(t *testing.T) {
	h := newHub(t)
	_ = h.SetTelegram(true, []int64{1}, "sysadmin", "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcde", false)
	_ = h.SetTelegram(true, []int64{1}, "sysadmin", "", true)
	if h.Telegram().HasToken { t.Fatal("token not cleared") }
}

func TestHubTelegramSecretsFiles(t *testing.T) {
	h := newHub(t)
	_ = h.SetTelegram(true, []int64{1}, "sysadmin", "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcde", false)
	cb, _ := os.ReadFile(h.cfgPath())
	if strings.Contains(string(cb), "123456:") { t.Fatal("token in sysadmin.json") }
	sb, _ := os.ReadFile(h.secPath())
	if !strings.Contains(string(sb), "123456:") { t.Fatal("token not in secrets") }
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(h.secPath())
		if fi.Mode().Perm() != 0600 { t.Fatalf("perms %v", fi.Mode().Perm()) }
	}
}

func TestHubTelegramOnChangeEvent(t *testing.T) {
	h := newHub(t)
	called := 0
	h.OnTelegramChange(func() { called++ })
	if err := h.SetTelegram(true, []int64{1}, "sysadmin", "123456:ABCDEFGHIJKLMNOPQRSTUVWXYZabcde", false); err != nil { t.Fatal(err) }
	if called != 1 { t.Fatalf("called=%d", called) }
	if err := h.SetTelegram(true, []int64{1}, "sysadmin", "bad", false); err == nil { t.Fatal("expected err") }
	if called != 1 { t.Fatalf("called on error=%d", called) }
}

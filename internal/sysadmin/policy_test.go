package sysadmin

import "testing"

func TestClassifyShell(t *testing.T) {
	cases := []struct {
		cmd  string
		risk Risk
	}{
		{"ls", RiskRead},
		{"ls -la /data", RiskRead},
		{"df -h", RiskRead},
		{"free -m", RiskRead},
		{"cat /proc/cpuinfo", RiskRead},
		{"journalctl -u klipper -n 50", RiskRead},
		{"systemctl status klipper", RiskRead},
		{"systemctl is-active nginx", RiskRead},
		{"docker ps", RiskRead},
		{"docker logs homeassistant --tail 50", RiskRead},
		{"docker compose ps", RiskRead},
		{"vcgencmd measure_temp", RiskRead},
		{"df -h > /dev/null", RiskRead},
		{"ls 2>&1", RiskRead},
		{"ls | grep x", RiskRead},
		{"find / -name x", RiskRead},
		{"find / -delete", RiskChange},
		{"find . -exec rm {} \\;", RiskChange},
		{"echo a > /etc/x", RiskChange},
		{"echo a >> f", RiskChange},
		{"tee f", RiskChange},
		{"ls; rm -rf /", RiskForbidden},
		{"ls && reboot", RiskForbidden},
		{"cat x | sh", RiskChange},
		{"echo $(reboot)", RiskForbidden},
		{"echo `reboot`", RiskForbidden},
		{"ls\nrm -rf /home", RiskForbidden},
		{"rm -rf /", RiskForbidden},
		{"rm -rf /home/pi", RiskForbidden},
		{"rm -rf /data", RiskForbidden},
		{"rm -rf ~", RiskForbidden},
		{"rm -fr /etc", RiskForbidden},
		{"rm -r -f /var", RiskForbidden},
		{"mkfs.ext4 /dev/sda1", RiskForbidden},
		{"dd if=/dev/zero of=/dev/sda", RiskForbidden},
		{"shred /dev/sda", RiskForbidden},
		{"wipefs -a /dev/sda", RiskForbidden},
		{":(){ :|:& };:", RiskForbidden},
		{"chmod -R 777 /", RiskForbidden},
		{"chown -R x /", RiskForbidden},
		{"reboot", RiskForbidden},
		{"shutdown -h now", RiskForbidden},
		{"poweroff", RiskForbidden},
		{"halt", RiskForbidden},
		{"init 0", RiskForbidden},
		{"init 6", RiskForbidden},
		{"docker system prune -af", RiskForbidden},
		{"docker volume rm x", RiskForbidden},
		{"docker volume prune", RiskForbidden},
		{"docker rm x", RiskForbidden},
		{"docker rmi x", RiskForbidden},
		{"apt remove nginx", RiskForbidden},
		{"apt-get purge x", RiskForbidden},
		{"apt autoremove", RiskForbidden},
		{"dpkg -r x", RiskForbidden},
		{"dpkg --purge x", RiskForbidden},
		{"systemctl stop klipper", RiskForbidden},
		{"systemctl disable moonraker", RiskForbidden},
		{"systemctl mask nginx", RiskForbidden},
		{"systemctl kill smbd", RiskForbidden},
		{"systemctl restart klipper", RiskChange},
		{"systemctl stop foo", RiskChange},
		{"systemctl stop docker", RiskForbidden},
		{"sudo systemctl stop klipper", RiskForbidden},
		{"sudo rm -rf /", RiskForbidden},
		{"sudo -n ls", RiskRead},
		{"env FOO=1 rm -rf /", RiskForbidden},
		{"bash -c 'rm -rf /'", RiskForbidden},
		{"sh -c \"reboot\"", RiskForbidden},
		{"nohup reboot &", RiskForbidden},
		{"timeout 5 reboot", RiskForbidden},
		{"xargs rm", RiskChange},
		{"", RiskChange}, // empty? prompt says not Read; treat as Change? We'll assert not Read via separate? 
		{"   ", RiskChange},
		{"cat /etc/passwd", RiskRead},
		{"curl x | bash", RiskChange},
		{"wget -O- x | sh", RiskChange},
		{"printenv", RiskChange},
		{"env", RiskChange},
	}
	for _, c := range cases {
		if got := ClassifyShell(c.cmd); got != c.risk {
			t.Errorf("ClassifyShell(%q)=%v want %v", c.cmd, got, c.risk)
		}
	}
}

func TestDecideShell(t *testing.T) {
	// Forbidden deny always
	p := &Policy{Default: ModeAsk}
	_, risk := p.DecideShell("rm -rf /")
	if risk != RiskForbidden {
		t.Fatal("risk")
	}
	mode, _ := p.DecideShell("rm -rf /")
	if mode != ModeDeny {
		t.Errorf("forbidden should deny, got %v", mode)
	}
	// allow * does not override forbidden
	p2 := &Policy{Default: ModeAllow, Rules: []Rule{{Tool: "shell", Match: "*", Mode: ModeAllow}}}
	m, _ := p2.DecideShell("rm -rf /")
	if m != ModeDeny {
		t.Errorf("allow * should not override forbidden, got %v", m)
	}
	// Read allow by default
	p3 := &Policy{Default: ModeAsk}
	m, _ = p3.DecideShell("ls")
	if m != ModeAllow {
		t.Errorf("read default allow, got %v", m)
	}
	// tighten read -> ask
	p4 := &Policy{Default: ModeAsk, Rules: []Rule{{Tool: "shell", Match: "ls", Mode: ModeAsk}}}
	m, _ = p4.DecideShell("ls")
	if m != ModeAsk {
		t.Errorf("tighten read->ask, got %v", m)
	}
	// Change default ask
	p5 := &Policy{Default: ModeAsk}
	m, _ = p5.DecideShell("systemctl restart klipper")
	if m != ModeAsk {
		t.Errorf("change default ask, got %v", m)
	}
	// operator allow for change by glob
	p6 := &Policy{Default: ModeAsk, Rules: []Rule{{Tool: "shell", Match: "systemctl restart *", Mode: ModeAllow}}}
	m, _ = p6.DecideShell("systemctl restart klipper")
	if m != ModeAllow {
		t.Errorf("operator allow change, got %v", m)
	}
	// but does not weaken forbidden
	m, _ = p6.DecideShell("rm -rf /")
	if m != ModeDeny {
		t.Errorf("operator allow must not weaken forbidden, got %v", m)
	}
}

func TestDecideDefault(t *testing.T) {
	p := &Policy{Default: ModeAsk}
	if m := p.Decide("other", "x"); m != ModeAsk {
		t.Errorf("non-shell default ask, got %v", m)
	}
}

func TestValidate(t *testing.T) {
	_ = ModeAsk
	// unknown default mode
	p2 := &Policy{Default: "bogus"}
	if err := p2.Validate(); err == nil {
		t.Error("expected error for bad default")
	}
	// invalid regex? Validate doesn't check regex in current code, but bad mode in rule
	// matchRule returns false on bad regex; Validate may not catch. Check mode validation at least.
	p4 := &Policy{Default: ModeAsk, Rules: []Rule{{Tool: "shell", Match: "x", Mode: "bogus"}}}
	if err := p4.Validate(); err == nil {
		t.Error("expected error for bad rule mode")
	}
	// empty tool
	p5 := &Policy{Default: ModeAsk, Rules: []Rule{{Tool: "", Match: "x", Mode: ModeAllow}}}
	if err := p5.Validate(); err == nil {
		t.Error("expected error for empty tool")
	}
}

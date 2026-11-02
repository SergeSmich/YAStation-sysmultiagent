package sysadmin

import "testing"

// Every line here used to slip through as a plain read, or must stay one.
func TestClassifyShellBypassAttempts(t *testing.T) {
	notRead := []string{
		"ls & rm /tmp/x",
		"ls &rm /tmp/x",
		"ls\nrm /tmp/x",
		"ls\rrm /tmp/x",
		"ls; rm /tmp/x",
		"ls && rm /tmp/x",
		"ls || rm /tmp/x",
		"cat <(rm /tmp/x)",
		"ls $(rm /tmp/x)",
		"ls `rm /tmp/x`",
		"(rm /tmp/x)",
		"{ rm /tmp/x; }",
		"ls > /tmp/x",
		"cat /etc/hosts >> /tmp/x",
		"ip link set eth0 down",
		"ip addr add 10.0.0.1/24 dev eth0",
		"ip route del default",
		"ip link set dev eth0 up",
		"mount /dev/sda1 /mnt",
		"date -s 2020-01-01",
		"hostname newname",
		"sort -o /tmp/x /tmp/y",
		"sort --output=/tmp/x /tmp/y",
		"sort -ro /tmp/x /tmp/y",
		"uniq in out",
		"find / -delete",
		"find / -exec rm {} ;",
		"find / -fprint /tmp/x",
		"journalctl --vacuum-size=1M",
		"journalctl --rotate",
		"journalctl -f",
		"tail -f /var/log/syslog",
		"dmesg -c",
		"dmesg -C",
		"systemctl restart nginx",
		"systemctl stop foo",
		"docker restart x",
		"docker rm x",
		"docker run alpine",
		"docker compose up -d",
		"docker logs -f x",
		"docker stats",
		"ss -K",
		"ss --kill",
		"lsof",
		"ssh host id",
		"sshpass -p x ssh h",
		"psql -c 'drop table x'",
		"idle",
		"top",
		"cat /etc/shadow",
		"cat /home/pi/.ssh/id_rsa",
		"grep password /etc/foo",
		"cat ~/x",
		"cat /etc/*",
		"echo hi | tee /tmp/x",
		"ls | xargs rm",
		"ls |",
		"| ls",
		"ls || ls",
		"sudo -u root ls",
		"sudo",
		"",
		"   ",
		"l\"s\" -delete",
		"vcgencmd otp_dump",
		"vcgencmd",
		"ip",
		"ip addr add",
		"ls >/dev/null.bak",
		"ls > /dev/nullx",
		"ls 2>&1x",
		"ls 2>&1|rm x",
		"ls >/dev/null; rm x",
		"ls >/dev/null && rm x",
		"sudo -n -u root ls",
	}
	for _, c := range notRead {
		if got := ClassifyShell(c); got == RiskRead {
			t.Errorf("ClassifyShell(%q) = Read, must not be auto-allowed", c)
		}
	}

	read := []string{
		"ls",
		"ls -la /var/log",
		"cat /proc/cpuinfo",
		"df -h",
		"free -m",
		"uptime",
		"ps aux",
		"ps aux | grep klipper",
		"ip addr",
		"ip -s link show",
		"ip route show",
		"systemctl status klipper",
		"systemctl is-active moonraker",
		"journalctl -u klipper -n 50 --no-pager",
		"docker ps",
		"docker ps -a",
		"docker logs --tail 20 homeassistant",
		"docker stats --no-stream",
		"docker compose ps",
		"vcgencmd measure_temp",
		"date",
		"date +%Y",
		"find /etc -name foo",
		"sort -n",
		"tail -n 20 /var/log/syslog",
		"sudo ls /root",
		"dmesg -T",
		"mount",
		"top -bn1",
		"hostname -I",
		"ss -tlnp",
	}
	for _, c := range read {
		if got := ClassifyShell(c); got != RiskRead {
			t.Errorf("ClassifyShell(%q) = %v, want Read", c, got)
		}
	}
}

func TestClassifyShellForbiddenSurvivesNesting(t *testing.T) {
	forbidden := []string{
		"ls & rm -rf /",
		"echo x; reboot",
		"(shutdown now)",
		"ls $(rm -rf /etc)",
		"true && systemctl stop klipper",
		"sudo -n rm -rf /var",
		"docker system prune -af",
	}
	for _, c := range forbidden {
		if got := ClassifyShell(c); got != RiskForbidden {
			t.Errorf("ClassifyShell(%q) = %v, want Forbidden", c, got)
		}
	}
}

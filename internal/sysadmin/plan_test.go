package sysadmin

import (
	"strings"
	"testing"
)

// IsDestructive: что не покрывается одобренным планом задачи.

func TestIsDestructive(t *testing.T) {
	destructive := []string{
		"rm -rf /tmp/x",
		"rm /tmp/x",
		"RM -f /tmp/x",
		"Remove-Item -Recurse -Force C:\\Temp\\a",
		"remove-item C:\\Temp\\a",
		"del C:\\Temp\\x.txt",
		"erase /q x.txt",
		"rmdir /s /q C:\\Temp\\old",
		"format C:",
		"Format-Volume -DriveLetter D",
		"uninstall-tool x",
		"Stop-Process -Name notepad",
		"stop-service klipper",
		"Restart-Service foo",
		"taskkill /IM notepad.exe /F",
		"kill -9 1234",
		"killall nginx",
		"pkill sshd",
		"shutdown /s /t 0",
		"reboot",
		"poweroff",
		"halt",
		"Set-ExecutionPolicy Bypass",
		"icacls C:\\Temp /grant Everyone:F",
		"takeown /f C:\\Temp",
		"netsh advfirewall set allprofiles state off",
		"net user test /add",
		"reg add HKCU\\Software\\X",
		"schtasks /create /tn x",
		"sc stop foo",
		"Clear-Content C:\\Temp\\x.txt",
		"Move-Item a b",
		"mv a b",
		"Rename-Item a b",
		"chmod 777 /tmp/x",
		"chown root /tmp/x",
		"sudo systemctl restart foo",
		"apt-get install x",
		"npm install x",
		"systemctl restart foo",
		"service nginx reload",
		"crontab -e",
		"Invoke-WebRequest http://x",
		"iex (New-Object Net.WebClient)",
		"curl http://x",
		"wget http://x",
		"dd if=/dev/zero of=/tmp/x",
		"mkfs.ext4 /dev/sda1",
		"bcdedit /set x",
		"diskpart",
		// Любой systemctl считается небезопасным, даже читающий.
		"systemctl status klipper",
	}
	for _, cmd := range destructive {
		if !IsDestructive(cmd) {
			t.Errorf("IsDestructive(%q)=false, ожидалось true", cmd)
		}
	}

	harmless := []string{
		"",
		"touch /tmp/x",
		"New-Item -Path C:\\Temp\\x.txt -ItemType File",
		"Set-Content -Path C:\\Temp\\note.txt -Value hello",
		"Add-Content -Path /tmp/x -Value y",
		"Get-Content C:\\Temp\\note.txt",
		"echo hello > /tmp/x",
		"mkdir /tmp/x",
		"cp a b",
		"Copy-Item a.txt b.txt",
		"ls -la /tmp",
		"firmware update",
		"charm",
		"git status",
		"df -h",
	}
	for _, cmd := range harmless {
		if IsDestructive(cmd) {
			t.Errorf("IsDestructive(%q)=true, ожидалось false", cmd)
		}
	}
}

func TestIsDestructiveBackslashesDoNotHide(t *testing.T) {
	// Обратные слэши заменяются пробелами, поэтому «\\rm» не спрячет слово.
	if !IsDestructive(`\\rm -f x`) {
		t.Fatal("обратный слэш перед rm не должен скрывать команду")
	}
	if !IsDestructive(`C:\Windows\System32\taskkill.exe /F /IM x`) {
		t.Fatal("taskkill в пути не найден")
	}
}

func TestPlanSafeAllowed(t *testing.T) {
	for _, cmd := range []string{
		`New-Item -Path C:\Temp\x.txt -ItemType File`,
		`Set-Content -Path C:\Temp\note.txt -Value hello`,
		`Get-Content C:\Temp\note.txt`,
		`touch /tmp/x`,
		`mkdir /tmp/x`,
		`  MKDIR /tmp/x  `,
		`mkdir C:\Temp\a; New-Item C:\Temp\a\b.txt; Set-Content C:\Temp\a\b.txt hi`,
		`Get-Content a.txt | Set-Content b.txt`,
		"mkdir a\ntouch a/b",
		`mkdir a && touch a/b`,
		`mkdir a || touch b`,
		`Get-ChildItem C:\Temp | Sort-Object | Select-Object`,
		`mkdir a;`,
	} {
		if !PlanSafe(cmd) {
			t.Errorf("PlanSafe(%q)=false, ожидалось true", cmd)
		}
	}
}

func TestPlanSafeRejected(t *testing.T) {
	cases := []string{
		// запрещённые команды
		`Start-Process calc.exe`,
		`Invoke-Expression "calc"`,
		`iex "calc"`,
		`Remove-Item C:\Temp\x`,
		// вызовы через &
		`& calc.exe`,
		`touch a & calc`,
		`& "C:\Windows\System32\calc.exe"`,
		// подвыражения, блоки, скобки, обратная кавычка
		`mkdir $(calc)`,
		`mkdir ${env:TEMP}`,
		`Set-Content a.txt (Get-Date)`,
		`Get-ChildItem | ForEach-Object { calc }`,
		`Set-Content a.txt {x}`,
		"mkdir a`;calc",
		// -enc и find -delete и прочие опасные ключи
		`powershell -enc AAAA`,
		`mkdir a -ENC AAAA`,
		`find /tmp -delete`,
		`find . -name x -exec rm {} ;`,
		`find . -ok ls`,
		`find . -fprint /etc/x`,
		// не из списка
		`calc`,
		`curl http://x`,
		`mkdir a; calc`,
		`touch a | calc`,
		`touch a && calc`,
		`touch a || calc`,
		"touch a\ncalc",
		// пустое
		``,
		`   `,
		"\n\t",
		`;`,
		`;;|`,
		// длина
		`touch ` + strings.Repeat("a", 2000),
		strings.Repeat("a", 2001),
	}
	for _, cmd := range cases {
		if PlanSafe(cmd) {
			t.Errorf("PlanSafe(%q)=true, ожидалось false", cmd)
		}
	}
}

func TestPlanSafeLengthBoundary(t *testing.T) {
	ok := "touch " + strings.Repeat("a", 2000-len("touch "))
	if len(ok) != 2000 {
		t.Fatalf("len=%d", len(ok))
	}
	if !PlanSafe(ok) {
		t.Error("команда ровно в 2000 символов должна проходить")
	}
	if PlanSafe(ok + "a") {
		t.Error("команда в 2001 символ должна отклоняться")
	}
}

func TestPolicyHasRule(t *testing.T) {
	p := Policy{Default: ModeAsk, Rules: []Rule{
		{Tool: "shell", Match: "systemctl restart klipper", Mode: ModeAllow},
		{Tool: "shell", Match: "ls *", Mode: ModeAllow},
		{Tool: "shell", Match: `re:^docker `, Mode: ModeDeny},
		{Tool: "shell", Match: `re:([`, Mode: ModeAllow}, // битая регулярка не должна паниковать
		{Tool: "read_file", Mode: ModeAllow},
		{Tool: "wol", Match: "pc", Mode: ModeAsk},
	}}
	cases := []struct {
		tool, subject string
		want          bool
	}{
		{"shell", "systemctl restart klipper", true},
		{"shell", "systemctl restart moonraker", false},
		{"shell", "ls -la", true},
		{"shell", "ls -la /tmp", false}, // path.Match: «*» не переходит через «/»
		{"shell", "lsblk", false},
		{"shell", "docker ps", true},
		{"shell", "DOCKER ps", false}, // регулярка чувствительна к регистру
		{"shell", "/etc/hostname", false},
		{"read_file", "/любой/путь", true},
		{"wol", "pc", true},
		{"wol", "pi", false},
		{"http_get", "http://x/", false},
	}
	for _, c := range cases {
		if got := p.HasRule(c.tool, c.subject); got != c.want {
			t.Errorf("HasRule(%q,%q)=%v, ожидалось %v", c.tool, c.subject, got, c.want)
		}
	}

	// Пустая политика: ни одного правила.
	empty := Policy{Default: ModeAsk}
	if empty.HasRule("shell", "touch /tmp/x") {
		t.Fatal("в пустой политике не должно быть правил")
	}
	// Правило с Tool="*" действует на любой инструмент.
	star := Policy{Default: ModeAsk, Rules: []Rule{{Tool: "*", Match: "/tmp/*", Mode: ModeAllow}}}
	if !star.HasRule("shell", "/tmp/x") || !star.HasRule("read_file", "/tmp/x") {
		t.Fatal("правило * не применилось ко всем инструментам")
	}
	if star.HasRule("shell", "/etc/x") {
		t.Fatal("правило * применилось к чужому subject")
	}
}

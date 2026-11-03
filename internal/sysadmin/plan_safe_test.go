package sysadmin

import (
	"strings"
	"testing"
)

func TestPlanSafe(t *testing.T) {
	ok := []string{
		`New-Item -Path C:\Users\Admin\Desktop\a.txt -ItemType File`,
		`Set-Content -Path C:\x\a.txt -Value hello`,
		`Get-Content C:\x\a.txt`,
		`touch /tmp/a.txt`,
		`mkdir /tmp/qb`,
		`New-Item a.txt; Set-Content a.txt hi`,
		`Get-ChildItem C:\x | Select-Object -First 3`,
		`mkdir x && touch x/a`,
		`New-Item -Path $env:USERPROFILE\Desktop\заметка.txt -ItemType File`,
	}
	for _, c := range ok {
		if !PlanSafe(c) {
			t.Errorf("PlanSafe(%q) = false, ожидалось true", c)
		}
	}
	bad := []string{
		``, `   `,
		`Start-Process calc`,
		`Invoke-Expression 'dir'`,
		`iex 'dir'`,
		`& calc`,
		`touch a & calc`,
		`echo $(whoami)`,
		`Get-Content a | ForEach-Object { $_ }`,
		`New-Item (calc)`,
		`powershell -enc AAAA`,
		`find / -delete`,
		`find . -exec rm {} \;`,
		`Set-Content a hi; calc`,
		`unknowncmd arg`,
		"echo `whoami`",
		strings.Repeat("touch a;", 400),
		`Get-Content $env:USERPROFILE\.s?h\id_r?a`,
		`cat ~/.s*/id_*`,
		`Get-Content $HOME\a.txt`,
		`Add-Content $PROFILE 'x'`,
		`Set-Content C:\Users\Admin\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup\a.txt hi`,
		`Set-Content a.ps1 hi`,
		`Add-Content /home/pi/.bashrc x`,
		`Get-Content \\host\share\a.txt`,
		`Copy-Item //host/share/a.txt .`,
	}
	for _, c := range bad {
		if PlanSafe(c) {
			t.Errorf("PlanSafe(%q) = true, ожидалось false", c)
		}
	}
}

func TestParseLaunchHardening(t *testing.T) {
	for _, in := range []string{"C:\\a’,(calc),’b.txt", "C:\\a“b.txt", "C:\\a(1).txt", "C:\\a,b.txt", "payload.bat", "x.ps1", "a.cmd", "app.com", "cmd .exe", "powershell -enc x", `C:\Temp\Start Menu\Programs\chrome.lnk`} {
		if _, err := ParseLaunch(in); err == nil {
			t.Errorf("ParseLaunch(%q) должен отклоняться", in)
		}
	}
	for _, in := range []string{"C:\\Users\\Admin\\Desktop\\заметка про печать.txt", "chrome.exe", "блокнот"} {
		if _, err := ParseLaunch(in); err != nil {
			t.Errorf("ParseLaunch(%q): %v", in, err)
		}
	}
}

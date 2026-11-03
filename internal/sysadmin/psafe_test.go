package sysadmin

import "testing"

func TestIsSafeChangePS(t *testing.T) {
	safe := []string{
		`Remove-Item -Recurse -Force $env:TEMP\*`,
		`Remove-Item -Path "$env:TEMP\*" -Recurse -Force -ErrorAction SilentlyContinue`,
		`rm -recurse -force $env:LOCALAPPDATA\Temp\*`,
		`Start-Process notepad`,
		`Start-Process calc.exe`,
		`Start-Process -FilePath firefox`,
		`start chrome`,
	}
	for _, c := range safe {
		if !IsSafeChangePS(c) {
			t.Errorf("IsSafeChangePS(%q) = false", c)
		}
	}
	unsafe := []string{
		``,
		`Remove-Item C:\Users\Admin\Documents -Recurse -Force`,
		`Remove-Item -Recurse -Force $env:TEMP`,
		`Remove-Item $env:TEMP\* ; Remove-Item C:\x`,
		`Remove-Item $env:TEMP\..\Documents\*`,
		`Remove-Item -Path $env:TEMP\* -Include *.doc`,
		`Remove-Item $env:USERPROFILE\*`,
		`Start-Process powershell`,
		`Start-Process cmd.exe`,
		`Start-Process notepad -ArgumentList x`,
		`Start-Process notepad -Verb RunAs`,
		`Start-Process x.ps1`,
		`Start-Process 'C:\Program Files\..\Windows\evil.exe'`,
		`Start-Process 'C:\Users\Admin\Downloads\x.exe'`,
		`Start-Process 'C:\Program Files\Foo\unins000.exe'`,
		`Start-Process logoff`,
		`Start-Process notepad; calc`,
		`Start-Process notepad | Out-Null`,
		"Start-Process notepad`n",
		`Start-Process \\srv\share\x`,
		`Start-Process reg`,
		`Stop-Process -Name notepad`,
	}
	for _, c := range unsafe {
		if IsSafeChangePS(c) {
			t.Errorf("IsSafeChangePS(%q) = true", c)
		}
	}
	p := Policy{Default: ModeAsk}
	if m, _ := p.DecideShellOS(`Start-Process notepad`, true); m != ModeAllow {
		t.Errorf("windows S = %v", m)
	}
	if m, _ := p.DecideShellOS(`Start-Process notepad`, false); m != ModeAsk {
		t.Errorf("linux host must ask, got %v", m)
	}
	p.Rules = []Rule{{Tool: "shell", Match: `Start-Process notepad`, Mode: ModeAsk}}
	if m, _ := p.DecideShellOS(`Start-Process notepad`, true); m != ModeAsk {
		t.Errorf("operator rule must win, got %v", m)
	}
}

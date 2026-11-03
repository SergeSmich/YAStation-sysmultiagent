package sysadmin

import "testing"

func TestIsReadOnlyPS(t *testing.T) {
	read := []string{
		"hostname",
		"Get-Process",
		"Get-Process | Sort-Object CPU -Descending | Select-Object -First 5 Name,CPU",
		"Get-ChildItem C:\\Users\\Admin\\Desktop",
		"Get-ChildItem $env:TEMP | Measure-Object",
		"Get-Service | Where-Object { $_.Status -eq 'Running' } | Select-Object Name",
		"Get-Service | Where-Object Status -eq Running",
		"Get-CimInstance Win32_OperatingSystem | Select-Object Caption,Version",
		"(Get-Date).Year",
		"Get-Printer | Format-Table -AutoSize",
		"Get-Volume | Format-List",
		"Test-Path 'C:\\Program Files'",
		"ipconfig /all",
		"Get-NetIPAddress 2>$null",
		"Get-Content C:\\Windows\\System32\\drivers\\etc\\hosts -Tail 20",
		"Get-Process | Out-String",
	}
	for _, c := range read {
		if !IsReadOnlyPS(c) {
			t.Errorf("IsReadOnlyPS(%q) = false, want true", c)
		}
	}
	change := []string{
		"",
		"Remove-Item C:\\Temp\\x.txt",
		"Get-Process | Stop-Process",
		"Get-Process; Stop-Process -Name x",
		"Get-Process && calc",
		"Get-Process > out.txt",
		"Get-Process | Out-File x.txt",
		"$x = Get-Process",
		"Get-Process | ForEach-Object { $_.Kill() }",
		"(Get-Process).Kill()",
		"[System.IO.File]::Delete('x')",
		"Get-Content \\\\server\\share\\x",
		"Get-Content $HOME\\.ssh\\id_ed25519",
		"Get-ChildItem Env:",
		"Get-Content x -Wait",
		"Get-Process `;calc",
		"Get-Service | Where-Object { $_.Kill() }",
		"Get-Process $(calc)",
		"Get-Process @{a=1}",
		"Invoke-Expression 'dir'",
		"iex 'dir'",
		"Start-Process notepad",
		"ipconfig /release",
		"ping -t 10.0.0.1",
		"Get-ItemProperty 'HKLM:\\SAM'",
		"netsh wlan show profile key=clear",
		"Get-Process | Where-Object { $_.Name -eq (calc) }",
	}
	for _, c := range change {
		if IsReadOnlyPS(c) {
			t.Errorf("IsReadOnlyPS(%q) = true, want false", c)
		}
	}
}

func TestClassifyShellOSWindows(t *testing.T) {
	if got := ClassifyShellOS("Get-Process | Select-Object -First 3", true); got != RiskRead {
		t.Errorf("windows read = %v", got)
	}
	if got := ClassifyShellOS("Get-Process | Select-Object -First 3", false); got != RiskChange {
		t.Errorf("linux host must not use PowerShell rules, got %v", got)
	}
	if got := ClassifyShellOS("Stop-Computer", true); got != RiskForbidden {
		t.Errorf("Stop-Computer = %v", got)
	}
	if got := ClassifyShellOS("Remove-Item -Recurse -Force $env:TEMP\\*", true); got != RiskChange {
		t.Errorf("clearing Temp must be a plain change, got %v", got)
	}
	var p Policy
	p.Default = ModeAsk
	if m, _ := p.DecideShellOS("Get-Date", true); m != ModeAllow {
		t.Errorf("Get-Date on windows = %v", m)
	}
}

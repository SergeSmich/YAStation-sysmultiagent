package sysadmin

import "testing"

// Commands found by review that must never pass as read-only.
func TestIsReadOnlyPSAttacks(t *testing.T) {
	bad := []string{
		"echo \u201c'\u201d ; Remove-Item C:\\x -Recurse ; echo \u2018'\u2019",
		"Get-Process \u2013Id 1",
		`Get-ChildItem Env:`,
		`echo $env:GITHUB_TOKEN`,
		`echo $env:OPENAI_API_KEY`,
		`gc Microsoft.PowerShell.Core\FileSystem::\\evil\share\x`,
		`Get-FileHash \\evil\x`,
		`Get-Content C:\Users\Admin\AppData\Local\Google\Chrome\User Data\Default\Cookies`,
		`Get-Content C:\Users\Admin\pw.kdbx`,
		`Get-Content C:\Windows\*.ini`,
		`Get-ChildItem C:\ | Get-Content`,
		`ping evil.com`,
		`ping -n 99999999 192.168.1.1`,
		`Get-WinEvent -LogName Security`,
		`Get-EventLog -LogName Security`,
		`Get-WmiObject Win32_Product`,
		`ver`,
		`Get-Process | Stop-Process`,
		`Get-Process; Remove-Item x`,
		`Get-Process $(Remove-Item x)`,
	}
	for _, c := range bad {
		if IsReadOnlyPS(c) {
			t.Errorf("IsReadOnlyPS(%q) = true", c)
		}
	}
	good := []string{
		`Get-Process | Sort-Object CPU -Descending | Select-Object -First 5`,
		`Get-Service | Where-Object {$_.Status -eq 'Running'}`,
		`ping -n 2 192.168.1.1`,
		`hostname`,
	}
	for _, c := range good {
		if !IsReadOnlyPS(c) {
			t.Errorf("IsReadOnlyPS(%q) = false", c)
		}
	}
}

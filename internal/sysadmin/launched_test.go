package sysadmin

import "testing"

func TestParseLaunchedProcs(t *testing.T) {
	out := "1234|MetroExodus|F:\\Metro Exodus\\MetroExodus.exe\r\n88|powershell|C:\\x\r\n5|svchost|\r\n4|System|\r\nbad\r\n77|explorer|C:\\Windows\\explorer.exe\r\n900|Launcher.exe|F:\\l.exe\r\n"
	got := ParseLaunchedProcs(out)
	if len(got) != 2 || got[0].Name != "metroexodus" || got[0].PID != 1234 || got[1].Name != "launcher" {
		t.Fatalf("got %+v", got)
	}
}

func TestStopProcessPS(t *testing.T) {
	cmd, err := StopProcessPS("MetroExodus.exe")
	if err != nil || cmd != "Stop-Process -Name 'metroexodus' -Force -ErrorAction SilentlyContinue" {
		t.Fatalf("%q %v", cmd, err)
	}
	for _, bad := range []string{"", "*", "explorer", "a b", "1234", "x;calc", "..\\x", "sshd"} {
		if _, err := StopProcessPS(bad); err == nil {
			t.Errorf("must refuse %q", bad)
		}
	}
	if !IsSafeChangePS(cmd) {
		t.Errorf("the built command must be class S: %s", cmd)
	}
}

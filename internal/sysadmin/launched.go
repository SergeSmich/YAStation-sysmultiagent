package sysadmin

import (
	"errors"
	"strconv"
	"strings"
)

// LaunchedProc is a process that appeared after a program was started.
type LaunchedProc struct {
	PID  int
	Name string // without .exe, lower case
	Path string
}

// LaunchedProcsPS lists processes of the user's sessions that started in the
// last window seconds (after a short pause for the program to come up). Output
// lines: pid|name|path. Read-only.
func LaunchedProcsPS(window int) string {
	if window < 10 || window > 300 {
		window = 45
	}
	return "Start-Sleep -Seconds 5; Get-Process -ErrorAction SilentlyContinue | " +
		"Where-Object { $_.SessionId -ne 0 -and $_.StartTime -gt (Get-Date).AddSeconds(-" + strconv.Itoa(window) + ") } | " +
		"ForEach-Object { '{0}|{1}|{2}' -f $_.Id, $_.ProcessName, $_.Path }"
}

var launchedNoise = map[string]bool{
	"powershell": true, "pwsh": true, "cmd": true, "conhost": true, "ssh": true, "ssh-agent": true,
	"wmiprvse": true, "backgroundtaskhost": true, "smartscreen": true, "searchprotocolhost": true,
	"searchfilterhost": true, "werfault": true, "taskhostw": true, "sihost": true, "ctfmon": true,
}

// ParseLaunchedProcs reads the output of LaunchedProcsPS and drops system and
// helper processes: only what a human could mean by «this program» stays.
func ParseLaunchedProcs(out string) []LaunchedProc {
	var res []LaunchedProc
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(parts) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || pid <= 4 {
			continue
		}
		name, err := NormProcName(parts[1])
		if err != nil || launchedNoise[name] {
			continue
		}
		p := LaunchedProc{PID: pid, Name: name}
		if len(parts) == 3 {
			p.Path = strings.TrimSpace(parts[2])
		}
		res = append(res, p)
		if len(res) >= 12 {
			break
		}
	}
	return res
}

// NormProcName validates a process name for stopping: one plain name without
// wildcards, ids or paths, not a process Windows or the remote session needs.
func NormProcName(s string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.TrimSuffix(strings.Trim(n, "'\""), ".exe")
	if n == "" || !psProcName.MatchString(n) || !psHasLetter.MatchString(n) {
		return "", errors.New("нужно простое имя процесса без масок и путей")
	}
	if psCriticalProcs[n] {
		return "", errors.New("этот процесс нужен системе: останавливать его нельзя")
	}
	return n, nil
}

// StopProcessPS stops every process of that name (the instances of one program).
func StopProcessPS(name string) (string, error) {
	n, err := NormProcName(name)
	if err != nil {
		return "", err
	}
	return "Stop-Process -Name '" + n + "' -Force -ErrorAction SilentlyContinue", nil
}

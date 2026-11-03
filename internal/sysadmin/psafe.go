package sysadmin

import (
	"regexp"
	"strings"
)

// IsSafeChangePS reports whether a Windows command is a harmless, reversible
// change that the sysadmin may run without asking (class S): clearing the
// user's Temp folder or starting a program without arguments. It is as strict
// as IsReadOnlyPS: anything that is not exactly one of these shapes is not safe.
func IsSafeChangePS(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" || len(cmd) > 300 {
		return false
	}
	for i := 0; i < 4; i++ {
		next := strings.TrimSpace(psSafeRedirect.ReplaceAllString(cmd, " "))
		if next == cmd {
			break
		}
		cmd = next
	}
	for _, r := range cmd {
		switch r {
		case ';', '|', '&', '<', '>', '(', ')', '{', '}', '[', ']', '@', '#', '!', '`', '\n', '\r', '%', ',':
			return false
		}
		if r < 0x20 && r != '\t' {
			return false
		}
	}
	low := strings.ToLower(cmd)
	for _, s := range sensitiveMarkers {
		if strings.Contains(low, s) {
			return false
		}
	}
	fields := psFields(cmd)
	if len(fields) < 2 {
		return false
	}
	head := strings.ToLower(fields[0])
	args := fields[1:]
	switch head {
	case "remove-item", "ri", "del", "rm", "erase":
		return psTempCleanup(args)
	case "start-process", "start", "saps":
		return psLaunch(args)
	case "stop-process", "spps", "kill":
		return psStop(args)
	}
	return false
}

var psProcName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)
var psHasLetter = regexp.MustCompile(`[a-z]`)

// psCriticalProcs are processes whose end breaks Windows, the remote session or
// the desktop; stopping one always asks.
var psCriticalProcs = map[string]bool{
	"system": true, "idle": true, "registry": true, "smss": true, "csrss": true, "wininit": true, "winlogon": true,
	"services": true, "lsass": true, "svchost": true, "dwm": true, "fontdrvhost": true, "explorer": true,
	"sshd": true, "ssh-agent": true, "conhost": true, "audiodg": true, "spoolsv": true, "memcompression": true,
	"sihost": true, "taskhostw": true, "runtimebroker": true, "searchhost": true, "startmenuexperiencehost": true,
	"shellexperiencehost": true, "ctfmon": true, "securityhealthservice": true, "msmpeng": true, "wudfhost": true,
	"dllhost": true, "lsm": true, "dashost": true, "powershell": true, "pwsh": true, "cmd": true, "ssh": true,
	"powershell_ise": true, "wmiprvse": true,
}

// psStop accepts one plain process name (no wildcards, no id) with optional
// -Force: the human asked to close a program, and ending it is the request.
func psStop(args []string) bool {
	name := ""
	for i := 0; i < len(args); i++ {
		a := strings.ToLower(args[i])
		switch a {
		case "-force", "-erroraction", "-ea", "silentlycontinue", "-name":
			continue
		}
		if name != "" {
			return false
		}
		name = strings.TrimSuffix(strings.Trim(a, "'\""), ".exe")
	}
	if name == "" || !psProcName.MatchString(name) || !psHasLetter.MatchString(name) {
		return false
	}
	return !psCriticalProcs[name]
}

var psTempPath = regexp.MustCompile(`(?i)^["']?\$env:(?:temp|tmp)\\\*["']?$|^["']?\$env:localappdata\\temp\\\*["']?$`)

func psTempCleanup(args []string) bool {
	paths := 0
	for i := 0; i < len(args); i++ {
		a := strings.ToLower(args[i])
		switch a {
		case "-recurse", "-force", "-erroraction", "-ea", "silentlycontinue", "-path":
			continue
		}
		if psTempPath.MatchString(args[i]) {
			paths++
			continue
		}
		return false
	}
	return paths == 1
}

// psAllowedApps is the only set of programs the agent may start unasked.
var psAllowedApps = map[string]bool{
	"notepad": true, "calc": true, "mspaint": true, "explorer": true, "chrome": true, "firefox": true,
	"msedge": true, "spotify": true, "steam": true, "discord": true, "telegram": true, "vlc": true,
	"code": true, "obs64": true, "taskmgr": true, "snippingtool": true, "wordpad": true,
}

func psLaunch(args []string) bool {
	if len(args) == 2 && strings.EqualFold(args[0], "-FilePath") {
		args = args[1:]
	}
	if len(args) != 1 {
		return false
	}
	name := strings.ToLower(strings.Trim(args[0], "'\""))
	name = strings.TrimSuffix(name, ".exe")
	return psAllowedApps[name]
}

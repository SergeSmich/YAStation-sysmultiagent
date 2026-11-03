package sysadmin

import (
	"regexp"
	"strings"
)

// destructiveRe matches commands that must keep asking even inside an approved
// plan: removal, overwriting moves, stopping things, accounts, firewall, power.
var destructiveRe = regexp.MustCompile(`(?i)(^|[^a-z0-9_-])(remove-[a-z]+|rm|del|erase|rmdir|rd|format(-volume)?|uninstall(-[a-z]+)?|stop-(process|service|computer)|restart-(computer|service)|taskkill|kill|killall|pkill|set-executionpolicy|set-acl|icacls|takeown|netsh|net|reg|schtasks|sc|disable-[a-z]+|clear-[a-z]+|mkfs|dd|shutdown|reboot|poweroff|halt|move-item|mv|rename-item|ren|chmod|chown|sudo|su|passwd|usermod|userdel|apt(-get)?|dpkg|pip|npm|systemctl|service|crontab|diskpart|bcdedit|cipher|wmic|invoke-expression|iex|invoke-webrequest|iwr|curl|wget|downloadstring|start-bitstransfer)($|[^a-z0-9_-])`)

// IsDestructive reports whether a command is outside what a task plan covers.
// The test is deliberately broad: a false positive only costs one more question.
func IsDestructive(command string) bool {
	return destructiveRe.MatchString(strings.ReplaceAll(command, "\\", " "))
}

// planSafeFirst are the commands an approved plan may run without more questions:
// creating and copying files, reading, writing output. Everything else asks.
var planSafeFirst = map[string]bool{
	// PowerShell
	"new-item": true, "ni": true, "set-content": true, "add-content": true, "out-file": true, "copy-item": true, "copy": true, "cpi": true,
	"mkdir": true, "md": true, "set-location": true, "cd": true, "write-output": true, "write-host": true, "echo": true,
	"test-path": true, "get-content": true, "gc": true, "type": true, "get-childitem": true, "gci": true, "dir": true, "ls": true,
	"get-item": true, "gi": true, "get-date": true, "select-object": true, "select": true, "sort-object": true, "measure-object": true,
	"resolve-path": true, "join-path": true, "split-path": true, "get-location": true, "pwd": true, "start-sleep": true,
	// POSIX
	"touch": true, "cp": true, "cat": true, "head": true, "tail": true, "grep": true, "wc": true, "printf": true, "tee": true, "sort": true,
	"find": true, "stat": true, "du": true, "df": true, "basename": true, "dirname": true,
}

var planBadTokens = []string{"`", "$(", "${", "{", "}", "(", ")", "-enc", "-delete", "-exec", "-execdir", "-fprint", "-fls", "-ok"}

// planBadTargets are places where a written file would run code later (profiles,
// autostart, scheduled work) or that belong to the system; a plan never touches them.
var planBadTargets = []string{
	"profile", "/startup", ".bashrc", ".bash_", "/etc/", "cron", "systemd", "autorun", "/windows/", "system32",
	".ps1", ".psm1", ".bat", ".cmd", ".vbs", ".lnk", ".sh", ".exe", ".dll", ".msi", ".reg", ".js", ".hta", ".scr",
	"/.ssh", "authorized_keys", "/boot/", "/usr/", "/bin/", "/sbin/", "/lib/",
}

// PlanSafe reports whether every statement of command starts with a command an
// approved plan covers and the text has no call operators, sub-expressions or
// script blocks that could hide another command.
func PlanSafe(command string) bool {
	c := strings.TrimSpace(command)
	if c == "" || len(c) > 2000 {
		return false
	}
	low := strings.ToLower(c)
	for _, bad := range planBadTokens {
		if strings.Contains(low, bad) {
			return false
		}
	}
	// Variables: only a few well-known folders may be expanded; any other `$`,
	// wildcards, `~` and `%` could spell a sensitive path past the marker check.
	flat := low
	for _, v := range []string{"$env:userprofile", "$env:temp", "$env:public"} {
		flat = strings.ReplaceAll(flat, v, "")
	}
	if strings.ContainsAny(flat, "$*?[]~%") {
		return false
	}
	// Network paths leak credentials and fetch foreign files.
	if strings.Contains(flat, `\\`) || strings.Contains(flat, "//") {
		return false
	}
	slashed := strings.ReplaceAll(flat, `\`, "/")
	for _, m := range planBadTargets {
		if strings.Contains(slashed, m) {
			return false
		}
	}
	c = strings.ReplaceAll(c, "&&", ";")
	c = strings.ReplaceAll(c, "||", ";")
	if strings.ContainsAny(c, "&") {
		return false
	}
	seg := strings.FieldsFunc(c, func(r rune) bool { return r == ';' || r == '|' || r == '\n' || r == '\r' })
	if len(seg) == 0 {
		return false
	}
	for _, s := range seg {
		f := strings.Fields(strings.TrimSpace(s))
		if len(f) == 0 {
			continue
		}
		first := strings.ToLower(f[0])
		if !planSafeFirst[first] {
			return false
		}
	}
	return true
}

// HasRule reports whether an operator or built-in rule names this subject.
func (p *Policy) HasRule(tool, subject string) bool { return hasMatchingRule(p, tool, subject) }

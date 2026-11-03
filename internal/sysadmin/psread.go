package sysadmin

import (
	"regexp"
	"strings"
)

// IsReadOnlyPS reports whether a command line for a Windows host is a plain
// PowerShell pipeline of read-only cmdlets. Like isReadOnly it is strict: the
// Read class runs without asking, so any doubt ends as "not read-only".
//
// Accepted: cmdlets from a fixed list joined by "|"; quoted strings; "(…)"
// groups that are themselves such pipelines; "{ $_.Prop -op value }" blocks
// after Where-Object; $env:NAME, $null, $true, $false. Refused: statements,
// assignments, redirects, method calls, static members, subexpressions,
// splatting, UNC paths, secret places.
func IsReadOnlyPS(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" || len(cmd) > 1500 {
		return false
	}
	for i := 0; i < 6; i++ {
		next := strings.TrimSpace(psSafeRedirect.ReplaceAllString(cmd, " "))
		if next == cmd {
			break
		}
		cmd = next
	}
	if cmd == "" || strings.ContainsRune(cmd, '`') || psHasOddRunes(cmd) {
		return false
	}
	for _, r := range cmd {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	low := strings.ToLower(cmd)
	for _, s := range sensitiveMarkers {
		if strings.Contains(low, s) {
			return false
		}
	}
	for _, s := range psSensitive {
		if strings.Contains(low, s) {
			return false
		}
	}
	return psPipelineOK(cmd, 0)
}

var psSafeRedirect = regexp.MustCompile(`(?i)(^|\s)(?:[12*]?>\s?\$null|2>&1)(\s|$)`)

// psSensitive are registry and system places that hold secrets.
var psSensitive = []string{"winlogon", `\lsa`, `:\sam`, `:\security`, `\sam`, "wlan", "credman", "vaultcmd", "cmdkey", "password", "secret", "token", "credential",
	"consolehost_history", ".kdbx", "local state", "cookies", "login data", "logins.json", "key4.db", ".rdp", "hosts.yml",
	".azure", ".docker", ".kube", ".aws", ".gnupg", "ntuser.dat", `microsoft\credentials`, `microsoft\protect`, "logname security", "-logname:security",
	"win32_product"}

// psHasOddRunes finds characters PowerShell treats as quotes, dashes or
// whitespace but a plain scanner does not: smart quotes, Unicode dashes and
// spaces, line separators, BOM.
func psHasOddRunes(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x2000 && r <= 0x206f, r == 0x00a0, r == 0x0085, r == 0xfeff, r == 0x3000, r == 0x00ad, r == 0x00ab, r == 0x00bb:
			return true
		}
	}
	return false
}

// psAllowedEnv are environment variables that hold no secrets.
var psAllowedEnv = map[string]bool{"temp": true, "tmp": true, "username": true, "computername": true, "userdomain": true,
	"localappdata": true, "appdata": true, "userprofile": true, "programfiles": true, "systemroot": true, "windir": true,
	"os": true, "processor_architecture": true, "systemdrive": true, "homedrive": true, "homepath": true}

const (
	psGroup = '\x01'
	psBlock = '\x02'
)

const psCond = `\$_\.[A-Za-z_][A-Za-z0-9_.]*[ \t]+-(?:eq|ne|gt|lt|ge|le|like|notlike|match|notmatch|contains|notcontains)[ \t]+(?:'[^'{}$]*'|"[^"{}$]*"|[A-Za-z0-9_.\\:*\-]+)`

var psBlockRe = regexp.MustCompile(`^[ \t]*` + psCond + `(?:[ \t]+-(?:and|or)[ \t]+` + psCond + `)*[ \t]*$`)

var psDollarRe = regexp.MustCompile(`^\$(?:env:([A-Za-z_][A-Za-z0-9_]*)|null|true|false)(?:[^A-Za-z0-9_(]|$)`)

func psDollarOK(rs []rune, i int) bool {
	end := i + 64
	if end > len(rs) {
		end = len(rs)
	}
	m := psDollarRe.FindStringSubmatch(string(rs[i:end]))
	if m == nil {
		return false
	}
	return m[1] == "" || psAllowedEnv[strings.ToLower(m[1])]
}

func psPipelineOK(s string, depth int) bool {
	if depth > 3 {
		return false
	}
	stages, ok := psScan(s, depth)
	if !ok || len(stages) == 0 {
		return false
	}
	for n, st := range stages {
		if !psStageOK(st, n) {
			return false
		}
	}
	return true
}

// psScan splits s into pipeline stages. "(…)" groups are validated recursively
// and replaced by psGroup, where-blocks by psBlock.
func psScan(s string, depth int) ([]string, bool) {
	rs := []rune(s)
	var stages []string
	var cur []rune
	var quote rune
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if quote != 0 {
			cur = append(cur, r)
			switch {
			case r == quote:
				quote = 0
			case r == '$' && quote == '"':
				if !psDollarOK(rs, i) {
					return nil, false
				}
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			cur = append(cur, r)
		case '|':
			if i+1 < len(rs) && rs[i+1] == '|' {
				return nil, false
			}
			stages = append(stages, string(cur))
			cur = cur[:0]
		case '(':
			if n := len(cur); n > 0 && cur[n-1] != ' ' && cur[n-1] != '\t' {
				return nil, false // a method or function call
			}
			j := psMatchParen(rs, i)
			if j < 0 || !psPipelineOK(string(rs[i+1:j]), depth+1) {
				return nil, false
			}
			cur = append(cur, psGroup)
			i = j
		case '{':
			j := -1
			for k := i + 1; k < len(rs); k++ {
				if rs[k] == '{' {
					return nil, false
				}
				if rs[k] == '}' {
					j = k
					break
				}
			}
			if j < 0 || !psBlockRe.MatchString(string(rs[i+1:j])) {
				return nil, false
			}
			cur = append(cur, psBlock)
			i = j
		case ')', '}', ';', '&', '<', '>', '@', '=', '!', '#', '[', ']':
			return nil, false
		case '$':
			if !psDollarOK(rs, i) {
				return nil, false
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	if quote != 0 {
		return nil, false
	}
	stages = append(stages, string(cur))
	return stages, true
}

// psMatchParen returns the index of the ")" that closes rs[open], or -1.
func psMatchParen(rs []rune, open int) int {
	depth := 0
	var quote rune
	for k := open; k < len(rs); k++ {
		r := rs[k]
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return -1
}

// psFields splits a stage on whitespace outside quotes.
func psFields(s string) []string {
	var out []string
	var cur []rune
	var quote rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if quote != 0 {
			cur = append(cur, r)
			if r == quote {
				quote = 0
			}
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			cur = append(cur, r)
		case r == ' ' || r == '\t':
			flush()
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// psReadCmds are cmdlets, aliases and native tools that only read.
var psReadCmds = map[string]bool{
	"get-childitem": true, "gci": true, "ls": true, "dir": true,
	"get-item": true, "gi": true, "get-itemproperty": true, "gp": true,
	"get-content": true, "gc": true, "cat": true, "type": true,
	"get-process": true, "gps": true, "ps": true,
	"get-service": true, "gsv": true,
	"get-volume": true, "get-psdrive": true, "gdr": true, "get-disk": true, "get-physicaldisk": true,
	"get-partition": true,
	"get-ciminstance": true, "get-wmiobject": true, "gwmi": true, "get-computerinfo": true,
	"get-netadapter": true, "get-netipaddress": true, "get-netipconfiguration": true,
	"get-nettcpconnection": true, "get-netroute": true, "get-dnsclientserveraddress": true,
	"get-printer": true, "get-printerport": true, "get-printerdriver": true, "get-printjob": true,
	"get-pnpdevice": true, "get-hotfix": true, "get-date": true,
	"get-location": true, "gl": true, "pwd": true, "get-filehash": true,
	"get-appxpackage": true, "get-package": true, "get-module": true, "get-host": true,
	"get-culture": true, "get-timezone": true, "get-uptime": true, "get-scheduledtask": true,
	"get-scheduledtaskinfo": true, "get-netfirewallprofile": true,
	"get-smbshare": true, "get-localuser": true, "get-localgroup": true,
	"test-path": true,
	"split-path": true, "join-path": true, "resolve-path": true, "convert-path": true,
	"select-object": true, "select": true, "sort-object": true, "sort": true,
	"measure-object": true, "measure": true, "group-object": true, "group": true,
	"where-object": true, "where": true, "?": true,
	"format-table": true, "ft": true, "format-list": true, "fl": true, "format-wide": true, "fw": true,
	"out-string": true, "convertto-json": true, "convertto-csv": true,
	"select-string": true, "sls": true, "write-output": true, "echo": true,
	"hostname": true, "whoami": true, "ipconfig": true, "systeminfo": true, "tasklist": true,
	"netstat": true, "ping": true, "driverquery": true, "getmac": true, "arp": true,
}

// psFileCmds read file or registry content from a path: only as the first
// stage, with no wildcards and no computed arguments, so a listing cannot be
// piped into a reader and a pattern cannot reach a secret by globbing.
var psFileCmds = map[string]bool{
	"get-childitem": true, "gci": true, "ls": true, "dir": true,
	"get-item": true, "gi": true, "get-itemproperty": true, "gp": true,
	"get-content": true, "gc": true, "cat": true, "type": true,
	"get-filehash": true, "select-string": true, "sls": true, "test-path": true,
	"resolve-path": true, "convert-path": true,
}

var psGroupHead = regexp.MustCompile("^\x01(?:\\.[a-z_][a-z0-9_]*)*$")
var psNetHost = regexp.MustCompile(`^(?:localhost|127\.0\.0\.1|192\.168\.\d{1,3}\.\d{1,3}|10\.\d{1,3}\.\d{1,3}\.\d{1,3}|[a-z][a-z0-9\-]{0,30})$`)
var psSmallInt = regexp.MustCompile(`^[1-9]$`)
var psNetstat = regexp.MustCompile(`^-[ano]{1,3}$`)

func psStageOK(stage string, idx int) bool {
	fields := psFields(stage)
	if len(fields) == 0 {
		return false
	}
	head := strings.ToLower(fields[0])
	args := fields[1:]
	if strings.HasPrefix(head, string(rune(psGroup))) {
		return psGroupHead.MatchString(head) && len(args) == 0
	}
	if !psReadCmds[head] {
		return false
	}
	whereLike := head == "where-object" || head == "where" || head == "?"
	for _, a := range args {
		la := strings.ToLower(a)
		if strings.ContainsRune(la, psBlock) && !whereLike {
			return false
		}
		if strings.ContainsRune(la, psGroup) {
			// a group is allowed only as a whole argument, and not for a
			// file reader or a network tool
			if !psGroupHead.MatchString(la) || psFileCmds[head] || head == "ping" {
				return false
			}
		}
		t := strings.TrimSpace(strings.Trim(la, `'"`))
		scrubbed := strings.ReplaceAll(t, "$env:", "")
		for _, bad := range []string{"env:", "variable:", "cert:", "wsman:", "function:", "alias:", `\\`, "//", "::"} {
			if strings.Contains(scrubbed, bad) {
				return false
			}
		}
		// -Name:value and abbreviated blocking switches
		if strings.HasPrefix(t, "-") && strings.Contains(t, ":") {
			return false
		}
		if strings.HasPrefix(t, "-w") && len(t) <= 5 && strings.HasPrefix("-wait", t) {
			return false
		}
		if strings.HasPrefix(t, "-p") && len(t) >= 3 && strings.HasPrefix("-paging", t) {
			return false
		}
		if psFileCmds[head] && strings.ContainsAny(t, "*?") {
			return false
		}
	}
	if psFileCmds[head] && idx != 0 {
		return false
	}
	switch head {
	case "ipconfig":
		for _, a := range args {
			if strings.ToLower(a) != "/all" {
				return false
			}
		}
	case "arp":
		if len(args) != 1 || strings.ToLower(args[0]) != "-a" {
			return false
		}
	case "hostname", "whoami", "systeminfo", "tasklist", "driverquery", "getmac":
		if len(args) != 0 {
			return false
		}
	case "netstat":
		if len(args) > 1 || (len(args) == 1 && !psNetstat.MatchString(strings.ToLower(args[0]))) {
			return false
		}
	case "ping":
		switch len(args) {
		case 1:
			if !psNetHost.MatchString(strings.ToLower(args[0])) {
				return false
			}
		case 3:
			if strings.ToLower(args[0]) != "-n" || !psSmallInt.MatchString(args[1]) || !psNetHost.MatchString(strings.ToLower(args[2])) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

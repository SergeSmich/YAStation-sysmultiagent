package sysadmin

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

type Mode string

const (
	ModeAllow Mode = "allow"
	ModeAsk   Mode = "ask"
	ModeDeny  Mode = "deny"
)

type Risk int

const (
	RiskRead Risk = iota
	RiskChange
	RiskForbidden
)

type Rule struct {
	Tool  string `json:"tool"`
	Match string `json:"match"`
	Mode  Mode   `json:"mode"`
}

type Policy struct {
	Default Mode   `json:"default"`
	Rules   []Rule `json:"rules"`
}

func (p *Policy) Validate() error {
	if p.Default == "" {
		p.Default = ModeAsk
	}
	switch p.Default {
	case ModeAllow, ModeAsk, ModeDeny:
	default:
		return fmt.Errorf("invalid default mode")
	}
	for i := range p.Rules {
		r := &p.Rules[i]
		if r.Tool == "" {
			return fmt.Errorf("rule %d: empty tool", i)
		}
		switch r.Mode {
		case ModeAllow, ModeAsk, ModeDeny:
		default:
			return fmt.Errorf("rule %d: invalid mode", i)
		}
	}
	return nil
}

func matchRule(r Rule, tool, subject string) bool {
	if r.Tool != tool && r.Tool != "*" {
		return false
	}
	if r.Match == "" {
		return true
	}
	if strings.HasPrefix(r.Match, "re:") {
		re, err := regexp.Compile(strings.TrimPrefix(r.Match, "re:"))
		if err != nil {
			return false
		}
		return re.MatchString(subject)
	}
	ok, _ := path.Match(r.Match, subject)
	return ok
}

func (p *Policy) Decide(tool, subject string) Mode {
	if p.Default == "" {
		p.Default = ModeAsk
	}
	for _, r := range p.Rules {
		if matchRule(r, tool, subject) {
			return r.Mode
		}
	}
	return p.Default
}

// ClassifyShell
func ClassifyShell(command string) Risk {
	// The forbidden list is checked on the whole line first, so no way of
	// splitting or nesting a command can hide a destructive one from it.
	if isForbidden(stripSudo(strings.ToLower(command))) {
		return RiskForbidden
	}
	for _, part := range splitShell(command) {
		if classifyPart(strings.TrimSpace(part)) == RiskForbidden {
			return RiskForbidden
		}
	}
	// Read is granted only to a plain pipeline of known read-only commands;
	// everything else, including anything unparsed, is a change.
	if isReadOnly(command) {
		return RiskRead
	}
	return RiskChange
}

func maxRisk(a, b Risk) Risk {
	if a > b {
		return a
	}
	return b
}

func splitShell(cmd string) []string {
	// replace newlines with ;
	s := strings.ReplaceAll(cmd, "\n", ";")
	// split by ; && || |
	s = strings.ReplaceAll(s, "\r", ";")
	seps := []string{"&&", "||", ";", "|", "&"}
	// simple recursive
	var out []string
	cur := s
	for {
		idx := -1
		sep := ""
		for _, se := range seps {
			i := strings.Index(cur, se)
			if i >= 0 && (idx == -1 || i < idx) {
				idx = i
				sep = se
			}
		}
		if idx == -1 {
			out = append(out, cur)
			break
		}
		out = append(out, cur[:idx])
		cur = cur[idx+len(sep):]
	}
	// handle $() and backticks by extracting? We'll also check substrings
	// Additionally split command substitutions by scanning
	var expanded []string
	for _, p := range out {
		expanded = append(expanded, splitSubshells(p)...)
	}
	return expanded
}

func splitSubshells(s string) []string {
	var res []string
	// find $(...) and backticks
	for {
		i := strings.Index(s, "$(")
		j := strings.Index(s, "`")
		if i == -1 && j == -1 {
			res = append(res, s)
			break
		}
		// choose earliest
		pos := -1
		if i != -1 && (j == -1 || i < j) {
			pos = i
		} else {
			pos = j
		}
		// add prefix
		res = append(res, s[:pos])
		// find matching close
		if pos == i {
			// find )
			end := strings.Index(s[pos:], ")")
			if end == -1 {
				res = append(res, s[pos:])
				s = ""
				break
			}
			end += pos
			res = append(res, s[pos+2:end])
			s = s[end+1:]
		} else {
			end := strings.Index(s[pos+1:], "`")
			if end == -1 {
				res = append(res, s[pos:])
				s = ""
				break
			}
			end += pos + 1
			res = append(res, s[pos+1:end])
			s = s[end+1:]
		}
	}
	return res
}

func hasRedirect(cmd string) bool {
	// > file except >/dev/null and 2>&1
	// simple scan
	if strings.Contains(cmd, ">/dev/null") {
		// still check other
	}
	// detect > or >> not followed by /dev/null or &
	i := 0
	for i < len(cmd) {
		if cmd[i] == '>' {
			// check 2>&1
			// find next non-space
			j := i + 1
			for j < len(cmd) && (cmd[j] == '>' || cmd[j] == ' ') {
				j++
			}
			target := strings.TrimSpace(cmd[j:])
			// take first token
			fields := strings.Fields(target)
			tgt := ""
			if len(fields) > 0 {
				tgt = fields[0]
			}
			if tgt != "/dev/null" && !strings.HasPrefix(tgt, "&") {
				// if 2>&1 style already handled? tgt like "1"? 
				if !(i > 0 && cmd[i-1] == '&') {
					return true
				}
			}
		}
		i++
	}
	return false
}

var forbiddenPatterns = []string{
	"rm -rf /", "rm -rf ~", "rm -rf $HOME", "mkfs", "dd if=", "shred", "wipefs", "fdisk", "parted",
}

func stripSudo(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 || fields[0] != "sudo" {
		return s
	}
	i := 1
	for i < len(fields) && strings.HasPrefix(fields[i], "-") {
		if fields[i] == "-u" || fields[i] == "-g" {
			i += 2
			continue
		}
		i++
	}
	return strings.Join(fields[i:], " ")
}

func classifyPart(p string) Risk {
	low := strings.ToLower(strings.TrimSpace(p))
	if low == "" {
		return RiskChange
	}
	low = stripSudo(low)
	// Forbidden checks
	if isForbidden(low) {
		return RiskForbidden
	}
	return RiskChange
}

func isForbidden(s string) bool {
	// rm -rf root/home
	rmForbidden := strings.Contains(s, "rm") && (strings.Contains(s, "-rf") || strings.Contains(s, "-fr") || (strings.Contains(s, "-r") && strings.Contains(s, "-f")))
	if rmForbidden {
		for _, t := range []string{"/", "~", "$home", "/home", "/data", "/var", "/etc", "/boot"} {
			if strings.Contains(s, t) {
				return true
			}
		}
	}
	if strings.HasPrefix(s, "mkfs") {
		return true
	}
	if strings.Contains(s, "dd ") && strings.Contains(s, "of=/dev/") {
		return true
	}
	if strings.Contains(s, "shred") || strings.Contains(s, "wipefs") {
		return true
	}
	if strings.Contains(s, "fdisk") || strings.Contains(s, "parted") {
		// writing? assume forbidden
		return true
	}
	if strings.Contains(s, "> /dev/sd") || strings.Contains(s, ">/dev/sd") {
		return true
	}
	if strings.Contains(s, ":(){") || strings.Contains(s, "fork bomb") {
		return true
	}
	if (strings.Contains(s, "chmod") || strings.Contains(s, "chown")) && strings.Contains(s, "-r") && strings.Contains(s, " /") {
		return true
	}
	for _, c := range []string{"reboot", "shutdown", "poweroff", "halt"} {
		if strings.Contains(s, c) {
			return true
		}
	}
	// Windows (PowerShell): disks, boot and power.
	for _, c := range []string{"stop-computer", "restart-computer", "format-volume", "clear-disk", "initialize-disk",
		"remove-partition", "diskpart", "bcdedit", "format c:", "cipher /w"} {
		if strings.Contains(s, c) {
			return true
		}
	}
	if strings.Contains(s, "init 0") || strings.Contains(s, "init 6") {
		return true
	}
	if strings.Contains(s, "docker system prune") {
		return true
	}
	if strings.Contains(s, "docker volume rm") || strings.Contains(s, "docker volume prune") {
		return true
	}
	if strings.Contains(s, "docker rm") || strings.Contains(s, "docker rmi") {
		return true
	}
	if strings.Contains(s, "apt") || strings.Contains(s, "apt-get") || strings.Contains(s, "dpkg") {
		if strings.Contains(s, "remove") || strings.Contains(s, "purge") || strings.Contains(s, "autoremove") || strings.Contains(s, " -r") || strings.Contains(s, "-r ") {
			return true
		}
	}
	protected := []string{"klipper", "moonraker", "mosquitto", "nginx", "smbd", "nmbd", "docker", "ssh", "sshd"}
	for _, u := range protected {
		if strings.Contains(s, "systemctl") && (strings.Contains(s, " stop ") || strings.Contains(s, " disable ") || strings.Contains(s, " mask ") || strings.Contains(s, " kill ")) && strings.Contains(s, u) {
			return true
		}
	}
	return false
}

// isReadOnly reports whether the whole command line is a plain pipeline of
// well-known read-only commands. It is deliberately strict: the Read class is
// executed without asking, so every doubt must end in "not read-only" (the
// caller then treats the command as a change and asks the operator).
//
// The line must contain no shell metacharacters, quotes, globs, redirects,
// substitutions or control characters at all, so nothing can hide a second
// command or a write. Each pipeline stage is then checked by its own rule.
func isReadOnly(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" || len(cmd) > 1000 {
		return false
	}
	// The only redirects that are not a write: to /dev/null and "2>&1".
	for i := 0; i < 6; i++ {
		next := strings.TrimSpace(safeRedirect.ReplaceAllString(cmd, " "))
		if next == cmd {
			break
		}
		cmd = next
	}
	if cmd == "" {
		return false
	}
	for _, r := range cmd {
		if r < 0x20 || r == 0x7f {
			return false
		}
		if strings.ContainsRune(";&<>()`$\\{}*?!~#\"'[]", r) {
			return false
		}
	}
	low := strings.ToLower(cmd)
	for _, s := range sensitiveMarkers {
		if strings.Contains(low, s) {
			return false
		}
	}
	for _, stage := range strings.Split(cmd, "|") {
		if !readStage(strings.Fields(stage)) {
			return false
		}
	}
	return true
}

var safeRedirect = regexp.MustCompile(`(^|\s)(?:[12]?>\s?/dev/null|&>\s?/dev/null|2>&1)(\s|$)`)

// sensitiveMarkers keep key and secret material away from an automatic read:
// the output goes to a language model provider.
var sensitiveMarkers = []string{
	"shadow", "id_rsa", "id_ed25519", "id_ecdsa", ".ssh", "secret", "token",
	".env", "private", "credential", "gnupg", "password", "authorized_keys", "known_hosts",
}

func argHas(args []string, bad ...string) bool {
	for _, a := range args {
		la := strings.ToLower(a)
		for _, b := range bad {
			if la == b || (strings.HasSuffix(b, "=") && strings.HasPrefix(la, b)) || (strings.HasSuffix(b, "*") && strings.HasPrefix(la, strings.TrimSuffix(b, "*"))) {
				return true
			}
		}
	}
	return false
}

func readStage(f []string) bool {
	if len(f) == 0 {
		return false
	}
	if f[0] == "sudo" {
		f = f[1:]
		if len(f) > 0 && f[0] == "-n" {
			f = f[1:]
		}
		if len(f) == 0 || strings.HasPrefix(f[0], "-") {
			return false
		}
	}
	name, args := f[0], f[1:]
	switch name {
	case "ls", "cat", "head", "df", "du", "free", "uptime", "uname", "whoami", "lsblk", "id", "wc",
		"stat", "grep", "pwd", "lscpu", "lsusb", "lspci", "findmnt", "uniq", "cut", "tr", "nproc":
		if name == "uniq" && len(args) > 0 {
			// "uniq in out" writes the second file.
			for _, a := range args {
				if !strings.HasPrefix(a, "-") {
					return false
				}
			}
		}
		return true
	case "tail":
		return !argHas(args, "-f", "-ff", "--follow", "--follow=*")
	case "hostname":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return false
			}
		}
		return true
	case "file":
		return !argHas(args, "-c", "--compile")
	case "ss":
		return !argHas(args, "-k", "--kill") && !strings.Contains(strings.Join(args, " "), "K")
	case "netstat", "ps":
		return true
	case "date":
		for _, a := range args {
			if !strings.HasPrefix(a, "+") && a != "-u" && a != "--utc" && a != "-R" {
				return false
			}
		}
		return true
	case "sort":
		for _, a := range args {
			la := strings.ToLower(a)
			if la == "-o" || strings.HasPrefix(la, "--output") || strings.HasPrefix(la, "--compress-program") ||
				(strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "o")) {
				return false
			}
		}
		return true
	case "find":
		return !argHas(args, "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls")
	case "dmesg":
		return len(args) == 0 || (len(args) == 1 && args[0] == "-T")
	case "mount":
		return len(args) == 0
	case "top":
		return len(args) == 1 && args[0] == "-bn1"
	case "journalctl":
		return !argHas(args, "-f", "--follow", "--vacuum*", "--rotate", "--flush", "--sync", "--setup-keys",
			"--relinquish-var", "--smart-relinquish-var", "--update-catalog", "--verify", "--header", "-h",
			"--file*", "--directory*", "-d")
	case "ip":
		rest := args
		for len(rest) > 0 && (rest[0] == "-4" || rest[0] == "-6" || rest[0] == "-s" || rest[0] == "-br" || rest[0] == "-c" || rest[0] == "-j") {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return false
		}
		switch rest[0] {
		case "addr", "address", "route", "link", "neigh", "a", "r", "l", "n":
		default:
			return false
		}
		return !argHas(rest[1:], "add", "del", "delete", "set", "flush", "change", "replace", "append", "up", "down", "save", "restore")
	case "systemctl":
		if len(args) == 0 || argHas(args, "-h", "--host", "-m", "--machine") {
			return false
		}
		switch args[0] {
		case "status", "is-active", "is-enabled", "is-failed", "list-units", "list-unit-files", "list-timers", "show", "cat":
			return true
		}
		return false
	case "docker":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "ps", "images", "inspect":
			return true
		case "logs":
			return !argHas(args[1:], "-f", "--follow")
		case "stats":
			return argHas(args[1:], "--no-stream")
		case "compose":
			return len(args) >= 2 && (args[1] == "ps" || args[1] == "config")
		}
		return false
	case "vcgencmd":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "measure_temp", "measure_volts", "measure_clock", "get_throttled", "get_mem", "version":
			return true
		}
		return false
	}
	return false
}

// ClassifyShellOS is ClassifyShell that also knows PowerShell: on a Windows
// host a plain pipeline of read-only cmdlets is a Read.
func ClassifyShellOS(command string, windows bool) Risk {
	risk := ClassifyShell(command)
	if risk == RiskChange && windows && IsReadOnlyPS(command) {
		return RiskRead
	}
	return risk
}

func (p *Policy) DecideShell(command string) (Mode, Risk) { return p.DecideShellOS(command, false) }

// DecideShellOS decides a command for a host of the given kind.
func (p *Policy) DecideShellOS(command string, windows bool) (Mode, Risk) {
	risk := ClassifyShellOS(command, windows)
	// Class S: harmless and reversible, run at once (the agent reports after).
	// An operator "deny" or "ask" rule for the exact command still wins.
	if risk == RiskChange && windows && IsSafeChangePS(command) {
		if hasMatchingRule(p, "shell", command) {
			return p.Decide("shell", command), risk
		}
		return ModeAllow, risk
	}
	switch risk {
	case RiskForbidden:
		return ModeDeny, risk
	case RiskRead:
		// rules may tighten
		m := p.Decide("shell", command)
		if m == ModeAllow {
			return ModeAllow, risk
		}
		// allow->ask/deny allowed; allow stays? If operator set deny/ask, use it; if no rule, default? read default allow
		// If no matching rule, p.Decide returns Default (ask). But read should allow by default. So:
		// Determine if any rule matched
		matched := false
		for _, r := range p.Rules {
			if matchRule(r, "shell", command) {
				matched = true
				break
			}
		}
		if !matched {
			return ModeAllow, risk
		}
		return m, risk
	default:
		m := p.Decide("shell", command)
		if m == "" {
			m = ModeAsk
		}
		if p.Default == "" {
			p.Default = ModeAsk
		}
		if m == ModeAllow && p.Default == ModeAsk {
			// default ask for change
		}
		if !hasMatchingRule(p, "shell", command) {
			return ModeAsk, risk
		}
		return m, risk
	}
}

func hasMatchingRule(p *Policy, tool, subject string) bool {
	for _, r := range p.Rules {
		if matchRule(r, tool, subject) {
			return true
		}
	}
	return false
}

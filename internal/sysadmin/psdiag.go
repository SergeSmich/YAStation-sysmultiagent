package sysadmin

import (
	"regexp"
	"strconv"
	"strings"
)

// IsDiagnosticPS reports whether a Windows command only looks: searches files,
// lists processes, reads counters and system information. It is wider than
// IsReadOnlyPS (it allows control flow, calculated properties, cmd tools like
// dir/findstr/wmic) and is still strict: every command word must be known and
// nothing that writes, deletes, starts or reconfigures is accepted.
func IsDiagnosticPS(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" || len(cmd) > 2000 || strings.ContainsRune(cmd, '`') {
		return false
	}
	for i := 0; i < 6; i++ {
		next := strings.TrimSpace(psSafeRedirect.ReplaceAllString(cmd, " "))
		if next == cmd {
			break
		}
		cmd = next
	}
	if strings.ContainsAny(cmd, "<>") || psdAmpCall.MatchString(cmd) || psHasOddRunes(cmd) {
		return false
	}
	for _, r := range cmd {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
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
	// Network shares leak credentials; "/namespace:\\root\..." of wmic is local.
	if strings.Contains(strings.ReplaceAll(low, `/namespace:\\root\`, ""), `\\`) || strings.Contains(low, "//") {
		return false
	}
	if psdBadWords.MatchString(low) || psdDynMember.MatchString(low) || !psdEnvOK(low) || !psdMembersOK(cmd) || !psdGuardOK(low) {
		return false
	}
	hash := strings.Contains(low, "@{")
	for _, tok := range psdTokenRe.FindAllString(low, -1) {
		if !strings.Contains(tok, "-") {
			continue
		}
		if psdDeniedFull[tok] {
			return false
		}
		if psdAllowedFull[tok] {
			continue
		}
		verb := tok[:strings.Index(tok, "-")]
		if psdCmdletVerb.MatchString(tok) && !psdAllowedVerbs[verb] {
			return false
		}
	}
	seen := false
	for _, piece := range psdSplitRe.Split(low, -1) {
		ok, used := psdPieceOK(piece, hash)
		if !ok {
			return false
		}
		seen = seen || used
	}
	return seen
}

var (
	psdSplitRe    = regexp.MustCompile(`[;|&\n\r{}()]`)
	psdTokenRe    = regexp.MustCompile(`[a-z][a-z0-9_]*(?:-[a-z0-9_]+)*`)
	psdCmdletVerb = regexp.MustCompile(`^[a-z]+-[a-z][a-z0-9]*$`)
	psdAmpCall    = regexp.MustCompile(`&\s*["'$.\\/({@\[]`)
	psdEnvRe      = regexp.MustCompile(`env:\s*([a-z0-9_]*)`)
	// Endless or remote modes of read-only tools.
	psdBadWords = regexp.MustCompile(`(^|\s)(-w(a(i(t)?)?)?|[-/]every|[-/]repeat|-con(t(i(n(u(o(u(s)?)?)?)?)?)?)?)(\s|$|:)`)
	// A member named through a quote, variable or bracket: $x.'Kill'(), [T]::$m().
	psdDynMember = regexp.MustCompile(`(::|\.)\s*['"$\[]`)
	psdExprWord  = regexp.MustCompile(`[a-z_][a-z0-9_]*`)
	psdQuoted    = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	psdNumber    = regexp.MustCompile(`^[0-9.,\s+*/%-]*$`)
	psdUnitRe    = regexp.MustCompile(`([0-9])(kb|mb|gb|tb)`)
	psdMember    = regexp.MustCompile(`(?i)(\[[a-z.]+\])?\s*(::|\.)\s*([a-z_][a-z0-9_]*)(\s*\()?`)
	psdAssign    = regexp.MustCompile(`^\$[a-z0-9_]+(\[[^\]]*\])?\s*[-+*/]?=\s*`)
	psdPairKey   = regexp.MustCompile(`^([a-z_][a-z0-9_]*)\s*=`)
	psdHeadRe    = regexp.MustCompile(`^[a-z0-9_.:\\/?%-]+`)
)

var psdAllowedMethods = map[string]bool{
	"round": true, "floor": true, "ceiling": true, "abs": true, "min": true, "max": true, "sqrt": true, "pow": true,
	"tostring": true, "substring": true, "trim": true, "trimend": true, "trimstart": true, "split": true,
	"contains": true, "startswith": true, "endswith": true, "tolower": true, "toupper": true,
	"indexof": true, "getfolderpath": true,
}

// psdMembersOK allows method calls and static members only from a short list:
// [math]::X, [Environment]::GetFolderPath and harmless string methods.
func psdMembersOK(cmd string) bool {
	for _, m := range psdMember.FindAllStringSubmatch(cmd, -1) {
		typ, op, name, call := strings.ToLower(m[1]), m[2], strings.ToLower(m[3]), strings.HasSuffix(m[4], "(")
		if op == "::" {
			switch typ {
			case "[math]":
			case "[environment]":
				if name != "getfolderpath" {
					return false
				}
			default:
				return false
			}
			continue
		}
		if call && !psdAllowedMethods[name] {
			return false
		}
	}
	return true
}

var psdKeywords = map[string]bool{
	"if": true, "else": true, "elseif": true, "switch": true,
	"return": true, "try": true, "catch": true, "finally": true, "break": true, "continue": true, "param": true,
	"begin": true, "process": true, "end": true,
}

var psdAllowedVerbs = map[string]bool{
	"get": true, "test": true, "select": true, "where": true, "sort": true, "measure": true, "group": true,
	"convertto": true, "convertfrom": true, "write": true, "join": true, "split": true,
	"resolve": true, "foreach": true,
}

var psdAllowedFull = map[string]bool{
	"start-sleep": true, "format-table": true, "format-list": true, "format-wide": true, "format-custom": true, "out-string": true, "out-host": true, "out-null": true, "nvidia-smi": true,
}

var psdDeniedFull = map[string]bool{
	"get-clipboard": true, "get-credential": true, "get-secret": true, "get-storedcredential": true,
	"write-eventlog": true, "convertfrom-securestring": true, "convertto-securestring": true,
}

// psdHeads are the bare commands that may start a pipeline stage.
var psdHeads = map[string]bool{
	"dir": true, "ls": true, "gci": true, "gc": true, "cat": true, "type": true, "gi": true, "gp": true,
	"gps": true, "ps": true, "gsv": true, "gwmi": true, "gcim": true, "gdr": true, "gl": true, "pwd": true,
	"gcm": true, "gm": true, "select": true, "sort": true, "measure": true, "group": true, "where": true,
	"?": true, "%": true, "foreach": true, "ft": true, "fl": true, "fw": true, "sls": true, "echo": true,
	"findstr": true, "find": true, "where.exe": true, "tasklist": true, "wmic": true, "systeminfo": true,
	"ipconfig": true, "ping": true, "nslookup": true, "hostname": true, "whoami": true, "netstat": true,
	"driverquery": true, "getmac": true, "typeperf": true, "vol": true, "ver": true, "tree": true,
	"arp": true, "tracert": true, "pathping": true, "quser": true, "qwinsta": true, "sleep": true,
	"more": true,
}

// psdHashKeys are the keys of a calculated property of Select-Object / Format-*.
var psdHashKeys = map[string]bool{
	"n": true, "name": true, "l": true, "label": true, "e": true, "expression": true,
	"w": true, "width": true, "a": true, "alignment": true, "f": true, "formatstring": true,
}

var (
	// wmic: any verb that changes something, anywhere in the command.
	psdWmicWords  = regexp.MustCompile(`\b(call|delete|create|set|terminate|assoc|add|rename)\b`)
	psdWmicSwitch = regexp.MustCompile(`^/(format:(list|table|csv|value)|value|namespace:\\\\root\\[a-z0-9\\]+)$`)
	psdRange      = regexp.MustCompile(`([0-9]|\$[a-z0-9_]*)\s*\.\.\s*([0-9]*)`)
	psdSecretRe   = regexp.MustCompile(`\.env($|[\s'"\\])|api[_ -]?key|[\\/~'"\s]\.[a-z0-9_]*[*?\[]`)
	psdConcatRe   = regexp.MustCompile(`['"]\s*\+|\+\s*['"]|%[a-z_][a-z0-9_]*%|\+\+|--`)
	// -ComputerName, -CimSession and their abbreviations reach other hosts.
	psdRemote = regexp.MustCompile(`(^|\s)-(cn|c|co|com|comp|compu|comput|compute|computer|computern|computerna|computernam|computername|cim[a-z]*|cred[a-z]*|ses[a-z]*)(\s|$|:)`)
)

// psdSecretWords are places and file kinds that hold secrets, beyond
// sensitiveMarkers and psSensitive.
var psdSecretWords = []string{
	".pem", ".pfx", ".key", ".p12", ".ppk", ".ovpn", ".npmrc", ".netrc", ".pgpass", "id_", "web data", "leveldb",
	"loginusers.vdf", "win32_environment", "\\environment", "wmic environment", "psreadline", "historysavepath",
	"get-windowsupdatelog", "get-help", "-online", "-showwindow", "\\/", "/\\",
}

// psdGuardOK refuses endless, remote and secret-reading forms that the
// token-by-token check cannot see.
func psdGuardOK(low string) bool {
	for _, s := range psdSecretWords {
		if strings.Contains(low, s) {
			return false
		}
	}
	if psdSecretRe.MatchString(low) || psdConcatRe.MatchString(low) || psdRemote.MatchString(low) {
		return false
	}
	for _, m := range psdRange.FindAllStringSubmatch(low, -1) {
		if n, err := strconv.Atoi(m[2]); err != nil || n > 100000 || strings.HasPrefix(m[1], "$") {
			return false
		}
	}
	if n := strings.Count(low, "sleep"); n > 2 || (n > 0 && (strings.Contains(low, "..") || strings.Contains(low, "%") ||
		strings.Contains(low, "foreach"))) {
		return false
	}
	return true
}

// psdFlagsOK: every switch must be in the list and every bare number small.
func psdFlagsOK(rest string, allowed map[string]bool) bool {
	for _, w := range strings.Fields(rest) {
		if w[0] == '-' && !allowed[w] {
			return false
		}
		if n, err := strconv.Atoi(w); err == nil && (n < 0 || n > 10) {
			return false
		}
	}
	return true
}

// psdSleepOK allows Start-Sleep for 1..60 seconds, the number must be literal.
func psdSleepOK(rest string) bool {
	num := false
	for _, w := range strings.Fields(rest) {
		if w == "-seconds" || w == "-s" {
			continue
		}
		n, err := strconv.Atoi(w)
		if err != nil || n < 0 || n > 60 || num {
			return false
		}
		num = true
	}
	return num
}

var (
	psdCastRe = regexp.MustCompile(`^\[(?:math|environment|int|int32|int64|long|double|single|float|decimal|string|datetime|bool|byte|char|array|uint32|uint64|timespan|regex|pscustomobject|ordered)\]\s*`)
	psdSortO  = regexp.MustCompile(`(^|\s)[/-]o`)
)

// psdHeadBad rejects the few flags of otherwise read-only tools that write or
// change something.
func psdHeadBad(head, rest string) bool {
	switch head {
	case "sort":
		return psdSortO.MatchString(rest)
	case "ipconfig":
		return strings.Contains(rest, "release") || strings.Contains(rest, "renew") ||
			strings.Contains(rest, "flushdns") || strings.Contains(rest, "registerdns") || strings.Contains(rest, "setclassid")
	case "arp":
		for _, w := range strings.Fields(rest) {
			if w == "-d" || w == "-s" || w == "/d" || w == "/s" {
				return true
			}
		}
	case "ping":
		// Only "-n N" (N<=10) and one host: no -t, -l, -w, no big counts.
		f := strings.Fields(rest)
		for i := 0; i < len(f); i++ {
			if f[i] == "-n" || f[i] == "/n" {
				if i+1 >= len(f) {
					return true
				}
				if n, err := strconv.Atoi(f[i+1]); err != nil || n < 1 || n > 10 {
					return true
				}
				i++
			} else if f[i][0] == '-' || f[i][0] == '/' {
				return true
			}
		}
	case "tasklist", "quser", "qwinsta", "systeminfo":
		for _, w := range strings.Fields(rest) {
			if w == "/s" || w == "-s" || w == "/u" || w == "/p" || w == "/server" || strings.HasPrefix(w, "/server:") {
				return true
			}
		}
	case "netstat":
		// A trailing interval repeats forever: allow only plain switches.
		for _, w := range strings.Fields(rest) {
			if len(w) < 2 || w[0] != '-' && w[0] != '/' {
				return true
			}
		}
	case "typeperf":
		// Needs a bounded sample count and no file output.
		f := strings.Fields(rest)
		bounded := false
		for i, w := range f {
			if w == "-o" || w == "/o" || w == "-cf" || w == "-config" {
				return true
			}
			if w == "-sc" && i+1 < len(f) {
				if n, err := strconv.Atoi(f[i+1]); err == nil && n >= 1 && n <= 10 {
					bounded = true
				}
			}
		}
		return !bounded
	}
	return false
}

// psdExprOK accepts an expression piece (a variable, a literal, a comparison):
// no assignment and no bare word that could be a command or a keyword.
func psdExprOK(p string) bool {
	e := psdQuoted.ReplaceAllString(p, " ")
	if strings.ContainsAny(e, "'\"=") {
		return false
	}
	e = psdUnitRe.ReplaceAllString(e, "$1 ")
	if p[0] >= '0' && p[0] <= '9' && psdNumber.MatchString(e) {
		return true
	}
	for _, loc := range psdExprWord.FindAllStringIndex(e, -1) {
		if loc[0] == 0 {
			return false
		}
		switch e[loc[0]-1] {
		case '$', '-', '.', ':', '[', '@':
		default:
			return false
		}
	}
	return true
}

// psdEnvOK allows only $env:NAME of harmless variables; the Env: drive and
// every other spelling is refused.
func psdEnvOK(low string) bool {
	for _, m := range psdEnvRe.FindAllStringSubmatchIndex(low, -1) {
		if m[0] == 0 || low[m[0]-1] != '$' || !psAllowedEnv[low[m[2]:m[3]]] {
			return false
		}
	}
	return true
}

// psdPieceOK checks one pipeline stage. used tells whether it held a command.
func psdPieceOK(piece string, hash bool) (ok, used bool) {
	p := strings.TrimSpace(piece)
	for i := 0; p != ""; i++ {
		if i > 40 {
			return false, false
		}
		if loc := psdAssign.FindStringIndex(p); loc != nil {
			p = strings.TrimSpace(p[loc[1]:])
			continue
		}
		break
	}
	if p == "" {
		return true, false
	}
	switch c := p[0]; {
	case c == '[':
		if loc := psdCastRe.FindStringIndex(p); loc != nil {
			rest := strings.TrimSpace(p[loc[1]:])
			if rest == "" || strings.HasPrefix(rest, "::") {
				return true, false
			}
			return psdPieceOK(rest, hash)
		}
		return false, false
	case c == '$' || c == '"' || c == '\'' || c == '@' || c == ',' || c == '+' || c == '-' || c == '*' || c == '!' || c == '/' || (c >= '0' && c <= '9'):
		return psdExprOK(p), false
	case c == '.' || c == '\\':
		return false, false
	}
	// "key = value" is a hashtable pair of a calculated property; outside @{}
	// it would be a call of the program named "key".
	if loc := psdPairKey.FindStringSubmatchIndex(p); loc != nil {
		if !hash || !psdHashKeys[p[loc[2]:loc[3]]] {
			return false, false
		}
		rest := strings.TrimSpace(p[loc[1]:])
		if rest == "" {
			return true, false
		}
		return psdPieceOK(rest, hash)
	}
	head := psdHeadRe.FindString(p)
	if head == "" {
		return false, false
	}
	if len(p) > len(head) && strings.ContainsRune("*?[", rune(p[len(head)])) {
		return false, true // wildcard command name: get-clip* resolves to Get-Clipboard
	}
	rest := strings.TrimSpace(p[len(head):])
	head = strings.TrimRight(head, ":")
	if psdKeywords[head] {
		// "return rm x" and "else rm x" still run a command: check what follows.
		if rest == "" {
			return true, false
		}
		return psdPieceOK(rest, hash)
	}
	switch {
	case head == "?" || head == "%" || head == "foreach" || head == "foreach-object":
		// "% Kill" would call a method by name; only the bare block form is accepted.
		return rest == "", true
	case head == "wmic":
		if psdWmicWords.MatchString(rest) {
			return false, true
		}
		for _, w := range strings.Fields(rest) {
			if strings.HasPrefix(w, "/") && !psdWmicSwitch.MatchString(w) {
				return false, true
			}
		}
		return true, true
	case head == "get-counter":
		return psdFlagsOK(rest, map[string]bool{"-counter": true, "-listset": true, "-maxsamples": true}), true
	case head == "test-connection":
		return psdFlagsOK(rest, map[string]bool{"-count": true, "-quiet": true}), true
	case head == "start-sleep" || head == "sleep":
		return psdSleepOK(rest), true
	case psdHeads[head]:
		return !psdHeadBad(head, rest), true
	case psdAllowedFull[head]:
		return true, true
	case psdCmdletVerb.MatchString(head):
		verb := head[:strings.Index(head, "-")]
		return psdAllowedVerbs[verb] && !psdDeniedFull[head], true
	}
	return false, true
}

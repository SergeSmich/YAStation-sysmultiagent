package sysadmin

import (
	"errors"
	"path"
	"regexp"
	"strings"
)

// Launching on Windows. Commands that arrive over SSH run in session 0, where
// nothing is drawn on the desktop. A GUI program is therefore started through a
// one-shot scheduled task bound to the logged-in user's interactive session.

var (
	launchAppRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,60}$`)
	// Strict allow-list: letters, digits, space and . _ - \ /. No quotes of any kind
	// (including typographic ones), commas or operators. Square brackets and
	// parentheses are kept: Start-menu folders are named like "Metro Exodus [GOG.com]"
	// and the path only ever travels inside double quotes.
	launchPathRe = regexp.MustCompile(`^[A-Za-z]:[\\/][\p{L}\p{N} ._\\/()\[\]-]{1,240}$`)
)

// safeDocExts are documents and media: opening one runs no code of its own.
var safeDocExts = map[string]bool{
	".txt": true, ".md": true, ".log": true, ".pdf": true, ".docx": true, ".doc": true, ".xlsx": true, ".xls": true,
	".pptx": true, ".odt": true, ".ods": true, ".csv": true, ".rtf": true, ".png": true, ".jpg": true, ".jpeg": true,
	".gif": true, ".bmp": true, ".webp": true, ".mp3": true, ".wav": true, ".flac": true, ".mp4": true, ".mkv": true,
	".avi": true, ".stl": true, ".gcode": true, ".3mf": true,
}

// launchBlocked are shells and script hosts: they would turn a launch into an
// arbitrary command.
var launchBlocked = map[string]bool{
	"cmd": true, "powershell": true, "pwsh": true, "wscript": true, "cscript": true, "mshta": true, "regedit": true,
	"rundll32": true, "regsvr32": true, "bash": true, "wsl": true, "sh": true, "msiexec": true, "schtasks": true,
	"reg": true, "sc": true, "net": true, "netsh": true, "certutil": true, "bitsadmin": true, "diskpart": true,
	"format": true, "python": true, "py": true, "node": true,
}

var launchBlockedExt = map[string]bool{
	".bat": true, ".cmd": true, ".vbs": true, ".vbe": true, ".js": true, ".jse": true, ".wsf": true, ".hta": true,
	".scr": true, ".msi": true, ".reg": true, ".ps1": true, ".com": true, ".pif": true, ".url": true,
	".jar": true, ".py": true, ".cpl": true, ".msc": true, ".wsh": true, ".sct": true, ".application": true,
	".appref-ms": true, ".msp": true, ".xll": true, ".docm": true, ".xlsm": true, ".pptm": true, ".psm1": true,
	".ps1xml": true, ".vb": true, ".chm": true,
}

// Launch describes what to start and whether it needs no question.
type Launch struct {
	Target  string // validated name or path, safe to place inside double quotes
	Subject string // canonical text a stored permission is keyed on
	Safe    bool   // allowed-list application or a plain document
	File    bool
}

// ParseLaunch validates a target: an application name ("notepad", "chrome") or
// an absolute local path of a file to open with its default program.
func ParseLaunch(target string) (Launch, error) {
	t := strings.TrimSpace(target)
	if t == "" || len(t) > 260 || strings.Contains(t, "..") {
		return Launch{}, errors.New("нужно имя программы или полный путь к файлу")
	}
	if launchPathRe.MatchString(t) {
		if isSensitiveText(t) {
			return Launch{}, errors.New("этот путь запускать нельзя")
		}
		slashed := strings.ReplaceAll(t, `\`, "/")
		ext := strings.ToLower(path.Ext(slashed))
		if ext == ".lnk" {
			// Only the shortcuts of installed programs from the Start menu.
			ls := strings.ToLower(slashed)
			const sm = "/microsoft/windows/start menu/programs/"
			if !(strings.HasPrefix(ls, "c:/programdata"+sm) ||
				(strings.HasPrefix(ls, "c:/users/") && strings.Contains(ls, "/appdata/roaming"+sm))) {
				return Launch{}, errors.New("ярлыки открываются только из меню «Пуск»")
			}
			name := strings.ToLower(strings.TrimSuffix(path.Base(slashed), path.Ext(slashed)))
			return Launch{Target: t, Subject: name, Safe: psAllowedApps[name], File: true}, nil
		}
		if launchBlockedExt[ext] || (ext == ".exe" && launchBlocked[strings.TrimSuffix(strings.ToLower(path.Base(slashed)), ".exe")]) {
			return Launch{}, errors.New("скрипты и установщики этим способом не запускаются")
		}
		return Launch{Target: t, Subject: strings.ToLower(t), Safe: safeDocExts[ext], File: true}, nil
	}
	if a, ok := launchAliases[strings.ToLower(t)]; ok {
		t = a
	}
	if !launchAppRe.MatchString(t) {
		return Launch{}, errors.New("нужно имя программы (латиницей) или полный путь вида C:\\папка\\файл.txt")
	}
	name := strings.ToLower(t)
	if strings.Contains(name, " ") {
		return Launch{}, errors.New("имя программы без пробелов; если не уверен в названии — найди его через поиск ярлыков")
	}
	if strings.Contains(name, ".") && !strings.HasSuffix(name, ".exe") {
		return Launch{}, errors.New("программа: имя или имя.exe; файлы открываются по полному пути")
	}
	name = strings.TrimSuffix(name, ".exe")
	if launchBlocked[name] {
		return Launch{}, errors.New("оболочки и скриптовые движки через запуск программ не открываются")
	}
	return Launch{Target: t, Subject: name, Safe: psAllowedApps[name]}, nil
}

func isSensitiveText(s string) bool {
	low := strings.ReplaceAll(strings.ToLower(s), `\`, "/")
	for _, m := range sensitiveMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// LaunchPS returns the PowerShell that starts l in the interactive session.
// `start` resolves program names the way the Start menu does (App Paths).
func (l Launch) LaunchPS() string {
	start := `start "" "` + l.Target + `"`
	if l.File {
		// Programs (games especially) expect to start in their own folder.
		if i := strings.LastIndexAny(l.Target, `\/`); i > 2 {
			start = `start "" /D "` + l.Target[:i] + `" "` + l.Target + `"`
		}
	}
	return "$u=(Get-CimInstance Win32_ComputerSystem).UserName; if(-not $u){'NO_INTERACTIVE_USER'; return}; " +
		"$n='QBL'+[guid]::NewGuid().ToString('N').Substring(0,8); " +
		"$a=New-ScheduledTaskAction -Execute 'cmd.exe' -Argument '/c " + start + "'; " +
		"$p=New-ScheduledTaskPrincipal -UserId $u -LogonType Interactive -RunLevel Limited; " +
		"$s=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries; " +
		"Register-ScheduledTask -TaskName $n -Action $a -Principal $p -Settings $s -Force | Out-Null; " +
		"Start-ScheduledTask -TaskName $n; Start-Sleep -Seconds 3; " +
		"Unregister-ScheduledTask -TaskName $n -Confirm:$false; 'LAUNCHED'"
}

var findAppRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,39}$`)

// FindAppPS returns a read-only PowerShell that lists Start menu shortcuts whose
// names contain every word of query, so a loosely spoken name can be matched.
func FindAppPS(query string) (string, error) {
	q := strings.TrimSpace(query)
	if !findAppRe.MatchString(q) {
		return "", errors.New("запрос: до 40 букв, цифр, пробелов и точек")
	}
	var cond []string
	for _, w := range strings.Fields(q) {
		cond = append(cond, "$_.BaseName -like '*"+w+"*'")
	}
	return "$d=@([Environment]::GetFolderPath('CommonPrograms'),[Environment]::GetFolderPath('Programs')); " +
		"Get-ChildItem -LiteralPath $d -Recurse -Filter *.lnk -ErrorAction SilentlyContinue | Where-Object { " + strings.Join(cond, " -and ") + " } | " +
		"Select-Object -First 8 -ExpandProperty FullName", nil
}

// launchAliases map the names people say aloud to program names.
var launchAliases = map[string]string{
	"блокнот": "notepad", "калькулятор": "calc", "хром": "chrome", "гугл хром": "chrome", "паинт": "mspaint", "пейнт": "mspaint",
	"проводник": "explorer", "диспетчер задач": "taskmgr", "фаерфокс": "firefox", "файрфокс": "firefox", "эдж": "msedge",
	"спотифай": "spotify", "стим": "steam", "дискорд": "discord", "телеграм": "telegram", "телеграмм": "telegram", "влс": "vlc",
	"ножницы": "snippingtool", "вордпад": "wordpad",
}

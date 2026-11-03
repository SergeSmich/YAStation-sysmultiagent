package sysadmin

import (
	"strings"
	"testing"
)

// ParseLaunch: имя программы, полный путь к документу, запреты и алиасы.

func TestParseLaunchAliases(t *testing.T) {
	cases := []struct {
		in      string
		target  string
		subject string
		safe    bool
	}{
		{"блокнот", "notepad", "notepad", true},
		{"Блокнот", "notepad", "notepad", true},
		{"notepad.exe", "notepad.exe", "notepad", true},
		{"хром", "chrome", "chrome", true},
		{"гугл хром", "chrome", "chrome", true},
		{"диспетчер задач", "taskmgr", "taskmgr", true},
		{"телеграмм", "telegram", "telegram", true},
		{"notepad", "notepad", "notepad", true},
		{"NOTEPAD.EXE", "NOTEPAD.EXE", "notepad", true},
		{"winword", "winword", "winword", false},
	}
	if _, err := ParseLaunch("Google Chrome"); err == nil {
		t.Fatal("имя с пробелом вне алиасов должно отклоняться")
	}
	for _, c := range cases {
		l, err := ParseLaunch(c.in)
		if err != nil {
			t.Fatalf("ParseLaunch(%q): %v", c.in, err)
		}
		if l.Target != c.target || l.Subject != c.subject || l.Safe != c.safe || l.File {
			t.Fatalf("ParseLaunch(%q)=%+v, ожидалось target=%q subject=%q safe=%v file=false",
				c.in, l, c.target, c.subject, c.safe)
		}
	}
}

func TestParseLaunchBlockedShells(t *testing.T) {
	for _, in := range []string{"cmd", "cmd.exe", "powershell", "PowerShell.exe", "pwsh", "wscript",
		"cscript", "mshta", "regedit", "rundll32", "regsvr32", "bash", "wsl", "sh", "msiexec",
		"schtasks", "reg", "sc", "net", "netsh", "certutil", "bitsadmin", "diskpart", "format",
		"python", "py", "node"} {
		if l, err := ParseLaunch(in); err == nil {
			t.Fatalf("ParseLaunch(%q) должен быть запрещён, получено %+v", in, l)
		}
	}
}

func TestParseLaunchBlockedExtensions(t *testing.T) {
	for _, in := range []string{
		`C:\Users\Admin\Desktop\script.ps1`,
		`C:\Users\Admin\Desktop\script.bat`,
		`C:\Users\Admin\Desktop\script.cmd`,
		`C:\Users\Admin\Desktop\script.vbs`,
		`C:\Users\Admin\Desktop\script.js`,
		`C:\Users\Admin\Desktop\script.hta`,
		`C:\Users\Admin\Desktop\setup.msi`,
		`C:\Users\Admin\Desktop\patch.reg`,
		`C:\Users\Admin\Desktop\app.scr`,
	} {
		if l, err := ParseLaunch(in); err == nil {
			t.Fatalf("ParseLaunch(%q) должен быть запрещён, получено %+v", in, l)
		}
	}
}

func TestParseLaunchExeIsNotSafe(t *testing.T) {
	// .exe не в списке запрещённых расширений: установщик не отклоняется, но и
	// не считается безопасным — политика спросит человека.
	l, err := ParseLaunch(`C:\Users\Admin\Downloads\setup.exe`)
	if err != nil {
		t.Fatalf("exe-путь не должен быть жёстко запрещён: %v", err)
	}
	if l.Safe || !l.File {
		t.Fatalf("%+v: ожидалось file=true safe=false", l)
	}
}

func TestParseLaunchDocuments(t *testing.T) {
	cases := []struct {
		in  string
		sub string
	}{
		{`C:\Users\Admin\Desktop\заметка.txt`, `c:\users\admin\desktop\заметка.txt`},
		{`C:\Users\Admin\Desktop\Рабочий стол\моя заметка.docx`, `c:\users\admin\desktop\рабочий стол\моя заметка.docx`},
		{`D:\Документы\отчёт за год.pdf`, `d:\документы\отчёт за год.pdf`},
		{`C:/Users/Admin/Desktop/note.md`, `c:/users/admin/desktop/note.md`},
		{`C:\Модели\деталь.3mf`, `c:\модели\деталь.3mf`},
	}
	for _, c := range cases {
		l, err := ParseLaunch(c.in)
		if err != nil {
			t.Fatalf("ParseLaunch(%q): %v", c.in, err)
		}
		if !l.Safe || !l.File {
			t.Fatalf("ParseLaunch(%q)=%+v: документ должен быть безопасным файлом", c.in, l)
		}
		if l.Target != c.in {
			t.Fatalf("Target=%q, ожидалось %q", l.Target, c.in)
		}
		if l.Subject != c.sub {
			t.Fatalf("Subject=%q, ожидалось %q", l.Subject, c.sub)
		}
		if strings.ContainsAny(l.Target, "\";`$()|&<>%*?") {
			t.Fatalf("в принятый target попал опасный символ: %q", l.Target)
		}
	}
}

func TestParseLaunchUnknownExtensionAsks(t *testing.T) {
	l, err := ParseLaunch(`C:\Users\Admin\Desktop\data.zip`)
	if err != nil {
		t.Fatalf("неизвестное расширение не запрещено: %v", err)
	}
	if l.Safe {
		t.Fatal("zip не документ: не должен считаться безопасным")
	}
	if !l.File {
		t.Fatal("ожидалось file=true")
	}
}

func TestParseLaunchShortcuts(t *testing.T) {
	if _, err := ParseLaunch(`C:\Users\Admin\Desktop\Chrome.lnk`); err == nil {
		t.Fatal("ярлык вне меню «Пуск» должен быть запрещён")
	}
	l, err := ParseLaunch(`C:\ProgramData\Microsoft\Windows\Start Menu\Programs\Google Chrome.lnk`)
	if err != nil {
		t.Fatalf("ярлык из меню «Пуск»: %v", err)
	}
	if !l.File || l.Subject != "google chrome" || l.Safe {
		t.Fatalf("%+v: ожидалось file=true subject=google chrome safe=false", l)
	}
}

func TestParseLaunchRejectsInjection(t *testing.T) {
	for _, in := range []string{
		`notepad"; calc; "`,
		`notepad; calc`,
		"notepad' ; calc ; '",
		`notepad $(calc)`,
		"notepad `calc`",
		`C:\tmp\a.txt"; calc; "`,
		`C:\tmp\a.txt` + "\x00",
		"C:\tmp\\a;b.txt",
		`C:\tmp\$env:USERPROFILE.txt`,
		"notepad|calc",
		"notepad&calc",
		"",
		"   ",
		strings.Repeat("a", 300),
		`C:\tmp\..\Windows\System32\cmd.exe`,
		"блокнот.exe", // алиас ищется по целой строке, кириллица не проходит launchAppRe
	} {
		if l, err := ParseLaunch(in); err == nil {
			t.Fatalf("ParseLaunch(%q) должен вернуть ошибку, получено %+v", in, l)
		}
	}
}

func TestParseLaunchStrictPathAllowList(t *testing.T) {
	for _, in := range []string{
		"C:\\a’,(calc),’b.txt",
		"C:\\a‘b.txt",
		"C:\\a“b”.txt",
		`C:\a,b.txt`,
		`C:\a(b).txt`,
		`C:\a[b].txt`,
		`C:\a{b}.txt`,
		`C:\a'b.txt`,
		`C:\a@b.txt`,
		`C:\a#b.txt`,
		`C:\a=b.txt`,
	} {
		if l, err := ParseLaunch(in); err == nil {
			t.Fatalf("ParseLaunch(%q) должен вернуть ошибку, получено %+v", in, l)
		}
	}
	for _, in := range []string{
		`C:\Users\Admin\Desktop\Рабочий стол\моя заметка.txt`,
		`D:\Мои документы\Отчёт 2026 года.pdf`,
	} {
		l, err := ParseLaunch(in)
		if err != nil {
			t.Fatalf("ParseLaunch(%q): %v", in, err)
		}
		if !l.File || !l.Safe || l.Target != in {
			t.Fatalf("ParseLaunch(%q)=%+v: ожидалось file=true safe=true", in, l)
		}
	}
}

func TestParseLaunchProgramNameExtensions(t *testing.T) {
	for _, in := range []string{"payload.bat", "x.ps1", "a.cmd", "run.vbs", "file.txt", "a.b.c", "calc.com"} {
		if l, err := ParseLaunch(in); err == nil {
			t.Fatalf("ParseLaunch(%q) должен вернуть ошибку, получено %+v", in, l)
		}
	}
	l, err := ParseLaunch("chrome.exe")
	if err != nil {
		t.Fatalf("chrome.exe: %v", err)
	}
	if l.Target != "chrome.exe" || l.Subject != "chrome" || !l.Safe || l.File {
		t.Fatalf("chrome.exe: %+v, ожидалось target=chrome.exe subject=chrome safe=true file=false", l)
	}
}

func TestLaunchPSQuotesTarget(t *testing.T) {
	l, err := ParseLaunch(`C:\Users\Admin\Desktop\Рабочий стол\моя заметка.txt`)
	if err != nil {
		t.Fatal(err)
	}
	ps := l.LaunchPS()
	if !strings.Contains(ps, `start "" "`+l.Target+`"`) {
		t.Fatalf("цель не подставлена в кавычках: %s", ps)
	}
	for _, must := range []string{"NO_INTERACTIVE_USER", "Register-ScheduledTask", "Unregister-ScheduledTask", "'LAUNCHED'"} {
		if !strings.Contains(ps, must) {
			t.Fatalf("в PS нет %q: %s", must, ps)
		}
	}
	// Цель заключена в двойные кавычки и не содержит ни кавычек, ни ; ни $(.
	if strings.ContainsAny(l.Target, "\";$`") {
		t.Fatalf("опасный символ в цели: %q", l.Target)
	}

	app, err := ParseLaunch("блокнот")
	if err != nil {
		t.Fatal(err)
	}
	if ps := app.LaunchPS(); !strings.Contains(ps, `start "" "notepad"`) {
		t.Fatalf("имя программы не подставлено: %s", ps)
	}
}

func TestFindAppPS(t *testing.T) {
	ps, err := FindAppPS("калькулятор")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ps, "$_.BaseName -like '*калькулятор*'") {
		t.Fatalf("нет условия поиска: %s", ps)
	}
	if !strings.Contains(ps, "Select-Object -First 8") || !strings.Contains(ps, "-Filter *.lnk") {
		t.Fatalf("поиск по ярлыкам не собран: %s", ps)
	}

	ps, err = FindAppPS("google chrome")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ps, "$_.BaseName -like '*google*' -and $_.BaseName -like '*chrome*'") {
		t.Fatalf("слова не объединены через -and: %s", ps)
	}

	ps, err = FindAppPS(" 3d ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ps, "'*3d*'") {
		t.Fatalf("цифры/пробелы: %s", ps)
	}
}

func TestFindAppPSRejectsInjection(t *testing.T) {
	for _, q := range []string{
		"",
		"   ",
		"note'pad",
		`note"pad`,
		"pad; calc",
		"$(calc)",
		"`calc`",
		"-notepad",
		"a|b",
		"a&b",
		"a<b",
		"a>b",
		"a*b",
		"a?b",
		strings.Repeat("a", 41),
		"note\npad",
	} {
		if ps, err := FindAppPS(q); err == nil {
			t.Fatalf("FindAppPS(%q) должен вернуть ошибку, получено %q", q, ps)
		}
	}
}

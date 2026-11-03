package sysadmin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Сохранённые сеансы: файл, порядок, обрезка полей.

func TestSessionsEmptyFileMissing(t *testing.T) {
	s := NewSessions(filepath.Join(t.TempDir(), "sysadmin_sessions.json"))
	if _, ok := s.Last(); ok {
		t.Fatal("пустой список не должен отдавать сеанс")
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("List=%v", got)
	}
	if s.Drop("нет-такого") {
		t.Fatal("Drop несуществующего не должен вернуть true")
	}
}

func TestSessionsSaveReloadLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sysadmin_sessions.json")
	s := NewSessions(path)
	if _, err := s.Save("тема", "краткая суть", "pc", "открытый хвост"); err != nil {
		t.Fatal(err)
	}
	sess, ok := s.Last()
	if !ok {
		t.Fatal("Last пуст после Save")
	}
	if sess.Topic != "тема" || sess.Summary != "краткая суть" || sess.Host != "pc" || sess.Open != "открытый хвост" {
		t.Fatalf("поля искажены: %+v", sess)
	}
	if len(sess.ID) != 8 {
		t.Fatalf("id=%q, ожидались 8 hex-символов", sess.ID)
	}
	if sess.Saved.IsZero() {
		t.Fatal("время сохранения не заполнено")
	}
	// Файл действительно записан (переживает «перезагрузку»).
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("временный файл остался после сохранения")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл сеансов не создан: %v", err)
	}
	// Новый объект читает тот же файл.
	r := NewSessions(path)
	got, ok := r.Last()
	if !ok || got.ID != sess.ID || got.Topic != "тема" || got.Open != "открытый хвост" {
		t.Fatalf("после перезагрузки: %+v ok=%v", got, ok)
	}
	if !got.Saved.Equal(sess.Saved) {
		t.Fatalf("время не пережило запись: %v != %v", got.Saved, sess.Saved)
	}
	if list := r.List(); len(list) != 1 || list[0].ID != sess.ID {
		t.Fatalf("List после перезагрузки: %+v", list)
	}
}

func TestSessionsLastIsNewestAndListNewestFirst(t *testing.T) {
	base := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	s := NewSessions(filepath.Join(t.TempDir(), "s.json"))
	step := 0
	s.now = func() time.Time { step++; return base.Add(time.Duration(step) * time.Minute) }

	first, err := s.Save("первая", "раз", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Save("вторая", "два", "", "")
	if err != nil {
		t.Fatal(err)
	}
	last, ok := s.Last()
	if !ok || last.ID != second.ID {
		t.Fatalf("Last=%+v, ожидалась вторая", last)
	}
	if !last.Saved.After(first.Saved) {
		t.Fatalf("время второй (%v) не позже первой (%v)", last.Saved, first.Saved)
	}
	list := s.List()
	if len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("List не «новее первым»: %+v", list)
	}
}

func TestSessionsDrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s := NewSessions(path)
	a, err := s.Save("а", "суть а", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Save("б", "суть б", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Drop(a.ID) {
		t.Fatal("Drop существующего вернул false")
	}
	if list := s.List(); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("после Drop: %+v", list)
	}
	if again := s.Drop(a.ID); again {
		t.Fatal("повторный Drop вернул true")
	}
	r := NewSessions(path)
	if list := r.List(); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("Drop не сохранён в файл: %+v", list)
	}
}

func TestSessionsClipLongFields(t *testing.T) {
	s := NewSessions(filepath.Join(t.TempDir(), "s.json"))
	sess, err := s.Save(
		"  "+strings.Repeat("т", 200)+"  ",
		strings.Repeat("с", 900),
		strings.Repeat("х", 100),
		strings.Repeat("о", 500),
	)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(sess.Topic)); n != maxSessionTopic {
		t.Fatalf("тема обрезана до %d, ожидалось %d", n, maxSessionTopic)
	}
	if strings.HasPrefix(sess.Topic, " ") || strings.HasSuffix(sess.Topic, " ") {
		t.Fatalf("пробелы по краям темы: %q", sess.Topic[:5])
	}
	if n := len([]rune(sess.Summary)); n != maxSessionSummary {
		t.Fatalf("суть обрезана до %d, ожидалось %d", n, maxSessionSummary)
	}
	if n := len([]rune(sess.Host)); n != 64 {
		t.Fatalf("хост обрезан до %d, ожидалось 64", n)
	}
	if n := len([]rune(sess.Open)); n != maxSessionOpen {
		t.Fatalf("остаток обрезан до %d, ожидалось %d", n, maxSessionOpen)
	}
}

func TestSessionsNeedsTopicAndSummary(t *testing.T) {
	s := NewSessions(filepath.Join(t.TempDir(), "s.json"))
	cases := [][2]string{
		{"", "суть"},
		{"   ", "суть"},
		{"тема", ""},
		{"тема", " \t "},
	}
	for _, c := range cases {
		if _, err := s.Save(c[0], c[1], "", ""); err == nil {
			t.Fatalf("Save(%q,%q) должен вернуть ошибку", c[0], c[1])
		}
	}
	if _, ok := s.Last(); ok {
		t.Fatal("неудачный Save не должен ничего добавлять")
	}
}

func TestSessionsKeepsAtMostFive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s := NewSessions(path)
	for i := 0; i < 7; i++ {
		if _, err := s.Save("тема "+string(rune('0'+i)), "суть", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	list := s.List()
	if len(list) != maxSessions {
		t.Fatalf("хранится %d сеансов, ожидалось %d", len(list), maxSessions)
	}
	if list[0].Topic != "тема 6" || list[maxSessions-1].Topic != "тема 2" {
		t.Fatalf("выброшен не самый старый: %+v", list)
	}
	r := NewSessions(path)
	if got := r.List(); len(got) != maxSessions || got[0].Topic != "тема 6" {
		t.Fatalf("после перезагрузки: %+v", got)
	}
}

func TestSessionsEmptyPathWorksInMemory(t *testing.T) {
	s := NewSessions("")
	sess, err := s.Save("тема", "суть", "", "")
	if err != nil {
		t.Fatalf("без файла сохранение должно работать: %v", err)
	}
	if got, ok := s.Last(); !ok || got.ID != sess.ID {
		t.Fatalf("Last=%+v ok=%v", got, ok)
	}
}

func TestSessionsBrokenFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte("{это не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSessions(path)
	if _, ok := s.Last(); ok {
		t.Fatal("битый файл должен давать пустой список, а не панику")
	}
	// После этого запись снова работает.
	if _, err := s.Save("тема", "суть", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := NewSessions(path).Last(); !ok {
		t.Fatal("файл не перезаписан после битого")
	}
}

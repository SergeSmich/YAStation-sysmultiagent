package sysadmin

import "testing"

// ParseYesNo: короткий ответ на вопрос «сохранить?».

func TestParseYesNo(t *testing.T) {
	type want struct {
		yes bool
		ok  bool
	}
	cases := []struct {
		text string
		w    want
	}{
		// Согласие.
		{"да", want{true, true}},
		{"Да!", want{true, true}},
		{"ДА", want{true, true}},
		{"да, сохрани", want{true, true}},
		{"давай", want{true, true}},
		{"ага", want{true, true}},
		{"угу", want{true, true}},
		{"конечно", want{true, true}},
		{"сохрани", want{true, true}},
		{"Сохрани, пожалуйста", want{true, true}},
		{"сохранить", want{true, true}},
		{"пожалуйста", want{false, false}},
		{"ладно", want{true, true}},
		{"хорошо", want{true, true}},
		{"можно", want{true, true}},
		{"запомни", want{true, true}},
		// Отказ.
		{"нет", want{false, true}},
		{"Нет, спасибо", want{false, true}},
		{"не надо", want{false, true}},
		{"не надо, спасибо", want{false, true}},
		{"ненадо", want{false, true}},
		{"не нужно", want{false, true}},
		{"не сохраняй", want{false, true}},
		{"без сохранения", want{false, true}},
		{"не стоит", want{false, true}},
		{"отбой", want{false, true}},
		{"неа", want{false, true}},
		// Не ответ.
		{"", want{false, false}},
		{"   ", want{false, false}},
		{",,,,", want{false, false}},
		{"не знаю", want{false, false}},
		{"какая погода", want{false, false}},
		{"привет", want{false, false}},
		{"продано", want{false, false}},
		// Разбор идёт по префиксу, поэтому фраза, начинающаяся с «нет»,
		// считается отказом (так в коде).
		{"нет ли там ошибки в журнале", want{false, false}}, // вопрос, а не ответ
		{"да один два три четыре пять шесть", want{false, false}},
		{"нет один два три четыре пять шесть", want{false, false}},
	}
	for _, c := range cases {
		yes, ok := ParseYesNo(c.text)
		if yes != c.w.yes || ok != c.w.ok {
			t.Errorf("ParseYesNo(%q)=(%v,%v), ожидалось (%v,%v)", c.text, yes, ok, c.w.yes, c.w.ok)
		}
	}
}

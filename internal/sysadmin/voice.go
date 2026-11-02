package sysadmin

import "strings"

// ParseVoiceDecision turns a spoken answer to "allow this?" into a scope.
// ok is false when the phrase is not an answer or is ambiguous (for example
// "всегда" together with "в этой сессии"); the caller then asks again.
func ParseVoiceDecision(text string) (Scope, bool) {
	t := normalizeVoice(text)
	if t == "" {
		return "", false
	}
	words := strings.Fields(t)
	prefix := func(prefixes ...string) bool {
		for _, w := range words {
			for _, p := range prefixes {
				if strings.HasPrefix(w, p) {
					return true
				}
			}
		}
		return false
	}
	exact := func(list ...string) bool {
		for _, w := range words {
			for _, p := range list {
				if w == p {
					return true
				}
			}
		}
		return false
	}
	phrase := func(list ...string) bool {
		for _, p := range list {
			if strings.Contains(t, p) {
				return true
			}
		}
		return false
	}

	if phrase("не надо", "не нужно", "не разреша", "не выполня", "не делай", "не позволя", "не подтвержда") {
		return ScopeDeny, true
	}
	// Any other negation ("не запоминай", "не всегда", "не сейчас") flips the
	// meaning of the grant words, so it is never taken as a permission.
	if exact("не", "ни", "нельзя") {
		return "", false
	}
	always := prefix("всегда", "постоянн", "навсегда", "запомн")
	session := phrase("в этой сессии", "на эту сессию", "в сессии", "на сессию", "до перезагрузки", "пока не перезагруз", "до рестарта") ||
		prefix("сессии", "сессию")
	strongOnce := phrase("только сейчас", "один раз", "только этот раз") || prefix("разово")
	weakOnce := exact("да", "ок", "окей", "ага", "угу", "сейчас", "хорошо") ||
		prefix("разреша", "разреши", "разрешу", "давай", "подтвержда", "подтверди", "подтвердить", "выполня", "согласен")
	deny := exact("нет", "неа") || prefix("отмен", "отклон", "запрет", "запрещ")

	grants := 0
	for _, b := range []bool{always, session, strongOnce} {
		if b {
			grants++
		}
	}
	switch {
	case deny && (grants > 0 || weakOnce):
		return "", false
	case deny:
		return ScopeDeny, true
	case grants > 1:
		return "", false
	case always:
		return ScopeAlways, true
	case session:
		return ScopeSession, true
	case strongOnce, weakOnce:
		return ScopeOnce, true
	}
	return "", false
}

// VoiceTarget says which of several waiting requests a spoken answer is about.
// The zero value means "not named": with one request it is that request, with
// several the caller asks the speaker to be specific.
type VoiceTarget struct {
	All   bool // «все», «оба»
	Index int  // 1-based from the oldest; -1 — the newest; 0 — not named
	Bad   bool // contradictory («все» вместе с «первую», две цифры сразу)
}

// ParseVoiceTarget finds «все / оба» or an ordinal («первую», «вторую»,
// «последнюю») in a spoken answer. It does not decide anything: the scope comes
// from ParseVoiceDecision.
func ParseVoiceTarget(text string) VoiceTarget {
	// «всё» («да, всё», «ладно, всё») is a figure of speech, not «все заявки»:
	// the ё is kept for this check.
	words := strings.Fields(normalizeVoiceKeepYo(text))
	var t VoiceTarget
	named := 0
	for i, w := range words {
		idx := 0
		switch {
		case w == "все" || w == "всех" || w == "оба" || w == "обе" || w == "обоих":
			t.All = true
			named++
			continue
		case ordinalWords[w] != 0:
			idx = ordinalWords[w]
		default:
			continue
		}
		// «последний раз», «первый раз» are not a request number.
		if i+1 < len(words) && words[i+1] == "раз" {
			continue
		}
		named++
		if t.Index != 0 && t.Index != idx {
			t.Bad = true
		}
		t.Index = idx
	}
	if t.All && t.Index != 0 || named > 1 && t.Index == 0 {
		t.Bad = true
	}
	return t
}

var ordinalWords = map[string]int{
	"первую": 1, "первый": 1, "первая": 1, "первое": 1,
	"вторую": 2, "второй": 2, "вторая": 2, "второе": 2,
	"третью": 3, "третий": 3, "третья": 3, "третье": 3,
	"четвертую": 4, "четвертый": 4, "четвертая": 4, "четвертое": 4,
	"четвёртую": 4, "четвёртый": 4, "четвёртая": 4, "четвёртое": 4,
	"пятую": 5, "пятый": 5, "пятая": 5, "пятое": 5,
	"последнюю": -1, "последний": -1, "последняя": -1, "последнее": -1,
	"новую": -1, "свежую": -1,
}

// HasVoiceVerb reports whether the phrase names the decision itself
// («разрешить», «отклонить»), not just «да/ок». A bulk decision requires it.
func HasVoiceVerb(text string) bool {
	for _, w := range strings.Fields(normalizeVoice(text)) {
		for _, p := range []string{"разреш", "подтверд", "подтвержд", "одобр", "отклон", "отмен", "запрет", "запрещ"} {
			if strings.HasPrefix(w, p) {
				return true
			}
		}
	}
	return false
}

func normalizeVoice(s string) string {
	return normalizeVoiceWith(s, true)
}

func normalizeVoiceKeepYo(s string) string {
	return normalizeVoiceWith(s, false)
}

func normalizeVoiceWith(s string, foldYo bool) string {
	s = strings.ToLower(s)
	if foldYo {
		s = strings.ReplaceAll(s, "ё", "е")
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'а' && r <= 'я', r == 'ё', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

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
		prefix("разреша", "давай", "подтвержда", "выполня", "согласен")
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

func normalizeVoice(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "ё", "е")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'а' && r <= 'я', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

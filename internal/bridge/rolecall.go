package bridge

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Spoken role names are a deliberately small grammar: a role answers only when
// the phrase starts with a calling verb or with the role name itself. A name in
// the middle of a sentence never resolves — "включи свет, helper" is a home
// command that happens to mention a role, not a call.

// The two kinds of a resolved call.
const (
	// RoleCallGreeting is a bare call: the role answers with its greeting, and
	// no model is asked anything.
	RoleCallGreeting = "call"
	// RoleCallQuestion carries the rest of the phrase for the role's own model.
	RoleCallQuestion = "question"
)

// MaxRoleAliases bounds the extra names one role may answer to.
const MaxRoleAliases = 4

// RoleCall is one phrase addressed to a role by name.
type RoleCall struct {
	Role     string `json:"role"`
	Kind     string `json:"kind"`
	Question string `json:"question,omitempty"`
}

// RoleDirEntry is one role as the resolver sees it: the canonical name and the
// aliases the operator added.
type RoleDirEntry struct {
	Role    string
	Aliases []string
}

// RoleDirectory maps every spoken form of a name to its role. It is built once
// per registry and then read-only, so a reload swaps it instead of mutating it.
type RoleDirectory struct {
	byForm map[string]string
}

func (d RoleDirectory) empty() bool { return len(d.byForm) == 0 }

// BuildRoleDirectory expands names and aliases into the forms a person may say.
// A form claimed twice keeps the first role: config validation rejects such a
// registry, so this only decides what a hand-edited file means.
func BuildRoleDirectory(entries []RoleDirEntry) RoleDirectory {
	dir := RoleDirectory{byForm: make(map[string]string)}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Role) == "" {
			continue
		}
		for _, word := range append([]string{entry.Role}, entry.Aliases...) {
			for _, form := range spokenForms(word) {
				if _, taken := dir.byForm[form]; taken {
					continue
				}
				dir.byForm[form] = entry.Role
			}
		}
	}
	return dir
}

// callVerbs start the first pattern. The preposition is part of the verb
// ("обратись к", "спроси у"), so the name after it is still the first word of
// what remains.
var callVerbs = []string{
	"обратись к", "обратись",
	"спросить у", "спроси у", "спросить", "спроси",
	"позвать", "позови", "вызови", "подзови",
}

// bareCallTails may follow a name that is called without a question. An empty
// tail is a call too: "helper" on its own is what a person says to check that a
// role is there.
var bareCallTails = []string{"ты здесь", "ты тут", "на связи", "ау", "отзовись", "привет", "здорово"}

// wakePrefixes may precede the verb: "Алиса, позови helper".
var wakePrefixes = []string{"алиса"}

// roleCaseEndings are the endings a Cyrillic name may carry when it is spoken in
// an oblique case. The stem is the name without its final vowel; nothing is
// stemmed beyond that, so a made-up role name still declines predictably.
var roleCaseEndings = []string{"а", "я", "у", "ю", "е", "и", "ой", "ей"}

// roleFinalVowels are the letters dropped before the case endings are appended.
var roleFinalVowels = []rune("аяьойеыиую")

// spokenForms lists every word shape that names one role.
func spokenForms(word string) []string {
	word = normalizeSpoken(word)
	if word == "" {
		return nil
	}
	forms := []string{word}
	if !isCyrillicWord(word) {
		// A Latin name is used as written: "urka, который час". Inventing
		// Latin case forms would match words nobody says.
		return forms
	}
	runes := []rune(word)
	stem := runes
	if last := runes[len(runes)-1]; strings.ContainsRune(string(roleFinalVowels), last) {
		stem = runes[:len(runes)-1]
	}
	if len(stem) < 3 {
		return forms
	}
	for _, ending := range roleCaseEndings {
		forms = append(forms, string(stem)+ending)
	}
	return dedupeForms(forms)
}

func dedupeForms(forms []string) []string {
	seen := make(map[string]bool, len(forms))
	out := forms[:0]
	for _, form := range forms {
		if seen[form] {
			continue
		}
		seen[form] = true
		out = append(out, form)
	}
	return out
}

// isCyrillicWord reports a name written in Cyrillic: digits, '-' and '_' are
// allowed (they are part of a valid role name) but at least one letter must be
// Cyrillic and none Latin.
func isCyrillicWord(word string) bool {
	cyrillic := false
	for _, r := range word {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic = true
		case r >= 'a' && r <= 'z':
			return false
		}
	}
	return cyrillic
}

// normalizeSpoken folds a phrase to what the resolver compares: lower case, ё as
// е (people and keyboards disagree), single spaces.
func normalizeSpoken(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(text), "ё", "е")), " ")
}

// resolve finds the role whose form starts the phrase, longest form first, and
// returns how many bytes it consumed. The form must end on a word boundary, so
// "helperka" is not "helper".
func (d RoleDirectory) resolve(phrase string) (string, int) {
	role, size := "", 0
	for form, candidate := range d.byForm {
		if len(form) <= size || !strings.HasPrefix(phrase, form) {
			continue
		}
		rest := phrase[len(form):]
		if rest != "" && !isRoleBoundary(rest) {
			continue
		}
		role, size = candidate, len(form)
	}
	return role, size
}

// isRoleBoundary reports whether what follows a name separates it from a
// continuation: a space, punctuation or the end of the phrase.
func isRoleBoundary(rest string) bool {
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(r) || strings.ContainsRune(",.!?;:—–-…", r)
}

// ParseRoleCall resolves one spoken phrase against the directory. It reports the
// role, whether the phrase is a bare call or a question, and the question text.
func ParseRoleCall(text string, dir RoleDirectory) (RoleCall, bool) {
	if dir.empty() {
		return RoleCall{}, false
	}
	phrase := normalizeSpoken(text)
	if phrase == "" {
		return RoleCall{}, false
	}
	// Pattern 1: a calling verb, optionally after the wake word.
	if body, ok := cutWakePrefix(phrase); ok {
		if call, ok := callAfterVerb(body, dir); ok {
			return call, true
		}
	}
	// Patterns 2 and 3: the name opens the phrase.
	role, size := dir.resolve(phrase)
	if role == "" {
		return RoleCall{}, false
	}
	return finishCall(role, phrase[size:])
}

// cutWakePrefix strips "алиса" and the separators after it. A phrase without the
// wake word is returned unchanged; the verb patterns do not need it.
func cutWakePrefix(phrase string) (string, bool) {
	for _, prefix := range wakePrefixes {
		if !strings.HasPrefix(phrase, prefix) {
			continue
		}
		rest := phrase[len(prefix):]
		if rest != "" && !isRoleBoundary(rest) {
			continue
		}
		trimmed := strings.TrimLeft(rest, " ,")
		if trimmed == "" {
			return "", false
		}
		return trimmed, true
	}
	return phrase, true
}

// callAfterVerb handles "<verb> <name>[ <question>]".
func callAfterVerb(phrase string, dir RoleDirectory) (RoleCall, bool) {
	body := phrase
	matched := false
	for _, verb := range callVerbs {
		if !strings.HasPrefix(phrase, verb) {
			continue
		}
		rest := phrase[len(verb):]
		if rest == "" || !isRoleBoundary(rest) {
			continue
		}
		body, matched = strings.TrimLeft(rest, " ,"), true
		break
	}
	if !matched || body == "" {
		return RoleCall{}, false
	}
	role, size := dir.resolve(body)
	if role == "" {
		return RoleCall{}, false
	}
	return finishCall(role, body[size:])
}

// finishCall classifies the tail after the name: empty or a greeting is a bare
// call, a separator followed by text is a question.
func finishCall(role, tail string) (RoleCall, bool) {
	trimmed := strings.TrimSpace(tail)
	if bareCallTail(trimmed) {
		return RoleCall{Role: role, Kind: RoleCallGreeting}, true
	}
	question, ok := cutQuestion(trimmed)
	if !ok {
		return RoleCall{}, false
	}
	return RoleCall{Role: role, Kind: RoleCallQuestion, Question: question}, true
}

// bareCallTail reports the optional acknowledgement that may follow a named
// role: "helper, ты здесь?".
func bareCallTail(tail string) bool {
	rest := strings.Trim(tail, ",.!?…—–- ")
	if rest == "" {
		return true
	}
	for _, phrase := range bareCallTails {
		if rest == phrase {
			return true
		}
	}
	return false
}

// cutQuestion accepts the third pattern only: a separator right after the name,
// then the question itself. Without a separator the phrase is a statement that
// mentions a role, not an address to it.
func cutQuestion(tail string) (string, bool) {
	if tail == "" {
		return "", false
	}
	r, size := utf8.DecodeRuneInString(tail)
	if !strings.ContainsRune(",:—–-", r) {
		return "", false
	}
	question := strings.TrimSpace(tail[size:])
	if question == "" || !utf8.ValidString(question) {
		return "", false
	}
	return question, true
}

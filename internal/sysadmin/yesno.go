package sysadmin

import "strings"

var (
	yesAnswer = map[string]bool{"да": true, "ага": true, "угу": true, "конечно": true, "давай": true, "давайте": true, "сохрани": true,
		"сохраните": true, "сохранить": true, "сохраняй": true, "сохраняйте": true, "сохранять": true, "ладно": true, "хорошо": true, "можно": true, "запомни": true}
	noAnswer = map[string]bool{"нет": true, "не": true, "ненадо": true, "отбой": true, "неа": true, "без": true}
	politeAnswer = map[string]bool{"пожалуйста": true, "ну": true, "спасибо": true, "и": true, "только": true,
		"надо": true, "нужно": true, "стоит": true, "хочу": true, "сохранения": true}
)

// ParseYesNo reads a short spoken answer to a yes/no question the bridge asked
// itself (for example, whether to save the conversation). It is an answer only
// when the whole phrase consists of answer words («да, пожалуйста», «не надо»):
// «давай откроем блокнот» is a new request, not a yes. ok is false otherwise.
func ParseYesNo(text string) (yes, ok bool) {
	words := strings.Fields(normalizeVoice(text))
	if len(words) == 0 || len(words) > 4 {
		return false, false
	}
	hasYes, hasNo := false, false
	for _, w := range words {
		switch {
		case yesAnswer[w]:
			hasYes = true
		case noAnswer[w]:
			hasNo = true
		case politeAnswer[w]:
		default:
			return false, false
		}
	}
	if hasNo {
		return false, true
	}
	if hasYes {
		return true, true
	}
	return false, false
}

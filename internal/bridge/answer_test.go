package bridge

import (
	"strings"
	"testing"
)

// The operator asked for plain answers: the prompt no longer asks the model to
// introduce itself, and a label that a hand-written role prompt or an old run
// still produces is cut before the answer is spoken and stored.
func TestAnswersAreNotLabelled(t *testing.T) {
	if strings.Contains(defaultAnswerPrompt, answerLabel) {
		t.Fatalf("answer prompt still asks for the %q prefix", answerLabel)
	}
	for _, tc := range []struct{ in, want string }{
		{"Нейросеть: Земля вращается вокруг Солнца.", "Земля вращается вокруг Солнца."},
		{"нейросеть:   ответ", "ответ"},
		{"  Нейросеть: ответ", "ответ"},
		{"Нейросеть: Нейросеть: ответ", "ответ"},
		{"Ответ без метки", "Ответ без метки"},
		{"Сети: не метка", "Сети: не метка"},
		// A model that returns only the label must not be silenced: the answer is
		// worse than useless as empty speech.
		{answerLabel, answerLabel},
		{answerLabel + "   ", answerLabel},
	} {
		if got := stripAnswerLabel(tc.in); got != tc.want {
			t.Errorf("stripAnswerLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRetryPromptIsUnlabelled keeps the second path («используй нейросеть»)
// consistent with the first: an answer triggered by the retry command is read
// aloud the same way, without a prefix.
func TestRetryPromptIsUnlabelled(t *testing.T) {
	if strings.Contains(retryAnswerPrompt, answerLabel) {
		t.Fatalf("retry prompt still asks for the %q prefix", answerLabel)
	}
}

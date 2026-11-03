package bridge

import "quasarbridge/internal/llm"

// llmFailureAnswer is what the speaker says when the model gave no answer. A
// used-up limit is named: waiting helps there, a log hunt does not.
func llmFailureAnswer(err error) string {
	if llm.IsRateLimited(err) {
		return "У нейросети закончился лимит запросов. Повторите через минуту."
	}
	return "Сейчас не получилось получить ответ нейросети."
}

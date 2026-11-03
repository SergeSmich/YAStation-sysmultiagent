package main

import (
	"context"
	stdjson "encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"quasarbridge/internal/llm"
)

// The role test asks a role's model a fixed human question and explains the
// outcome in plain words. Unlike «Проверить» of a connection it uses the pair
// the role would really use (the connection plus the model chosen in the role
// card, saved or not). Per-role overrides of address and format are not part of
// the card, so they are not part of the test either.
const (
	panelLLMAskTimeout = 30 * time.Second
	panelLLMAskTokens  = 512
	panelLLMAskSlow    = 15 * time.Second
	panelLLMAskSystem  = "Ты голосовой помощник. Отвечай по-русски, одной короткой фразой, без списков."
	panelLLMAskPrompt  = "Сколько будет дважды два? Ответь словом."
)

// explainLLMError turns a client error into advice. llm.Client errors carry a
// status code or a short cause and never the key, so the text is safe to show.
func explainLLMError(err error) string {
	advice := adviceForLLMError(err)
	detail := llm.ErrorDetail(err)
	if detail == "" {
		return advice
	}
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "opencode") || strings.Contains(lower, "claude code") || strings.Contains(lower, "client_restricted") {
		return "сервис пускает этот ключ только в свои программы (Claude Code, OpenCode, Codex), а мост для него «чужой клиент». Для станции такой ключ не подойдёт: выберите другую нейросеть. Сервис пишет: «" + detail + "»."
	}
	return advice + " Сервис пишет: «" + detail + "»."
}

func adviceForLLMError(err error) string {
	text := err.Error()
	lower := strings.ToLower(text)
	if match := llmHTTPStatus.FindStringSubmatch(text); match != nil {
		switch match[1] {
		case "401", "403":
			return "сервис не принял ключ. Проверьте, что ключ вставлен целиком и не просрочен: вставьте его заново и нажмите «Применить»."
		case "404":
			return "сервис не нашёл такой модели или такого адреса. Выберите модель из списка и проверьте адрес API нейросети."
		case "429":
			return "сервис отказал из-за лимита запросов: бесплатная квота кончилась или запросов слишком много. Подождите немного или выберите другую модель."
		case "400", "422":
			return "сервис отклонил запрос. Чаще всего модель не подходит для разговора или её имя написано неверно — выберите другую модель из списка. Если и другая модель отклоняется, проверьте ключ."
		case "402":
			return "на счёте нейросети нет денег. Пополните счёт или выберите бесплатную модель."
		case "408", "504":
			return "сервис долго не отвечал. Попробуйте ещё раз позже или возьмите модель побыстрее."
		}
		if strings.HasPrefix(match[1], "5") {
			return "у самого сервиса нейросети сбой или он перегружен. Это не ваша ошибка: попробуйте через несколько минут."
		}
		return "сервис ответил неожиданным отказом (код " + match[1] + "). Проверьте адрес API и модель."
	}
	switch {
	case strings.Contains(lower, "deadline exceeded"), strings.Contains(lower, "timeout"):
		return "нейросеть не ответила вовремя. Для голоса такая модель слишком медленная: выберите побыстрее."
	case strings.Contains(lower, "unreachable"), strings.Contains(lower, "transport"):
		return "до сервиса не получилось достучаться. Проверьте адрес API и интернет на станции."
	case strings.Contains(lower, "token budget"):
		return "модель слишком долго «думала» и не успела дать ответ. Для голоса возьмите модель без длинных рассуждений."
	case strings.Contains(lower, "no answer"), strings.Contains(lower, "service markup"):
		return "модель вернула пустой или служебный ответ. Для роли она не годится: выберите другую."
	case strings.Contains(lower, "authorization key is empty"):
		return "ключ не задан. Вставьте ключ у нейросети и нажмите «Применить»."
	case strings.Contains(lower, "not a model list"), strings.Contains(lower, "no models"):
		return "сервис ответил, но списка моделей в ответе нет. Впишите модель вручную."
	case strings.Contains(lower, "too large"), strings.Contains(lower, "exceeded size"):
		return "сервис прислал слишком большой ответ. Проверьте адрес API: похоже, это не тот адрес."
	case strings.Contains(lower, "endpoint"):
		return "адрес API записан неверно. Проверьте адрес нейросети: он должен начинаться с https://."
	case strings.Contains(lower, "invalid"), strings.Contains(lower, "cannot read"), strings.Contains(lower, "cannot encode"):
		return "сервис ответил в непонятном виде. Проверьте адрес API и «Формат API» нейросети."
	}
	return "не получилось получить ответ (" + text + "). Проверьте адрес, ключ и модель."
}

var llmHTTPStatus = regexp.MustCompile(`HTTP (\d{3})`)

// apiLLMAsk is the «Проверить модель» button of a role card. The request names a
// saved connection (or none: the station default) and a model; an empty model
// means the one of the connection.
func (p *panelServer) apiLLMAsk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	decoder := stdjson.NewDecoder(http.MaxBytesReader(w, r.Body, panelLLMBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		p.audit.record("llm_ask", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "запрос не разобран: обновите страницу (Ctrl+F5)"})
		return
	}
	cfg, err := readConfig(p.configPath)
	if err != nil {
		p.audit.record("llm_ask", r, "failed", nil)
		writePanelJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось прочитать настройки"})
		return
	}
	target, err := cfg.resolveLLM(agentConfig{
		Provider: normalizeProviderName(body.Provider),
		Model:    strings.TrimSpace(body.Model),
	})
	if err != nil {
		p.audit.record("llm_ask", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "нейросеть не сохранена: сначала сохраните её"})
		return
	}
	if target.BaseURL == "" || target.Model == "" {
		p.audit.record("llm_ask", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{
			"error": "не задана модель: выберите её в карточке или у нейросети",
		})
		return
	}
	stored := map[string]string{}
	if p.settings.Secrets != nil {
		if file, loadErr := p.settings.Secrets.load(); loadErr == nil {
			stored = file.Providers
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelLLMAskTimeout)
	defer cancel()
	// The test asks with the same chat tuning a role would use, so a parameter
	// the service refuses shows up here and not on the first real question.
	var chatParameters map[string]any
	if target.API == "chat_completions" {
		chatParameters = cfg.LLMChatParameters
		if target.ChatParameters != nil {
			chatParameters = target.ChatParameters
		}
	}
	client := &llm.Client{
		BaseURL: target.BaseURL, Model: target.Model, APIKey: apiKey(stored, target),
		API: target.API, ReasoningEffort: target.ReasoningEffort,
		MaxOutputTokens: panelLLMAskTokens,
		ChatParameters:  chatParameters,
		HTTP:            &http.Client{Timeout: panelLLMAskTimeout},
	}
	start := time.Now()
	reply, chatErr := client.Complete(ctx, []llm.Message{
		{Role: "system", Content: panelLLMAskSystem},
		{Role: "user", Content: panelLLMAskPrompt},
	}, false)
	elapsed := time.Since(start)
	answer := map[string]any{
		"provider": target.Provider, "model": target.Model,
		"latency_ms": elapsed.Milliseconds(), "question": panelLLMAskPrompt,
	}
	audit := map[string]any{"provider": target.Provider, "latency_ms": elapsed.Milliseconds()}
	if chatErr != nil {
		answer["ok"] = false
		answer["verdict"] = explainLLMError(chatErr)
		answer["error"] = chatErr.Error()
		p.audit.record("llm_ask", r, "failed", audit)
		writePanelJSON(w, http.StatusOK, answer)
		return
	}
	sample := panelLLMSample(reply)
	lower := strings.ToLower(sample)
	answer["ok"] = true
	answer["sample"] = sample
	switch {
	case !strings.Contains(lower, "четыре") && !strings.Contains(lower, "4"):
		answer["ok"] = false
		answer["verdict"] = "модель ответила, но не то, что ждали (правильный ответ — «четыре»). Для роли она ненадёжна: выберите другую."
	case elapsed > panelLLMAskSlow:
		answer["verdict"] = "модель отвечает правильно, но медленно: колонка ждёт ответ не дольше 15 секунд. Для голоса возьмите модель побыстрее."
	default:
		answer["verdict"] = "всё хорошо: модель отвечает правильно и достаточно быстро."
	}
	result := "ok"
	if answer["ok"] != true {
		result = "failed"
	}
	p.audit.record("llm_ask", r, result, audit)
	writePanelJSON(w, http.StatusOK, answer)
}

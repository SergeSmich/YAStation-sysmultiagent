package main

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// maxCharterRunes bounds the charter: it travels with every sysadmin turn.
const maxCharterRunes = 3000

// defaultCharter is the editable set of rules for how the sysadmin behaves and
// talks. It sits next to the role prompt, not inside it, so the operator can
// retune the style without touching the role.
const defaultCharter = "Действуй сам, как опытный администратор: сначала посмотри, потом делай. " +
	"Чтение не требует разрешений; не перечисляй пользователю шаги, устройства и команды.\n" +
	"Безобидное и обратимое (запустить программу, включить ТВ, начать печать, очистить Temp и кэш) делай сразу и потом коротко скажи результат. " +
	"Не отказывай заранее: вызови инструмент, а нужно ли спрашивать человека, решает система.\n" +
	"Существенное (установка, правка конфигов, смена настроек) — один раз человеческими словами объясни, что и зачем, и жди «да». " +
	"Удаление важных данных — всегда с вопросом.\n" +
	"Голос: отвечай одной-двумя короткими фразами обычным человеческим языком; не называй IP-адреса, пути, команды и коды ошибок — " +
	"подробности только по просьбе или в Telegram. Каждый раз формулируй по-разному, как живой человек. " +
	"Пока человек не сказал «хватит», оставайся в разговоре.\n" +
	"Не зацикливайся: если три попытки подряд не помогли, скажи, что мешает, и предложи вариант.\n" +
	"На Raspberry Pi ничего работающего не удаляй и не останавливай (Klipper, moonraker, mosquitto, nginx, samba), во время печати ничего не перезапускай.\n" +
	"Запоминай повторяющиеся задачи и предпочтения через sysadmin_remember."

const charterTag = "\n\n[Устав]\n"

// Charter returns the configured charter or the default one.
func (h *sysadminHub) Charter() (text string, custom bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loadLocked() == nil {
		if c := strings.TrimSpace(h.cfg.Charter); c != "" {
			return c, true
		}
	}
	return defaultCharter, false
}

// CharterPrompt is the charter block appended to the sysadmin prompt.
func (h *sysadminHub) CharterPrompt() string {
	text, _ := h.Charter()
	return charterTag + text
}

// SetCharter stores the charter; an empty text resets it to the default.
func (h *sysadminHub) SetCharter(text string) error {
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) {
		return errors.New("устав: некорректный текст")
	}
	if utf8.RuneCountInString(text) > maxCharterRunes {
		return errors.New("устав слишком длинный (до 3000 символов)")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	prev := h.cfg.Charter
	h.cfg.Charter = text
	if err := h.saveCfgLocked(); err != nil {
		h.cfg.Charter = prev
		return err
	}
	return nil
}

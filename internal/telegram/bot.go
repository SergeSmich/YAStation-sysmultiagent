package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"quasarbridge/internal/sysadmin"
)

type Config struct {
	Token       string
	Users       []int64
	BaseURL     string
	Client      *http.Client
	PollTimeout time.Duration
	AskTimeout  time.Duration
}

type Asker interface {
	Ask(ctx context.Context, chatID int64, text string) (string, error)
}

// Followuper lets the agent report the outcome of an approved action in its own
// words. An Asker that implements it gets the result instead of the raw text.
type Followuper interface {
	Followup(ctx context.Context, chatID int64, it sysadmin.Item) (string, error)
}

type Approvals interface {
	Pending() []sysadmin.Item
	DecideScope(id string, scope sysadmin.Scope, by string) (sysadmin.Item, error)
	Result(id string) (sysadmin.Item, bool)
}

// approvalKeyboard builds the buttons of a request card. Only requests that
// can be remembered offer the session and always choices.
func approvalKeyboard(it sysadmin.Item) map[string]any {
	var rows [][]map[string]string
	if it.Remember {
		rows = [][]map[string]string{
			{
				{"text": "✅ Только сейчас", "callback_data": "a:" + it.ID},
				{"text": "🕘 В сессии", "callback_data": "s:" + it.ID},
			},
			{
				{"text": "♾ Всегда", "callback_data": "w:" + it.ID},
				{"text": "⛔ Отклонить", "callback_data": "d:" + it.ID},
			},
		}
	} else {
		rows = [][]map[string]string{{
			{"text": "✅ Разрешить", "callback_data": "a:" + it.ID},
			{"text": "⛔ Отклонить", "callback_data": "d:" + it.ID},
		}}
	}
	return map[string]any{"inline_keyboard": rows}
}

var callbackScopes = map[string]sysadmin.Scope{
	"a": sysadmin.ScopeOnce, "s": sysadmin.ScopeSession,
	"w": sysadmin.ScopeAlways, "d": sysadmin.ScopeDeny,
}

var scopeLabels = map[sysadmin.Scope]string{
	sysadmin.ScopeOnce: "Разрешено один раз", sysadmin.ScopeSession: "Разрешено до перезагрузки моста",
	sysadmin.ScopeAlways: "Разрешено всегда", sysadmin.ScopeDeny: "Отклонено",
}

type Bot struct {
	cfg     Config
	asker   Asker
	ap      Approvals
	client  *http.Client
	baseURL string
	users   map[int64]struct{}
	mu      sync.Mutex
	busy    map[int64]bool
}

func New(cfg Config, asker Asker, ap Approvals) *Bot {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.telegram.org"
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 25 * time.Second
	}
	if cfg.AskTimeout <= 0 {
		cfg.AskTimeout = 3 * time.Minute
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 60 * time.Second}
	}
	b := &Bot{
		cfg:     cfg,
		asker:   asker,
		ap:      ap,
		client:  cfg.Client,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		users:   map[int64]struct{}{},
		busy:    map[int64]bool{},
	}
	for _, u := range cfg.Users {
		b.users[u] = struct{}{}
	}
	return b
}

func (b *Bot) isUser(id int64) bool {
	_, ok := b.users[id]
	return ok
}

func (b *Bot) apiURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", b.baseURL, b.cfg.Token, method)
}

func (b *Bot) redact(err error) error {
	if err == nil {
		return nil
	}
	s := strings.ReplaceAll(err.Error(), b.cfg.Token, "***")
	return fmt.Errorf("%s", s)
}

type update struct {
	UpdateID      int64           `json:"update_id"`
	Message       *tgMessage      `json:"message"`
	CallbackQuery *callbackQuery  `json:"callback_query"`
}

type tgMessage struct {
	MessageID int64  `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Text string `json:"text"`
}

type callbackQuery struct {
	ID      string `json:"id"`
	From    struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	Data string `json:"data"`
}

func (b *Bot) do(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", b.apiURL(method), bytes.NewReader(body))
	if err != nil {
		return nil, b.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, b.redact(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Desc   string          `json:"description"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, b.redact(err)
	}
	if !r.OK {
		return nil, fmt.Errorf("telegram: %s", r.Desc)
	}
	return r.Result, nil
}

func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		params := map[string]any{
			"offset":  offset,
			"timeout": int(b.cfg.PollTimeout.Seconds()),
		}
		raw, err := b.do(ctx, "getUpdates", params)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("telegram getUpdates error", "err", b.redact(err))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = time.Second
		var ups []update
		if err := json.Unmarshal(raw, &ups); err != nil {
			continue
		}
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			b.handleUpdate(ctx, u)
		}
	}
}

func (b *Bot) handleUpdate(ctx context.Context, u update) {
	if len(b.users) == 0 {
		return
	}
	if u.CallbackQuery != nil {
		cq := u.CallbackQuery
		if !b.isUser(cq.From.ID) {
			slog.Warn("telegram unauthorized callback", "user_id", cq.From.ID)
			return
		}
		b.handleCallback(ctx, cq)
		return
	}
	if u.Message != nil {
		m := u.Message
		fromID := m.From.ID
		if !b.isUser(fromID) {
			slog.Warn("telegram unauthorized message", "user_id", fromID)
			return
		}
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return
		}
		if strings.HasPrefix(text, "/") {
			b.handleCommand(ctx, m.Chat.ID, fromID, text)
			return
		}
		b.handleText(ctx, m.Chat.ID, fromID, text)
	}
}

func (b *Bot) handleCommand(ctx context.Context, chatID, userID int64, text string) {
	cmd := strings.Fields(text)[0]
	cmd = strings.SplitN(cmd, "@", 2)[0]
	switch cmd {
	case "/start", "/help":
		b.sendChunked(ctx, chatID, "Quasar bridge bot.\nКоманды: /pending, /status, /help")
	case "/pending":
		items := b.ap.Pending()
		if len(items) == 0 {
			b.sendChunked(ctx, chatID, "Нет ожидающих запросов.")
			return
		}
		for _, it := range items {
			card := fmt.Sprintf("%s | %s | %s | %s", it.Host, it.Tool, it.Summary, it.Origin)
			b.sendMessage(ctx, chatID, card, approvalKeyboard(it))
		}
	case "/status":
		b.sendChunked(ctx, chatID, "ok")
	}
}

func (b *Bot) handleText(ctx context.Context, chatID, userID int64, text string) {
	b.mu.Lock()
	if b.busy[chatID] {
		b.mu.Unlock()
		b.sendMessage(ctx, chatID, "занят, дождитесь ответа", nil)
		return
	}
	b.busy[chatID] = true
	b.mu.Unlock()

	b.sendChatAction(ctx, chatID, "typing")

	go func() {
		defer func() {
			b.mu.Lock()
			delete(b.busy, chatID)
			b.mu.Unlock()
		}()
		cctx, cancel := context.WithTimeout(ctx, b.cfg.AskTimeout)
		defer cancel()
		res, err := b.asker.Ask(cctx, chatID, text)
		if err != nil {
			msg := err.Error()
			if len([]rune(msg)) > 300 {
				msg = string([]rune(msg)[:300])
			}
			b.sendChunked(ctx, chatID, "Ошибка: "+msg)
			return
		}
		b.sendChunked(ctx, chatID, res)
	}()
}

func (b *Bot) handleCallback(ctx context.Context, cq *callbackQuery) {
	kind, id, found := strings.Cut(cq.Data, ":")
	scope, known := callbackScopes[kind]
	if !found || !known || id == "" {
		return
	}
	allow := scope != sysadmin.ScopeDeny
	decided, err := b.ap.DecideScope(id, scope, fmt.Sprintf("telegram:%d", cq.From.ID))
	if err != nil {
		msg := err.Error()
		b.answerCallback(ctx, cq.ID, "Ошибка: "+msg)
		return
	}
	resText := scopeLabels[scope]
	if decided.Note != "" {
		resText = "Выполнено один раз: " + decided.Note
	}
	b.answerCallback(ctx, cq.ID, resText)
	// edit markup remove buttons / add mark
	markup := map[string]any{"inline_keyboard": [][]map[string]string{}}
	text := resText
	b.editMessageReplyMarkup(ctx, cq.Message.Chat.ID, cq.Message.MessageID, markup, text)

	if allow {
		go func(id string, chatID int64) {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			timeout := time.After(60 * time.Second)
			for {
				select {
				case <-ctx.Done():
					return
				case <-timeout:
					return
				case <-ticker.C:
				}
				it, ok := b.ap.Result(id)
				if !ok {
					return
				}
				if it.Status == "running" || it.Status == "approved" || it.Status == "pending" {
					continue
				}
				// done/failed
				if fu, ok := b.asker.(Followuper); ok && it.ChatID == chatID {
					fctx, cancel := context.WithTimeout(ctx, b.cfg.AskTimeout)
					res, ferr := fu.Followup(fctx, chatID, it)
					cancel()
					if ferr == nil && strings.TrimSpace(res) != "" {
						b.sendChunked(ctx, chatID, res)
						return
					}
					slog.Warn("telegram followup failed", "err", ferr)
				}
				out := it.Result
				if it.Err != "" {
					out = "Ошибка: " + it.Err
				}
				if len([]rune(out)) > 3500 {
					out = string([]rune(out)[:3500])
				}
				b.sendMessage(ctx, chatID, out, nil)
				return
			}
		}(id, cq.Message.Chat.ID)
	}
}

func (b *Bot) sendChatAction(ctx context.Context, chatID int64, action string) {
	_, _ = b.do(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action})
}

func (b *Bot) sendMessage(ctx context.Context, chatID int64, text string, markup any) {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	_, _ = b.do(ctx, "sendMessage", payload)
}

func (b *Bot) answerCallback(ctx context.Context, id, text string) {
	_, _ = b.do(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text})
}

func (b *Bot) editMessageReplyMarkup(ctx context.Context, chatID int64, msgID int64, markup any, _ string) {
	_, _ = b.do(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": chatID, "message_id": msgID, "reply_markup": markup})
}

func (b *Bot) sendChunked(ctx context.Context, chatID int64, text string) {
	chunks := splitChunks(text, 4000)
	for _, c := range chunks {
		b.sendMessage(ctx, chatID, c, nil)
	}
}

func splitChunks(s string, max int) []string {
	if s == "" {
		return []string{""}
	}
	runes := []rune(s)
	var out []string
	_ = s
	// simple: split by lines
	lines := strings.Split(s, "\n")
	var buf strings.Builder
	for _, l := range lines {
		if buf.Len() > 0 && len([]rune(buf.String()+"\n"+l)) > max {
			out = append(out, buf.String())
			buf.Reset()
		}
		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(l)
		if len([]rune(buf.String())) > max {
			// force split
			r := []rune(buf.String())
			for len(r) > max {
				out = append(out, string(r[:max]))
				r = r[max:]
			}
			buf.Reset()
			buf.WriteString(string(r))
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	// ensure no chunk > max
	_ = runes
	return out
}

func (b *Bot) Notify(it sysadmin.Item) {
	card := fmt.Sprintf("Хост: %s\nИнструмент: %s\nSummary: %s\nИсточник: %s", it.Host, it.Tool, it.Summary, it.Origin)
	kb := approvalKeyboard(it)
	for u := range b.users {
		b.sendMessage(context.Background(), u, card, kb)
	}
}

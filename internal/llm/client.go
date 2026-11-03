package llm

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Preserve reasoning and other Responses items only for this tool loop.
	// They are deliberately absent from the Chat Completions wire format.
	responseOutput []jsontext.Value
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Function struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
type Client struct {
	BaseURL         string
	Model           string
	APIKey          string
	API             string
	ReasoningEffort string
	MaxOutputTokens int
	HostedTools     []string
	ChatParameters  map[string]any
	HTTP            *http.Client
}

var serviceMarkup = regexp.MustCompile(`(?i)(?:</?[|｜]|<\s*/?\s*(?:[\w.-]+:)?(?:(?:tool_calls?|function_calls?|tool_responses?|tool_results?|think(?:ing)?|analysis|reasoning(?:_content)?)\b|(?:function|parameter|invoke)(?:\s*=|\s+|\s*>|$)))`)

// ValidateAnswer rejects service markup rather than speaking unexecuted calls
// or internal reasoning. Structured planning bypasses this speech check.
func ValidateAnswer(text string) error {
	if serviceMarkup.MatchString(text) {
		return errors.New("LLM returned service markup instead of a final answer")
	}
	return nil
}

// ValidateChatParameters protects the request fields controlled by the bridge.
// Parameter values are validated by the configured API.
func ValidateChatParameters(parameters map[string]any) error {
	for key := range parameters {
		switch key {
		case "temperature", "max_tokens", "max_completion_tokens", "response_format", "chat_template_kwargs", "reasoning_effort":
		default:
			return errors.New("unsupported LLM chat parameter")
		}
	}
	return nil
}

func (c *Client) Complete(ctx context.Context, messages []Message, structured bool) (string, error) {
	m, err := c.chat(ctx, messages, structured, nil)
	if err != nil {
		return "", err
	}
	if len(m.ToolCalls) > 0 {
		return "", errors.New("unexpected tool call in plain completion")
	}
	return strings.TrimSpace(m.Content), nil
}
func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	return c.chat(ctx, messages, false, tools)
}
func (c *Client) chat(ctx context.Context, messages []Message, structured bool, tools []Tool) (Message, error) {
	if err := ValidateChatParameters(c.ChatParameters); err != nil {
		return Message{}, err
	}
	var m Message
	var err error
	switch c.API {
	case "", "chat_completions", APIGigaChat:
		m, err = c.chatCompletions(ctx, messages, structured, tools)
	case "responses":
		if len(c.ChatParameters) > 0 {
			return Message{}, errors.New("LLM chat parameters require chat_completions API")
		}
		m, err = c.responses(ctx, messages, structured, tools)
	default:
		return Message{}, errors.New("unsupported LLM API")
	}
	if err != nil {
		return Message{}, err
	}
	if !structured && len(m.ToolCalls) == 0 {
		if err := ValidateAnswer(m.Content); err != nil {
			return Message{}, err
		}
	}
	return m, nil
}

func (c *Client) chatCompletions(ctx context.Context, messages []Message, structured bool, tools []Tool) (Message, error) {
	request := map[string]any{
		"model": c.Model, "messages": messages, "stream": false,
		"temperature": 0, "max_tokens": 1024,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	// Google, Groq, OpenAI and similar endpoints reject unknown fields with
	// HTTP 400, and chat_template_kwargs is a vLLM-style extension.
	if !acceptsTemplateKwargs(c.BaseURL) {
		delete(request, "chat_template_kwargs")
	}
	if c.API == APIGigaChat {
		delete(request, "chat_template_kwargs")
	}
	if structured {
		request["response_format"] = map[string]string{"type": "json_object"}
	}
	for key, value := range c.ChatParameters {
		if key == "response_format" && !structured {
			continue
		}
		if value == nil {
			delete(request, key)
		} else {
			request[key] = value
		}
	}
	if len(tools) > 0 {
		request["tools"] = tools
		request["tool_choice"] = "auto"
		if c.API != APIGigaChat {
			request["parallel_tool_calls"] = false
		}
	}
	started := time.Now()
	data, err := c.post(ctx, "/chat/completions", request)
	if err != nil {
		logCall(c, started, 0, 0, err)
		return Message{}, err
	}
	var result struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		logCall(c, started, 0, 0, err)
		return Message{}, errors.New("invalid LLM response")
	}
	logCall(c, started, result.Usage.PromptTokens, result.Usage.CompletionTokens, nil, result.Usage.PromptTokensDetails.CachedTokens)
	if len(result.Choices) == 0 || (strings.TrimSpace(result.Choices[0].Message.Content) == "" && len(result.Choices[0].Message.ToolCalls) == 0) {
		return Message{}, errors.New("LLM returned no answer")
	}
	if result.Choices[0].FinishReason == "length" {
		return Message{}, errors.New("LLM answer exceeded token budget")
	}
	m := result.Choices[0].Message
	m.Role = "assistant"
	return m, nil
}

// logCall writes one line per model round: where the time and the tokens of a
// slow answer went. Host and model only; the URL may carry userinfo and the
// key never reaches a log.
//
// cachedTokens is the part of the prompt the service served from its prefix
// cache; it is logged only when the service reports one.
func logCall(c *Client, started time.Time, promptTokens, completionTokens int, err error, cachedTokens ...int) {
	attrs := []any{"host", hostOf(c.BaseURL), "model", c.Model, "elapsed_ms", time.Since(started).Milliseconds(),
		"prompt_tokens", promptTokens, "completion_tokens", completionTokens}
	if len(cachedTokens) > 0 && cachedTokens[0] > 0 {
		attrs = append(attrs, "cached_tokens", cachedTokens[0])
	}
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			attrs = append(attrs, "status", httpErr.Status)
			if httpErr.RetryAfter > 0 {
				attrs = append(attrs, "retry_after_ms", httpErr.RetryAfter.Milliseconds())
			}
			if httpErr.Detail != "" {
				// Already cut to the error message with keys and ids masked.
				attrs = append(attrs, "detail", httpErr.Detail)
			}
		}
		attrs = append(attrs, "error", err.Error())
		slog.Warn("llm_call_failed", attrs...)
		return
	}
	slog.Info("llm_call", attrs...)
}

// maxRetryWait is the longest pause one retry after HTTP 429 may take: a voice
// answer cannot wait longer than the cloud does.
const maxRetryWait = 6 * time.Second

// retrySleep waits for d or until ctx ends; a variable so tests do not sleep.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// post sends the request and, when the service says "too many requests, try
// again in a moment", waits that moment once and repeats it.
func (c *Client) post(ctx context.Context, path string, request any) ([]byte, error) {
	data, err := c.postOnce(ctx, path, request)
	var httpErr *HTTPError
	if err == nil || !errors.As(err, &httpErr) || httpErr.Status != http.StatusTooManyRequests {
		return data, err
	}
	wait := httpErr.RetryAfter
	if wait <= 0 || wait > maxRetryWait {
		return data, err
	}
	wait += 100 * time.Millisecond
	slog.Warn("llm_rate_limited_retry", "host", hostOf(c.BaseURL), "model", c.Model,
		"wait_ms", wait.Milliseconds(), "detail", httpErr.Detail)
	if serr := retrySleep(ctx, wait); serr != nil {
		return nil, fmt.Errorf("LLM transport: %w", serr)
	}
	return c.postOnce(ctx, path, request)
}

func hostOf(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return "?"
}

func (c *Client) postOnce(ctx context.Context, path string, request any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("cannot encode LLM request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid LLM endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if c.API == APIGigaChat {
		token, err := GigaAccessToken(ctx, c.APIKey)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		hc = GigaHTTPClient(60 * time.Second)
	} else if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("LLM transport: %w", ctx.Err())
		}
		return nil, errors.New("LLM transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, newHTTPError(resp.StatusCode, detail, c.APIKey, resp.Header.Get("Retry-After"))
	}
	const maxResponseBytes = 2 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("LLM transport: %w", ctx.Err())
		}
		return nil, errors.New("cannot read LLM response")
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("LLM response exceeded size limit")
	}
	return data, nil
}

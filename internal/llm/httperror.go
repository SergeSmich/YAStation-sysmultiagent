package llm

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// HTTPError is a non-200 answer of the LLM service. Error() stays short and
// free of upstream text (it reaches logs); Detail keeps the service's own
// explanation, already cleaned, for the operator panel only.
type HTTPError struct {
	Status int
	Detail string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("LLM returned HTTP %d", e.Status) }

// ErrorDetail returns the service's explanation of a failed request, or "".
func ErrorDetail(err error) string {
	var h *HTTPError
	if errors.As(err, &h) {
		return h.Detail
	}
	return ""
}

const maxDetailRunes = 240

var tokenLike = regexp.MustCompile(`[A-Za-z0-9_\-]{32,}`)

// newHTTPError cleans the response body: only the error message is kept, the
// request key and anything shaped like a token are masked, the text is short.
func newHTTPError(status int, body []byte, key string) *HTTPError {
	return &HTTPError{Status: status, Detail: errorDetail(body, key)}
}

func errorDetail(body []byte, key string) string {
	var parsed struct {
		Error   jsontext.Value `json:"error"`
		Message string         `json:"message"`
	}
	text := ""
	if json.Unmarshal(body, &parsed) == nil {
		text = parsed.Message
		if len(parsed.Error) > 0 {
			var obj struct {
				Message string `json:"message"`
			}
			var plain string
			if json.Unmarshal(parsed.Error, &obj) == nil && obj.Message != "" {
				text = obj.Message
			} else if json.Unmarshal(parsed.Error, &plain) == nil && plain != "" {
				text = plain
			}
		}
	} else if t := strings.TrimSpace(string(body)); t != "" && !strings.HasPrefix(t, "<") {
		text = t
	}
	if !utf8.ValidString(text) {
		return ""
	}
	if key != "" {
		text = strings.ReplaceAll(text, key, "***")
	}
	text = tokenLike.ReplaceAllString(text, "***")
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > maxDetailRunes {
		text = string(runes[:maxDetailRunes]) + "…"
	}
	return text
}

// strictHosts reject request fields they do not know with HTTP 400.
var strictHosts = []string{
	"api.groq.com",
	"generativelanguage.googleapis.com",
	"api.openai.com",
	"api.mistral.ai",
	"api.cerebras.ai",
	"api.arena.ai",
}

// acceptsTemplateKwargs reports whether chat_template_kwargs (a vLLM extension)
// may be sent to this endpoint.
func acceptsTemplateKwargs(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, strict := range strictHosts {
		if host == strict {
			return false
		}
	}
	return true
}

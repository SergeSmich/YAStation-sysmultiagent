package llm

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPErrorKeepsShortMessageAndHidesDetail(t *testing.T) {
	key := "sk-supersecretkey-1234567890"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":"property 'chat_template_kwargs' is unsupported; key %s"}}`, key)
	}))
	defer s.Close()
	c := Client{BaseURL: s.URL, APIKey: key}
	_, err := c.Complete(t.Context(), nil, false)
	if err == nil || err.Error() != "LLM returned HTTP 400" {
		t.Fatalf("error=%v", err)
	}
	detail := ErrorDetail(err)
	if !strings.Contains(detail, "chat_template_kwargs") {
		t.Fatalf("detail lost the service message: %q", detail)
	}
	if strings.Contains(detail, key) || strings.Contains(detail, "supersecret") {
		t.Fatalf("detail leaks the key: %q", detail)
	}
	if ErrorDetail(errors.New("other")) != "" {
		t.Fatal("plain error has detail")
	}
}

func TestErrorDetailShapes(t *testing.T) {
	long := strings.Repeat("слово ", 100)
	for body, want := range map[string]string{
		`{"error":"plain text error"}`:         "plain text error",
		`{"message":"top level"}`:              "top level",
		`<html>blocked</html>`:                 "",
		``:                                     "",
		`{"error":{"message":"` + long + `"}}`: "…",
	} {
		got := errorDetail([]byte(body), "")
		if want == "" && got != "" || !strings.Contains(got, want) {
			t.Fatalf("%q -> %q, want %q", body, got, want)
		}
	}
}

func TestTemplateKwargsOnlyForLenientHosts(t *testing.T) {
	for url, want := range map[string]bool{
		"https://api.groq.com/openai/v1":                          false,
		"https://generativelanguage.googleapis.com/v1beta/openai": false,
		"https://api.arena.ai/v1":                                 false,
		"https://api.deepseek.com/v1":                             true,
		"http://192.168.1.5:8000/v1":                              true,
	} {
		if acceptsTemplateKwargs(url) != want {
			t.Fatalf("%s: want %v", url, want)
		}
	}
	var body string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		w.WriteHeader(http.StatusTeapot)
	}))
	defer s.Close()
	lenient := Client{BaseURL: s.URL, Model: "m"}
	_, _ = lenient.Complete(t.Context(), nil, false)
	if !strings.Contains(body, "chat_template_kwargs") {
		t.Fatalf("lenient host lost the field: %s", body)
	}
}

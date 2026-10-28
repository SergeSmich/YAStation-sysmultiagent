package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"quasarbridge/internal/wire"
)

type recordingInjector struct {
	mu     sync.Mutex
	frames [][]byte
	err    error
}

func (r *recordingInjector) Client(_ context.Context, _ websocket.MessageType, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, append([]byte{}, data...))
	return r.err
}

func (r *recordingInjector) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

func askPost(body, remote string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	request.RemoteAddr = remote
	return request
}

func TestAskHandlerRejectsNonLoopback(t *testing.T) {
	injector := &recordingInjector{}
	handler := askHandler(func() textInjector { return injector })
	response := httptest.NewRecorder()
	handler(response, askPost(`{"text":"привет"}`, "192.168.1.186:41000"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if injector.count() != 0 {
		t.Fatalf("frame was injected from a non-loopback caller")
	}
}

func TestAskHandlerRejectsBadBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"broken json", `{"text":`},
		{"blank text", `{"text":"   "}`},
		{"missing text", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			injector := &recordingInjector{}
			handler := askHandler(func() textInjector { return injector })
			response := httptest.NewRecorder()
			handler(response, askPost(tc.body, "127.0.0.1:41000"))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.Code)
			}
			if injector.count() != 0 {
				t.Fatalf("frame was injected for a rejected body")
			}
		})
	}
}

func TestAskHandlerWithoutSession(t *testing.T) {
	handler := askHandler(func() textInjector { return nil })
	response := httptest.NewRecorder()
	handler(response, askPost(`{"text":"привет"}`, "127.0.0.1:41000"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestAskHandlerInjectsTextInput(t *testing.T) {
	injector := &recordingInjector{}
	handler := askHandler(func() textInjector { return injector })
	response := httptest.NewRecorder()
	handler(response, askPost(`{"text":"  Алиса, спроси нейросеть: расскажи анекдот  "}`, "127.0.0.1:41000"))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Request string `json:"request"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.OK || body.Request == "" {
		t.Fatalf("response = %+v, want ok with request id", body)
	}
	if injector.count() != 1 {
		t.Fatalf("injected %d frames, want 1", injector.count())
	}
	decoded := wire.Decode(injector.frames[0], wire.ClientToServer)
	if got := decoded.Kind(); got != "TextInput" {
		t.Fatalf("frame kind = %q, want TextInput", got)
	}
	event := decoded.Client.GetEvent()
	if got := event.GetHeader().GetMessageId(); got != body.Request {
		t.Fatalf("message id = %q, want response id %q", got, body.Request)
	}
	if got := event.GetTextInput().GetRequest().GetEvent().GetText(); got != "Алиса, спроси нейросеть: расскажи анекдот" {
		t.Fatalf("text = %q", got)
	}
}

func TestAskHandlerReportsInjectionFailure(t *testing.T) {
	injector := &recordingInjector{err: errors.New("too many pending native requests")}
	handler := askHandler(func() textInjector { return injector })
	response := httptest.NewRecorder()
	handler(response, askPost(`{"text":"привет"}`, "127.0.0.1:41000"))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}

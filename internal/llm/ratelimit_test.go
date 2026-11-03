package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterFromDetail(t *testing.T) {
	cases := map[string]time.Duration{
		"Please try again in 1.5s.":    1500 * time.Millisecond,
		"Please try again in 200ms.":   200 * time.Millisecond,
		"Please try again in 1m30.5s.": 90*time.Second + 500*time.Millisecond,
		"no hint at all":               0,
		"try again later":              0,
	}
	for in, want := range cases {
		if got := retryAfterFromDetail(in); got != want {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

func TestOrgIDMaskedInDetail(t *testing.T) {
	body := []byte(`{"error":{"message":"Rate limit in organization org_01abcdefghijk on tokens per minute (TPM): Limit 8000"}}`)
	got := errorDetail(body, "")
	if strings.Contains(got, "org_01abc") || !strings.Contains(got, "TPM") {
		t.Fatalf("detail = %q", got)
	}
}

func rateLimitServer(t *testing.T, failures int32, retryAfter string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= failures {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached. Please try again in 2s."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Четыре."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func withFastSleep(t *testing.T) *time.Duration {
	t.Helper()
	var slept time.Duration
	old := retrySleep
	retrySleep = func(ctx context.Context, d time.Duration) error { slept = d; return nil }
	t.Cleanup(func() { retrySleep = old })
	return &slept
}

func TestChatRetriesOnceAfterRateLimit(t *testing.T) {
	slept := withFastSleep(t)
	srv, calls := rateLimitServer(t, 1, "1")
	c := &Client{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}
	m, err := c.Chat(t.Context(), []Message{{Role: "user", Content: "2+2"}}, nil)
	if err != nil || m.Content != "Четыре." {
		t.Fatalf("got %q, %v", m.Content, err)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want 2", *calls)
	}
	if *slept < time.Second || *slept > 2*time.Second {
		t.Fatalf("slept %v, want about the Retry-After of 1s", *slept)
	}
}

func TestChatUsesMessageHintWithoutHeader(t *testing.T) {
	slept := withFastSleep(t)
	srv, _ := rateLimitServer(t, 1, "")
	c := &Client{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}
	if _, err := c.Chat(t.Context(), []Message{{Role: "user", Content: "x"}}, nil); err != nil {
		t.Fatal(err)
	}
	if *slept < 2*time.Second || *slept > 3*time.Second {
		t.Fatalf("slept %v, want about 2s from the message", *slept)
	}
}

func TestChatDoesNotRetryTwiceOrLongWaits(t *testing.T) {
	withFastSleep(t)
	srv, calls := rateLimitServer(t, 5, "1")
	c := &Client{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}
	_, err := c.Chat(t.Context(), []Message{{Role: "user", Content: "x"}}, nil)
	if !IsRateLimited(err) {
		t.Fatalf("err = %v, want rate limit", err)
	}
	if *calls != 2 {
		t.Fatalf("calls = %d, want exactly one retry", *calls)
	}

	srv2, calls2 := rateLimitServer(t, 5, "60")
	c2 := &Client{BaseURL: srv2.URL, Model: "m", HTTP: srv2.Client()}
	if _, err := c2.Chat(t.Context(), []Message{{Role: "user", Content: "x"}}, nil); !IsRateLimited(err) {
		t.Fatalf("err = %v", err)
	}
	if *calls2 != 1 {
		t.Fatalf("a 60s wait must not be retried, calls = %d", *calls2)
	}
}

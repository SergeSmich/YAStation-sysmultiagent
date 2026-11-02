package sysadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPGetBasic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello world"))
	}))
	defer srv.Close()
	got, err := HTTPGet(context.Background(), srv.URL, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Fatalf("got=%q", got)
	}
}

func TestHTTPGetTruncate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("abcdefghijklmnopqrstuvwxyz"))
	}))
	defer srv.Close()
	got, err := HTTPGet(context.Background(), srv.URL, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got != "abcde…усечено" {
		t.Fatalf("got=%q", got)
	}
}

func TestHTTPGetRedirectSameHost(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			http.Redirect(w, r, "/b", http.StatusFound)
			return
		}
		w.Write([]byte("redirected"))
	}))
	defer srv.Close()
	got, err := HTTPGet(context.Background(), srv.URL+"/a", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got != "redirected" {
		t.Fatalf("got=%q", got)
	}
}

func TestHTTPGetRedirectDiffHost(t *testing.T) {
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("other"))
	}))
	defer srv2.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv2.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()
	_, err := HTTPGet(context.Background(), srv.URL+"/a", 1000)
	if err == nil || !contains(err.Error(), "different host") {
		t.Fatalf("expected diff host error, got %v", err)
	}
}

func TestHTTPGetBadScheme(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x"} {
		_, err := HTTPGet(context.Background(), u, 1000)
		if err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
}

func TestHTTPGet404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found body"))
	}))
	defer srv.Close()
	got, err := HTTPGet(context.Background(), srv.URL+"/nope", 1000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// 404 body returned as-is (documented behavior)
	if got != "not found body" {
		t.Fatalf("got=%q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

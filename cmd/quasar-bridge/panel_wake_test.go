package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWakeFixture builds the daemon's data directory the way the station has
// it: one directory per word, name.txt next to the model, and settings.json
// holding the enabled flags.
func writeWakeFixture(t *testing.T, dir string, settings string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "words", "егорка"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "words", "брифинг"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join("words", "егорка", "name.txt"):    "Егорка\n",
		filepath.Join("words", "егорка", "model.json"):  `{"takes":6,"threshold":0.4}`,
		filepath.Join("words", "брифинг", "name.txt"):   "брифинг\n",
		filepath.Join("words", "брифинг", "model.json"): `{"takes":3}`,
		"settings.json": settings,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWakeWordsFromDirReadsTheDaemonState: with the daemon stopped the panel
// still has to offer the trained words, so the same list is rebuilt from the
// files the daemon writes.
func TestWakeWordsFromDirReadsTheDaemonState(t *testing.T) {
	dir := t.TempDir()
	writeWakeFixture(t, dir, `{"channel":"vqe_0","gain":32,"dry_run":false,"words":{"егорка":{"enabled":true,"sensitivity":0},"брифинг":{"enabled":false,"sensitivity":0}}}`)

	words, err := wakeWordsFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 2 {
		t.Fatalf("words: %+v", words)
	}
	// Sorted by id, so the page shows a stable list across reloads.
	if words[0].Word != "брифинг" || words[0].Name != "брифинг" || words[0].Takes != 3 || words[0].Enabled {
		t.Fatalf("брифинг: %+v", words[0])
	}
	if words[1].Word != "егорка" || words[1].Name != "Егорка" || words[1].Takes != 6 || !words[1].Enabled {
		t.Fatalf("егорка: %+v", words[1])
	}
}

// TestWakeWordsFromDaemonReadsTheState: the daemon's own view wins when it runs,
// because only it knows the training quality.
func TestWakeWordsFromDaemonReadsTheState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/state" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"words":[
			{"id":"егорка","name":"Егорка","enabled":true,"takes":6},
			{"id":"брифинг","name":"брифинг","enabled":false,"takes":3}
		]}`))
	}))
	defer server.Close()
	t.Setenv("QUASAR_WAKE_URL", server.URL)

	words, err := wakeWordsFromDaemon()
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 2 || words[0].Word != "брифинг" || words[1].Takes != 6 || !words[1].Enabled {
		t.Fatalf("words: %+v", words)
	}
}

// TestPanelWakeDictionaryFallsBackToFiles covers the endpoint contract: 200 even
// when the daemon is down, so the settings form keeps working, with "source" and
// a note the page can show.
func TestPanelWakeDictionaryFallsBackToFiles(t *testing.T) {
	panel, _, _ := panelTestServer(t)
	dir := t.TempDir()
	writeWakeFixture(t, dir, `{"dry_run":false,"words":{"егорка":{"enabled":true}}}`)

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	t.Setenv("QUASAR_WAKE_URL", down.URL)
	t.Setenv("QUASAR_WAKE_DIR", dir)

	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/panel/wake/dictionary", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET = %d", recorder.Code)
	}

	cookie, _ := panelLogin(t, panel)
	request := httptest.NewRequest(http.MethodGet, "/api/panel/wake/dictionary", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"source":"files"`, `"word":"брифинг"`, `"name":"Егорка"`, `"takes":6`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dictionary lacks %s: %s", want, body)
		}
	}
}

// TestWakePageNeedsThePanelSession: the training page runs on its own port, so a
// session-less visitor is sent to the panel's own login instead of the daemon.
func TestWakePageNeedsThePanelSession(t *testing.T) {
	panel, _, _ := panelTestServer(t)
	proxy, err := newWakePageProxy("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	handler := panel.wakePageHandler(proxy)

	request := httptest.NewRequest(http.MethodGet, "https://192.168.1.210:8445/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("anonymous page = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Location"); got != "https://192.168.1.210:8444/login" {
		t.Fatalf("redirect = %q", got)
	}

	cookie, _ := panelLogin(t, panel)
	request = httptest.NewRequest(http.MethodGet, "https://192.168.1.210:8445/", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	// The upstream is dead on purpose: reaching it at all proves the session was
	// accepted, and the daemon's absence must not look like a panel failure.
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("authorized page = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestWakePageURLFollowsTheListener keeps the link the page shows honest: no
// handler, no link.
func TestWakePageURLFollowsTheListener(t *testing.T) {
	panel, _, _ := panelTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "https://192.168.1.210:8444/", nil)
	if got := panel.wakePageURL(request); got != "https://192.168.1.210:8445/" {
		t.Fatalf("link = %q", got)
	}
	panel.wakePage = nil
	if got := panel.wakePageURL(request); got != "" {
		t.Fatalf("link without a listener = %q", got)
	}
}

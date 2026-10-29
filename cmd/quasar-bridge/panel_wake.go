package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The wake-word daemon owns its own UI and its own word list. The bridge only
// reads that list, so the panel can offer "trained words" instead of a free-text
// field, and forwards the training page over the panel's TLS and session: the
// daemon listens on loopback only.
const (
	defaultWakeURL = "http://127.0.0.1:8780"
	defaultWakeDir = "/data/wakeword"

	wakeStateTimeout = 2 * time.Second
	wakeStateBytes   = 1 << 20
)

// wakeBaseURL is the daemon address. It is a constant for the station and an
// environment override for a development machine or a test.
func wakeBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("QUASAR_WAKE_URL")); value != "" {
		return strings.TrimRight(value, "/")
	}
	return defaultWakeURL
}

func wakeDir() string {
	if value := strings.TrimSpace(os.Getenv("QUASAR_WAKE_DIR")); value != "" {
		return value
	}
	return defaultWakeDir
}

// wakeWordInfo is one trained word as the panel needs it: what to show, whether
// the daemon will fire it, and how many takes the model was built from.
type wakeWordInfo struct {
	Word    string `json:"word"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Takes   int    `json:"takes"`
}

// apiWakeDictionary lists the words the daemon has learned. The list is read
// from the daemon's own state, which is the only place that knows the training
// quality; when the daemon is down the same data is reconstructed from its data
// directory, so the panel still lets the operator attach a word to a role.
func (p *panelServer) apiWakeDictionary(w http.ResponseWriter, _ *http.Request) {
	words, source, err := readWakeWords()
	payload := map[string]any{"words": words, "source": source}
	if err != nil {
		// Not an error status: the settings form must still work, and the page
		// shows the message next to the word list.
		payload["error"] = err.Error()
	}
	writePanelJSON(w, http.StatusOK, payload)
}

func readWakeWords() ([]wakeWordInfo, string, error) {
	words, err := wakeWordsFromDaemon()
	if err == nil {
		return words, "daemon", nil
	}
	fallback, fileErr := wakeWordsFromDir(wakeDir())
	if fileErr != nil {
		return []wakeWordInfo{}, "", errors.New("wake-word daemon is unavailable")
	}
	return fallback, "files", nil
}

// wakeWordsFromDaemon reads GET /api/state, the daemon's own view. The shape is
// its public one: a list of trained words with the training metrics.
func wakeWordsFromDaemon() ([]wakeWordInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wakeStateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wakeBaseURL()+"/api/state", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: wakeStateTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("wake-word daemon returned HTTP " + strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, wakeStateBytes+1))
	if err != nil || len(data) > wakeStateBytes {
		return nil, errors.New("cannot read the wake-word daemon state")
	}
	var state struct {
		Words []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			Takes   int    `json:"takes"`
		} `json:"words"`
	}
	if err := stdjson.Unmarshal(data, &state); err != nil {
		return nil, errors.New("invalid wake-word daemon state")
	}
	words := make([]wakeWordInfo, 0, len(state.Words))
	for _, item := range state.Words {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = id
		}
		words = append(words, wakeWordInfo{Word: id, Name: name, Enabled: item.Enabled, Takes: item.Takes})
	}
	sortWakeWords(words)
	return words, nil
}

// wakeWordsFromDir rebuilds the list from the daemon's data directory: one
// directory per word, settings.json keeps its enabled flag, model.json its take
// count. It is the fallback that keeps the panel usable when the daemon is not
// running.
func wakeWordsFromDir(dir string) ([]wakeWordInfo, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "words"))
	if err != nil {
		return nil, err
	}
	enabled, settingsErr := wakeEnabledMap(filepath.Join(dir, "settings.json"))
	words := make([]wakeWordInfo, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := strings.TrimSpace(entry.Name())
		if id == "" {
			continue
		}
		info := wakeWordInfo{Word: id, Name: id, Enabled: true}
		if value, ok := enabled[id]; ok {
			info.Enabled = value
		}
		wordDir := filepath.Join(dir, "words", entry.Name())
		if raw, readErr := os.ReadFile(filepath.Join(wordDir, "name.txt")); readErr == nil {
			if name := strings.TrimSpace(string(raw)); name != "" {
				info.Name = name
			}
		}
		if raw, readErr := os.ReadFile(filepath.Join(wordDir, "model.json")); readErr == nil {
			var model struct {
				Takes int `json:"takes"`
			}
			if stdjson.Unmarshal(raw, &model) == nil {
				info.Takes = model.Takes
			}
		}
		words = append(words, info)
	}
	if settingsErr != nil && len(words) == 0 {
		return nil, settingsErr
	}
	sortWakeWords(words)
	return words, nil
}

// wakeEnabledMap reads the daemon's settings file. A missing map means every
// trained word is reported as enabled, which is how the daemon treats it.
func wakeEnabledMap(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var settings struct {
		Words map[string]struct {
			Enabled bool `json:"enabled"`
		} `json:"words"`
	}
	if err := stdjson.Unmarshal(raw, &settings); err != nil {
		return nil, err
	}
	enabled := make(map[string]bool, len(settings.Words))
	for id, word := range settings.Words {
		enabled[id] = word.Enabled
	}
	return enabled, nil
}

func sortWakeWords(words []wakeWordInfo) {
	sort.Slice(words, func(i, j int) bool { return words[i].Word < words[j].Word })
}

// newWakePageProxy forwards the daemon's page, scripts and API unchanged. The
// daemon is loopback-only and path-relative, so the proxy runs on its own
// listener (:8445) with the panel's TLS certificate: no path rewriting, no
// second authentication scheme, the panel session alone decides access.
func newWakePageProxy(target string) (http.Handler, error) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("invalid wake-word daemon address")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(parsed)
			// The daemon may care about its own host header; the client does not
			// need the panel's host to leak into it.
			r.Out.Host = parsed.Host
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The daemon is a separate process; its absence must not look like a
			// panel failure.
			http.Error(w, "wake-word daemon is unavailable", http.StatusBadGateway)
		},
	}
	return proxy, nil
}

// wakePageHandler serves the proxied training page behind the panel session.
// A request without a session is sent to the panel login, so the page cannot be
// reached by anyone who could not reach the panel itself.
func (p *panelServer) wakePageHandler(proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.sessions.valid(panelSessionToken(r)) {
			http.Redirect(w, r, p.panelLoginURL(r), http.StatusSeeOther)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// panelLoginURL is the login page of the panel listener itself: the training
// page runs on another port, so a relative redirect would land on the daemon.
func (p *panelServer) panelLoginURL(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if host == "" || p.panelPort == "" {
		return "https://" + r.Host + "/login"
	}
	return "https://" + net.JoinHostPort(host, p.panelPort) + "/login"
}

// wakePageURL is the link the panel shows for the training page. Empty when the
// extra listener is disabled, and then the page offers the SSH tunnel instead.
func (p *panelServer) wakePageURL(r *http.Request) string {
	// The extra listener is started only when the handler exists, so its absence
	// is the one and only "the training page is off" signal.
	if p.wakePage == nil {
		return ""
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if host == "" {
		return ""
	}
	port := p.wakePagePort
	if port == "" {
		port = "8445"
	}
	return "https://" + net.JoinHostPort(host, port) + "/"
}

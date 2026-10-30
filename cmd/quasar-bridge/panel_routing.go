package main

import (
	"context"
	stdjson "encoding/json"
	"net/http"
	"strings"
	"time"
)

// panelRoutingTestBodyLimit is small on purpose: the box takes one phrase, not
// a document, and the classifier timeout already bounds the wait.
const panelRoutingTestBodyLimit = 4 << 10

// panelRoutingTestTimeout covers the classifier round trip plus a margin. The
// panel waits for the answer, so this is a ceiling, not a typical wait.
const panelRoutingTestTimeout = 30 * time.Second

// apiRoutingTest runs one phrase through the live routing policy and reports
// what the bridge would do with it: the rule that matched, or the classifier
// verdict. It is a diagnostic, not a guarantee — the classifier may answer
// differently on the next run, which is why source is reported separately.
func (p *panelServer) apiRoutingTest(w http.ResponseWriter, r *http.Request) {
	if p.decide == nil {
		writePanelJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "routing test is not available"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelRoutingTestBodyLimit)
	var body struct {
		Text string `json:"text"`
	}
	decoder := stdjson.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelRoutingTestTimeout)
	defer cancel()
	decision := p.decide(ctx, text)
	writePanelJSON(w, http.StatusOK, map[string]any{
		"text":   text,
		"route":  decision.Route,
		"rule":   decision.Rule,
		"source": decision.Source,
	})
}

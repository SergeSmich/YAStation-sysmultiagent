package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	upstreamRepo        = "skrashevich/quasar-bridge"
	upstreamReleasesAPI = "https://api.github.com/repos/" + upstreamRepo + "/releases/latest"
	updateCacheTTL      = 10 * time.Minute
	updateCheckTimeout  = 8 * time.Second
	// updateCommand is what the operator pastes into PowerShell on the machine
	// that owns the source tree. The station has no git and must never merge:
	// the script builds, tests and deploys, and keeps a snapshot for rollback.
	updateCommand = `powershell -NoProfile -ExecutionPolicy Bypass -File F:\Projects\Midi\bridge\work\update-upstream.ps1`
)

// buildVersion and buildTag are stamped at link time, for example
// -X main.buildVersion=stage16 -X main.buildTag=v0.5.0.
var (
	buildVersion = "dev"
	buildTag     = ""
)

// releaseInfo is one GitHub release, reduced to what the panel shows.
type releaseInfo struct {
	Tag       string `json:"tag"`
	URL       string `json:"url"`
	Published string `json:"published"`
}

// updateChecker remembers one GitHub answer for a while: the panel is polled,
// and unauthenticated API calls are rate limited per address.
type updateChecker struct {
	mu      sync.Mutex
	checked time.Time
	latest  releaseInfo
	err     string
}

func newUpdateChecker() *updateChecker { return new(updateChecker) }

// status returns the cached release unless the cache is stale. A station
// without internet is a normal state: the failure is reported and the previous
// answer, if any, is kept so the block does not flicker between runs.
func (c *updateChecker) status(ctx context.Context) (releaseInfo, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checked.IsZero() && time.Since(c.checked) < updateCacheTTL {
		return c.latest, c.err
	}
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	latest, err := fetchLatestRelease(checkCtx)
	c.checked = time.Now()
	if err != nil {
		c.err = err.Error()
		return c.latest, c.err
	}
	c.latest, c.err = latest, ""
	return c.latest, ""
}

func fetchLatestRelease(ctx context.Context) (releaseInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamReleasesAPI, nil)
	if err != nil {
		return releaseInfo{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "quasar-bridge-panel")
	client := &http.Client{Timeout: updateCheckTimeout}
	response, err := client.Do(request)
	if err != nil {
		return releaseInfo{}, err
	}
	defer response.Body.Close()
	var body struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Message     string `json:"message"`
	}
	if err := stdjson.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return releaseInfo{}, err
	}
	if response.StatusCode != http.StatusOK {
		if body.Message != "" {
			return releaseInfo{}, errors.New(body.Message)
		}
		return releaseInfo{}, fmt.Errorf("github answered %d", response.StatusCode)
	}
	return releaseInfo{Tag: body.TagName, URL: body.HTMLURL, Published: body.PublishedAt}, nil
}

// apiUpdate reports the build the station runs and the latest upstream release.
// It never changes anything: the update itself is the operator's script, which
// owns the source tree, the tests and the snapshot.
func (p *panelServer) apiUpdate(w http.ResponseWriter, r *http.Request) {
	latest, failure := p.updates.status(r.Context())
	payload := map[string]any{
		"current_version": buildVersion,
		"upstream":        upstreamRepo,
		"upstream_url":    "https://github.com/" + upstreamRepo,
		"command":         updateCommand,
		// Without a stamped tag the build cannot be compared to a release, and
		// claiming an update would be a guess.
		"update_available": failure == "" && latest.Tag != "" && buildTag != "" && latest.Tag != buildTag,
	}
	if buildTag != "" {
		payload["current_tag"] = buildTag
	}
	if latest.Tag != "" {
		payload["latest_tag"] = latest.Tag
		payload["latest_url"] = latest.URL
		payload["latest_published"] = latest.Published
	}
	if failure != "" {
		payload["error"] = failure
	}
	writePanelJSON(w, http.StatusOK, payload)
}

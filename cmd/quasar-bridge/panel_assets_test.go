package main

import (
	"strings"
	"testing"
)

// TestPageLinksCarryAssetVersion: the cached script and style must change their
// address when the file changes, or an update shows a new page with an old script.
func TestPageLinksCarryAssetVersion(t *testing.T) {
	page, err := panelAssets.ReadFile("panel.html")
	if err != nil {
		t.Fatal(err)
	}
	out := string(versionedAssets(page))
	for _, name := range []string{"/panel.js", "/panel.css"} {
		if !strings.Contains(out, `"`+name+`?v=`) {
			t.Fatalf("link to %s has no version", name)
		}
		if strings.Contains(out, `"`+name+`"`) {
			t.Fatalf("link to %s left unversioned", name)
		}
	}
}

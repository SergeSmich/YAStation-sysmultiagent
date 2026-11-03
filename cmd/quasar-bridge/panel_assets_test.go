package main

import (
	"strings"
	"testing"
)

// TestProviderKeyFieldIsNotAPasswordField: a type=password field makes the
// browser offer to store the API key. The field is a text input drawn as dots.
func TestProviderKeyFieldIsNotAPasswordField(t *testing.T) {
	script, err := panelAssets.ReadFile("panel.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "key.type = 'password'") {
		t.Fatal("API key field is a password field again")
	}
	style, _ := panelAssets.ReadFile("panel.css")
	if !strings.Contains(string(style), "-webkit-text-security") {
		t.Fatal("panel.css does not mask .secret-input")
	}
	if _, ok := llmPresetByID("arena"); !ok {
		t.Fatal("arena preset missing")
	}
}

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

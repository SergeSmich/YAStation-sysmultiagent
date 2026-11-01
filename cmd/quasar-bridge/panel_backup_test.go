package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportSettingsBlanksSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	src := `{"llm_providers":[{"name":"a","key":"sk-live","key_env":"A_KEY","max_tokens":40}],"nested":{"password":"hunter2","token":"","client_secret":"s3","max_tokens":9},"agents":["helper"]}`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, blanked, err := exportSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, leaked := range []string{"sk-live", "hunter2"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("export leaks %q:\n%s", leaked, text)
		}
	}
	if blanked != 3 {
		t.Fatalf("blanked = %d, want 3", blanked)
	}
	for _, kept := range []string{"A_KEY", "helper", `"max_tokens": 40`} {
		if !strings.Contains(text, kept) {
			t.Fatalf("export lost %q:\n%s", kept, text)
		}
	}
}

func TestExportSettingsRejectsBrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := exportSettings(path); err == nil {
		t.Fatal("broken config exported")
	}
}

// The page is split into capsule tabs; every card must belong to a group that
// has a tab, or it would be hidden for good.
func TestPanelTabsCoverEveryGroup(t *testing.T) {
	page, err := panelAssets.ReadFile("panel.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	tabs := map[string]bool{}
	for _, part := range strings.Split(html, `data-tab="`)[1:] {
		tabs[part[:strings.Index(part, `"`)]] = true
	}
	if len(tabs) < 4 {
		t.Fatalf("only %d tabs", len(tabs))
	}
	groups := strings.Split(html, `data-group="`)[1:]
	if len(groups) < 8 {
		t.Fatalf("only %d grouped cards", len(groups))
	}
	for _, part := range groups {
		group := part[:strings.Index(part, `"`)]
		if !tabs[group] {
			t.Fatalf("card group %q has no tab", group)
		}
	}
}
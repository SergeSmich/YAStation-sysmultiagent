package main

import (
	"strings"
	"testing"
)

// The UI is embedded and has no build step, so nothing else ties the HTML ids to
// the JS lookups or to the handlers that serve them. A renamed id leaves an empty
// section on the page while every Go test still passes; these checks keep the
// three files honest about the names they share.

// TestPanelUIWiresTheSettingsSections checks the elements the settings form is
// built from: the provider list, the role list, and the wake-word dictionary.
func TestPanelUIWiresTheSettingsSections(t *testing.T) {
	page, err := panelAssets.ReadFile("panel.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{
		"settings-form",
		"llm-providers-list",
		"add-llm-provider",
		"agents-list",
		"add-agent",
		"wake-words-list",
		"wake-dictionary-hint",
		"wake-train",
		"wake-train-hint",
		"settings-status",
		// Routing: the section, the rule table, the preset box, the classifier
		// prompt and the phrase tester all fail silently if an id drifts.
		"section-routing",
		"routing-preset",
		"routing-preset-apply",
		"routing-default",
		"routing-on-error",
		"routing-timeout",
		"routing-refusal",
		"routing-rules",
		"add-routing-rule",
		"routing-reset",
		"routing-prompt",
		"routing-test",
		"routing-test-text",
		"routing-test-result",
		// The update block only reports; it must not hide the numbers.
		"update-build",
		"update-latest",
		"update-check",
		"update-status",
		"update-command",
	} {
		if !strings.Contains(string(page), `id="`+id+`"`) {
			t.Fatalf("panel.html lacks #%s", id)
		}
	}
	// The training page opens in its own tab: an anchor, not a button that
	// would lose the session when it navigates the panel away.
	if !strings.Contains(string(page), `target="_blank"`) {
		t.Fatal("panel.html: the training-page link does not open a new tab")
	}
	// Four «Сохранить», four sections. Each button names the section it writes,
	// because the page sends only that section: one bad row in the providers must
	// not block saving the roles, and saving the roles must not rewrite the
	// providers that are still being edited.
	for _, scope := range []string{"providers", "words", "roles", "routing"} {
		marker := `data-scope="` + scope + `"`
		if !strings.Contains(string(page), marker) {
			t.Fatalf("panel.html: no «Сохранить» button for section %q (%s)", scope, marker)
		}
	}
	if got := strings.Count(string(page), `data-scope="`); got != 4 {
		t.Fatalf("panel.html: %d scoped save buttons, want exactly 4", got)
	}
}

// TestPanelJSCallsTheNewEndpoints: the page and the bridge must agree on the
// paths, otherwise the buttons report a 404 nobody sees until the operator
// presses them.
func TestPanelJSCallsTheNewEndpoints(t *testing.T) {
	script, err := panelAssets.ReadFile("panel.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"'/api/panel/llm/test'",
		"'/api/panel/llm/models'",
		"'/api/panel/llm/detect'",
		"'/api/panel/wake/dictionary'",
		"wake-dictionary-list",
		"model-select",
		"agent-provider",
		"provider_keys",
		"providerPayload",
		"wake_page",
		"'/api/panel/routing/test'",
		"'/api/panel/update'",
		"routing_presets",
		"routingPayload",
		"dataset.ruleField",
		// Scoped saves: the submit handler reads the section from the button and
		// the payload builder keys off it.
		"event.submitter",
		"settingsScopeLabels",
	} {
		if !strings.Contains(string(script), marker) {
			t.Fatalf("panel.js lacks %s", marker)
		}
	}
	// The payload builder is a whitelist: the transient fields the page keeps on
	// a row must not be sent, or the strict decoder refuses the whole write.
	if strings.Contains(string(script), "key_set:") {
		t.Fatal("panel.js sends key_set, which the write path does not accept")
	}
}

// TestPanelCSSStylesTheNewControls keeps the new rows from rendering as bare
// text: the link-as-button and the key state need their own classes.
func TestPanelCSSStylesTheNewControls(t *testing.T) {
	style, err := panelAssets.ReadFile("panel.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{".button-link", ".row-actions", ".key-state", ".routing-rule", ".routing-classifier", ".update-command"} {
		if !strings.Contains(string(style), marker) {
			t.Fatalf("panel.css lacks %s", marker)
		}
	}
}

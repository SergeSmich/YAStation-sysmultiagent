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
		// The spoken call: the switch above the roles and the main model card.
		"role-call-box",
		"role-call-enabled",
		"role-call-dry-run",
		"role-call-save",
		"main-agent-box",
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
	// Providers, roles and start words are saved card by card: the «Сохранить»
	// buttons are built by the script (data-save), so the page keeps a single
	// section-wide button, the routing one. The «+» buttons sit in the section
	// titles.
	if !strings.Contains(string(page), `data-scope="routing"`) {
		t.Fatal(`panel.html: no «Сохранить» button for the routing section`)
	}
	if got := strings.Count(string(page), `data-scope="`); got != 1 {
		t.Fatalf("panel.html: %d scoped save buttons, want exactly 1", got)
	}
	script, err := panelAssets.ReadFile("panel.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `button.dataset.save = kind`) {
		t.Fatal("panel.js: saveButton does not set data-save")
	}
	for _, kind := range []string{"provider", "agent", "word"} {
		if !strings.Contains(string(script), `saveButton('`+kind+`')`) {
			t.Fatalf("panel.js: no per-card «Сохранить» for %s", kind)
		}
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
		"model-combo",
		"'/api/panel/llm/check'",
		"'/api/panel/llm/ask'",
		"data-test-model",
		"llm_presets",
		"provider_models",
		"danger-solid",
		"agent-provider",
		"role-length",
		"max_output_tokens",
		"наследовать",
		"role-length",
		"max_output_tokens",
		"наследовать",
		"role-length",
		"max_output_tokens",
		"наследовать",
		"role-length",
		"max_output_tokens",
		"наследовать",
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
		// The spoken call: the phrase probe and the per-role checks.
		"data-call-probe",
		"data-call-check",
		"role_call",
		"aliases",
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
	for _, marker := range []string{".button-link", ".row-actions", ".key-state", ".key-note", ".combo-menu", ".danger-solid", ".routing-rule", ".routing-classifier", ".update-command", ".main-agent-box", ".model-mismatch", ".role-call-box", ".alias-chip", ".role-chips", ".role-chip", ".role-dialog", ".role-length"} {
		if !strings.Contains(string(style), marker) {
			t.Fatalf("panel.css lacks %s", marker)
		}
	}
}

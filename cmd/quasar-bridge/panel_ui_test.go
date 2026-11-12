package main

import (
	"os"
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
		// The role chips above the cards: the click picks the main model.
		"role-chips",
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
		// The role mini-dialog: its own endpoint and its three probe controls.
		"'/api/panel/llm/chat'",
		"dataset.roleAsk",
		"dataset.roleSay",
		"dataset.roleQuestion",
		"dataset.roleChip",
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

// TestPanelRoleDialogKeepsItsQuestionOutOfTheCard: the question box is a probe,
// not a field of the role. With data-field the card would carry «question» into
// the write path, whose decoder rejects unknown members, and saving the role
// after a question would fail with «запрос не разобран».
func TestPanelRoleDialogKeepsItsQuestionOutOfTheCard(t *testing.T) {
	script, err := panelAssets.ReadFile("panel.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), "settingsControl('input', 'question'") {
		t.Fatal("panel.js: the role dialog question is a card field, the write path rejects it")
	}
}

// TestPanelA0012ManifestAndDragOrder: the manifest link, the light-theme
// token overrides and the drag-and-drop handle classes must survive edits.
func TestPanelA0012ManifestAndDragOrder(t *testing.T) {
	page, err := panelAssets.ReadFile("panel.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{`<link rel="manifest" href="/manifest.json">`, `<link rel="icon" href="/icons/icon.svg"`} {
		if !strings.Contains(string(page), marker) {
			t.Fatalf("panel.html lacks %s", marker)
		}
	}
	css, err := panelAssets.ReadFile("panel.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"--err-line", "--warn-line", "cursor: grab", ".dragging", "card-dragging"} {
		if !strings.Contains(string(css), marker) {
			t.Fatalf("panel.css lacks %s", marker)
		}
	}
	script, err := panelAssets.ReadFile("panel.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"quasar.panel.cardOrder", "wireCardOrder", "applyCardOrder", "rememberCardOrder"} {
		if !strings.Contains(string(script), marker) {
			t.Fatalf("panel.js lacks %s", marker)
		}
	}
	// Restoring the saved order must permute the cards among the slots they
	// already hold. An appendChild pulls the non-card neighbours along: the
	// wake-training bar sits inside settings-form between «Нейросети» and
	// «Старт-слова» and jumped to the end of the form on every load after a drag.
	if !strings.Contains(string(script), "createComment('card-slot')") {
		t.Fatal("panel.js: applyCardOrder no longer permutes card slots, the training bar will move")
	}
	if strings.Contains(string(script), "for (const card of cards) container.appendChild(card)") {
		t.Fatal("panel.js: applyCardOrder appends cards and drags the training bar to the end of the form")
	}
	// panel.go itself is not embedded (it is the server, not a page asset), so
	// the manifest routes are checked from the source tree next to the test.
	src, err := os.ReadFile("panel.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"manifest.json", "icons/icon.svg", "application/manifest+json", "image/svg+xml", "manifest-src 'self'"} {
		if !strings.Contains(string(src), marker) {
			t.Fatalf("panel.go lacks %s", marker)
		}
	}
	// The panel is served from the root, so /panel would be a 404 start_url.
	manifest, err := panelAssets.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"start_url": "/"`) {
		t.Fatal(`manifest.json: start_url must point at the panel root, not /panel`)
	}
}

// TestPanelCSSStylesTheNewControls keeps the new rows from rendering as bare
// text: the link-as-button and the key state need their own classes.
func TestPanelCSSStylesTheNewControls(t *testing.T) {
	style, err := panelAssets.ReadFile("panel.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{".button-link", ".row-actions", ".key-state", ".key-note", ".combo-menu", ".danger-solid", ".routing-rule", ".routing-classifier", ".update-command", ".main-agent-box", ".model-mismatch", ".role-call-box", ".alias-chip", ".role-chips", ".role-chip", ".role-dialog", ".role-length", "--ctl-h", "--fs-base", "grid-template-columns: repeat(12", "@media (max-width: 700px)", "span-6"} {
		if !strings.Contains(string(style), marker) {
			t.Fatalf("panel.css lacks %s", marker)
		}
	}
}

// TestPanelRoleCardFieldsSpanTheGrid: the role card is a twelve-column grid, so
// a field without a span class shrinks to one column (72 px) and its label
// wraps to six lines. The live check of v0.9.1.19 found both offenders.
func TestPanelRoleCardFieldsSpanTheGrid(t *testing.T) {
	script, err := panelAssets.ReadFile("panel.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"settingsField('Имя роли', name, 'span-2')",
		"settingsField('Длина ответа', length, 'span-4')",
	} {
		if !strings.Contains(string(script), marker) {
			t.Fatalf("panel.js lacks %s: the field collapses to one column", marker)
		}
	}
	style, err := panelAssets.ReadFile("panel.css")
	if err != nil {
		t.Fatal(err)
	}
	// `.settings-row .field select { min-width: 0 }` outranks a bare
	// `.role-length`, so the rule has to name the field to take effect.
	if !strings.Contains(string(style), ".field select.role-length") {
		t.Fatal("panel.css: .role-length alone loses to the .field select min-width rule")
	}
}

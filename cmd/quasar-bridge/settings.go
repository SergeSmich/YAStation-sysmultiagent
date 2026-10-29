package main

import (
	stdjson "encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"

	"quasarbridge/internal/bridge"
)

// settingsSnapshotSuffix names the copy of a file kept before the panel writes
// it: config.json -> config.json.settings-prev, secrets.json likewise.
const settingsSnapshotSuffix = ".settings-prev"

// settingsPatch is a partial update of the three sections the operator edits
// from the panel. An absent section keeps the file untouched; a present one
// replaces the section whole, because a word table, a role list or a provider
// list is edited as a list, not field by field.
//
// All three sections are validated together: a start word may name a removed
// role and a role may name a removed provider, and that combination must be
// reported instead of written.
type settingsPatch struct {
	LLMProviders *[]llmProviderPatch `json:"llm_providers"`
	Agents       *[]agentConfig      `json:"agents"`
	WakeWords    *wakeWordsConfig    `json:"wake_words"`
}

// llmProviderPatch is one provider as the panel sends it back: the stored
// configuration plus the write-only credential fields. The key never reaches
// config.json and never comes back in a response.
type llmProviderPatch struct {
	llmProviderConfig
	APIKey   *string `json:"api_key"`
	ClearKey bool    `json:"clear_key"`
}

// settingsDeps is the slice of the running bridge the settings write path needs.
// Apply pushes confirmed sections into the live engine, so a saved file and a
// reacting bridge never disagree.
type settingsDeps struct {
	Voice   *voiceState
	Live    func() *bridge.Native
	Apply   func(config) error
	Secrets *secretStore
}

// panelSettingsBodyLimit bounds the settings body. It is larger than the other
// panel mutations because a role prompt may legally be MaxAgentPromptRunes long.
const panelSettingsBodyLimit = 256 << 10

// settingsSnapshot describes the editable sections as the next start would read
// them: role voices are already folded into local_voice.routes by validation,
// so the panel shows the effective values. Keys are reported as a set/unset map
// and never leave the station.
func settingsSnapshot(cfg config, secrets *secretStore) map[string]any {
	agents := cfg.Agents
	if agents == nil {
		agents = []agentConfig{}
	}
	// The page compares what it got with what it sends back, so an empty table
	// is published as [] rather than null, exactly like the write path stores it.
	providers := cfg.LLMProviders
	if providers == nil {
		providers = []llmProviderConfig{}
	}
	keys := make(map[string]bool, len(providers))
	if secrets != nil {
		stored, err := secrets.load()
		if err == nil {
			for _, p := range providers {
				keys[p.Name] = strings.TrimSpace(stored.Providers[p.Name]) != ""
			}
		}
	}
	wakeWords := cfg.WakeWords
	if wakeWords.Words == nil {
		wakeWords.Words = []wakeWordConfig{}
	}
	return map[string]any{
		"llm_providers": providers,
		"provider_keys": keys,
		"agents":        agents,
		"wake_words":    wakeWords,
		"actions":       []string{wakeListen, wakeAsk, wakeCommand},
		"voices":        bridge.KnownVoices,
	}
}

// applySettingsPatch performs the read-modify-write of config.json and, when the
// patch carries credentials, of secrets.json under the same lock as the voice
// write, then applies the result to the live bridge. It returns the HTTP status
// and the body of the answer.
func applySettingsPatch(path string, deps settingsDeps, patch settingsPatch) (int, any) {
	if patch.Agents == nil && patch.WakeWords == nil && patch.LLMProviders == nil {
		return http.StatusBadRequest, map[string]string{"error": "nothing to change: send llm_providers, agents, wake_words or all of them"}
	}
	if deps.Voice != nil {
		deps.Voice.applyMu.Lock()
		defer deps.Voice.applyMu.Unlock()
	}
	before, err := readConfig(path)
	if err != nil {
		return http.StatusInternalServerError, map[string]string{"error": err.Error()}
	}
	next := before
	if patch.LLMProviders != nil {
		providers := make([]llmProviderConfig, 0, len(*patch.LLMProviders))
		for _, item := range *patch.LLMProviders {
			providers = append(providers, item.llmProviderConfig)
		}
		next.LLMProviders = providers
	}
	if patch.Agents != nil {
		next.Agents = append([]agentConfig(nil), (*patch.Agents)...)
		// local_voice.routes is derived from the role voices, so the stale entries
		// have to go before validation: otherwise changing a role voice collides
		// with the route an earlier write left behind. Every agent:* route is
		// derived, so a hand-written one is dropped here as well.
		routes := make(map[string]string, len(before.LocalVoice.Routes))
		for route, voice := range before.LocalVoice.Routes {
			if _, derived := bridge.ParseAgentRoute(route); derived {
				continue
			}
			routes[route] = voice
		}
		if len(routes) == 0 {
			routes = nil
		}
		next.LocalVoice.Routes = routes
	}
	if patch.WakeWords != nil {
		next.WakeWords = *patch.WakeWords
	}
	// The same validation a restart runs, on the combination before it reaches
	// the disk: this also fills the defaults a legacy role inherits from llm_*.
	if err := next.validateProviders(); err != nil {
		return http.StatusBadRequest, map[string]string{"error": err.Error()}
	}
	if err := next.validateAgents(); err != nil {
		return http.StatusBadRequest, map[string]string{"error": err.Error()}
	}
	if err := next.validateWakeWords(); err != nil {
		return http.StatusBadRequest, map[string]string{"error": err.Error()}
	}

	// Credentials are folded in before anything is written, so a bad key is
	// reported with the rest of the validation instead of after the file change.
	var keys providerSecrets
	keysChanged := false
	if patch.LLMProviders != nil {
		current, loadErr := deps.Secrets.load()
		if loadErr != nil {
			return http.StatusInternalServerError, map[string]string{"error": loadErr.Error()}
		}
		keys, keysChanged, err = applyProviderKeys(current, next.LLMProviders, *patch.LLMProviders)
		if err != nil {
			return http.StatusBadRequest, map[string]string{"error": err.Error()}
		}
	}

	members := make(map[string]any, 4)
	if patch.LLMProviders != nil {
		if next.LLMProviders == nil {
			members["llm_providers"] = []llmProviderConfig{}
		} else {
			members["llm_providers"] = next.LLMProviders
		}
	}
	if patch.Agents != nil {
		if next.Agents == nil {
			members["agents"] = []agentConfig{}
		} else {
			members["agents"] = next.Agents
		}
		if !reflect.DeepEqual(before.LocalVoice, next.LocalVoice) {
			members["local_voice"] = next.LocalVoice
		}
	}
	if patch.WakeWords != nil {
		words := next.WakeWords
		if words.Words == nil {
			words.Words = []wakeWordConfig{}
		}
		members["wake_words"] = words
	}
	if !keysChanged &&
		reflect.DeepEqual(before.LLMProviders, next.LLMProviders) &&
		reflect.DeepEqual(before.Agents, next.Agents) &&
		reflect.DeepEqual(before.WakeWords, next.WakeWords) &&
		reflect.DeepEqual(before.LocalVoice, next.LocalVoice) {
		return http.StatusOK, settingsPayload(next, deps, true)
	}

	// The keys go first: a configuration that already points at a new provider
	// must never be applied with the old credential still in place.
	if keysChanged {
		if err := deps.Secrets.update(keys); err != nil {
			return http.StatusInternalServerError, map[string]string{"error": err.Error()}
		}
	}
	configWritten := len(members) > 0
	if configWritten {
		if err := writeConfigMembers(path, settingsSnapshotSuffix, members); err != nil {
			rollbackSettings(path, deps, false, keysChanged)
			return http.StatusInternalServerError, map[string]string{"error": err.Error()}
		}
	}
	// Re-read through the strict parser: the file on disk, not the request, must
	// be what the process starts from after the next restart.
	confirmed := before
	if configWritten {
		if confirmed, err = readConfig(path); err != nil {
			if !rollbackSettings(path, deps, true, keysChanged) {
				return http.StatusInternalServerError, map[string]string{"error": "written configuration was rejected and the previous files cannot be restored"}
			}
			return http.StatusBadRequest, map[string]string{"error": "written configuration was rejected: " + err.Error()}
		}
	}
	if err := applySettings(deps, confirmed); err != nil {
		// The files are valid but the live engine refused them. Put both back, so
		// a restart and the running process keep telling the same story.
		if !rollbackSettings(path, deps, configWritten, keysChanged) {
			return http.StatusInternalServerError, map[string]string{"error": err.Error() + "; the previous files cannot be restored"}
		}
		if previous, readErr := readConfig(path); readErr != nil {
			slog.Error("settings_rollback_read_failed", "error", readErr)
		} else if applyErr := applySettings(deps, previous); applyErr != nil {
			slog.Error("settings_rollback_apply_failed", "error", applyErr)
		}
		return http.StatusInternalServerError, map[string]string{"error": err.Error()}
	}
	slog.Info("settings_applied", "providers", len(confirmed.LLMProviders), "agents", len(confirmed.Agents), "wake_words", len(confirmed.WakeWords.Words), "dry_run", confirmed.WakeWords.DryRun)
	return http.StatusOK, settingsPayload(confirmed, deps, false)
}

// applyProviderKeys folds the write-only credential fields of the patch into the
// stored table. An absent field or an empty string keeps the stored key, so the
// panel can send every row back without knowing the secrets; clear_key removes
// one. Keys of a removed provider are dropped with it.
func applyProviderKeys(current providerSecrets, providers []llmProviderConfig, patch []llmProviderPatch) (providerSecrets, bool, error) {
	next := providerSecrets{Providers: make(map[string]string, len(current.Providers)+len(patch))}
	for name, key := range current.Providers {
		next.Providers[name] = key
	}
	known := make(map[string]bool, len(providers))
	for _, p := range providers {
		known[p.Name] = true
	}
	for name := range next.Providers {
		if !known[name] {
			delete(next.Providers, name)
		}
	}
	for _, item := range patch {
		name := strings.TrimSpace(item.Name)
		if item.ClearKey {
			delete(next.Providers, name)
			continue
		}
		if item.APIKey == nil {
			continue
		}
		key := strings.TrimSpace(*item.APIKey)
		if key == "" {
			continue
		}
		if err := validateAPIKey(key); err != nil {
			return providerSecrets{}, false, fmt.Errorf("llm_providers[%s].api_key: %w", name, err)
		}
		next.Providers[name] = key
	}
	return next, !reflect.DeepEqual(current.Providers, next.Providers), nil
}

// rollbackSettings restores the files the failed write replaced. It reports
// whether everything could be put back; the caller already has the original
// failure to show the operator.
func rollbackSettings(path string, deps settingsDeps, configWritten, secretsWritten bool) bool {
	restored := true
	if configWritten {
		if err := restoreConfig(path, settingsSnapshotSuffix); err != nil {
			slog.Error("settings_restore_failed", "error", err)
			restored = false
		}
	}
	if secretsWritten {
		if err := deps.Secrets.restore(); err != nil {
			slog.Error("secrets_restore_failed", "error", err)
			restored = false
		}
	}
	return restored
}

// applySettings pushes the sections into the live bridge and the live voice
// table, so a saved role answers and a saved word reacts without a restart.
func applySettings(deps settingsDeps, cfg config) error {
	if deps.Apply != nil {
		if err := deps.Apply(cfg); err != nil {
			return err
		}
	}
	if deps.Voice != nil {
		deps.Voice.SetIfChanged(cfg.LocalVoice)
	}
	if native := deps.Live(); native != nil {
		native.SetVoice(cfg.LocalVoice)
	}
	return nil
}

func settingsPayload(cfg config, deps settingsDeps, unchanged bool) map[string]any {
	payload := settingsSnapshot(cfg, deps.Secrets)
	payload["wake_words_count"] = len(cfg.WakeWords.Words)
	if deps.Voice != nil {
		payload["local_voice_revision"] = deps.Voice.Revision()
	}
	if unchanged {
		payload["unchanged"] = true
	}
	return payload
}

// restoreConfig puts the snapshot taken before the write back in place.
func restoreConfig(path, snapshotSuffix string) error {
	backup, err := os.ReadFile(path + snapshotSuffix)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, backup)
}

// apiSettings hands the panel the editable sections with the values as they will
// be applied, plus the vocabularies its forms need. Credentials are reported as
// set/unset only.
func (p *panelServer) apiSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := readConfig(p.configPath)
	if err != nil {
		writePanelJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	payload := settingsSnapshot(cfg, p.settings.Secrets)
	// The training page lives on its own listener; the page opens it in a tab
	// and falls back to the SSH tunnel when the listener is off.
	payload["wake_page"] = p.wakePageURL(r)
	writePanelJSON(w, http.StatusOK, payload)
}

// apiSettingsUpdate is the panel's write path to llm_providers, agents and
// wake_words. It shares applySettingsPatch with the file handling, so
// validation, snapshots and hot-apply are one code path.
func (p *panelServer) apiSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, panelSettingsBodyLimit)
	decoder := stdjson.NewDecoder(r.Body)
	// A misspelled field would otherwise silently drop a whole section.
	decoder.DisallowUnknownFields()
	var patch settingsPatch
	if err := decoder.Decode(&patch); err != nil {
		p.audit.record("settings_update", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	status, payload := applySettingsPatch(p.configPath, p.settings, patch)
	extra := map[string]any{"status": status}
	if patch.LLMProviders != nil {
		extra["llm_providers"] = len(*patch.LLMProviders)
	}
	if patch.Agents != nil {
		extra["agents"] = len(*patch.Agents)
	}
	if patch.WakeWords != nil {
		extra["wake_words"] = len(patch.WakeWords.Words)
		extra["dry_run"] = patch.WakeWords.DryRun
	}
	p.audit.record("settings_update", r, panelAuditResult(status), extra)
	writePanelJSON(w, status, payload)
}

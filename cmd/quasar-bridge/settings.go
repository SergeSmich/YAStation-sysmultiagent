package main

import (
	stdjson "encoding/json"
	"log/slog"
	"net/http"
	"os"
	"reflect"

	"quasarbridge/internal/bridge"
)

// settingsPatch is a partial update of the two sections the operator edits from
// the panel. An absent section keeps the file untouched; a present one replaces
// the section whole, because a word table or a role list is edited as a list,
// not field by field.
//
// Both sections are validated together: a start word may name a removed role,
// and that combination must be reported instead of written.
type settingsPatch struct {
	Agents    *[]agentConfig   `json:"agents"`
	WakeWords *wakeWordsConfig `json:"wake_words"`
}

// settingsDeps is the slice of the running bridge the settings write path needs.
// Apply pushes confirmed sections into the live engine, so a saved file and a
// reacting bridge never disagree.
type settingsDeps struct {
	Voice *voiceState
	Live  func() *bridge.Native
	Apply func(config) error
}

// panelSettingsBodyLimit bounds the settings body. It is larger than the other
// panel mutations because a role prompt may legally be MaxAgentPromptRunes long.
const panelSettingsBodyLimit = 256 << 10

// settingsSnapshot describes the editable sections as the next start would read
// them: role voices are already folded into local_voice.routes by validation,
// so the panel shows the effective values.
func settingsSnapshot(cfg config) map[string]any {
	agents := cfg.Agents
	if agents == nil {
		agents = []agentConfig{}
	}
	// The page compares what it got with what it sends back, so an empty table
	// is published as [] rather than null, exactly like the write path stores it.
	wakeWords := cfg.WakeWords
	if wakeWords.Words == nil {
		wakeWords.Words = []wakeWordConfig{}
	}
	return map[string]any{
		"agents":     agents,
		"wake_words": wakeWords,
		"actions":    []string{wakeListen, wakeAsk, wakeCommand},
		"voices":     bridge.KnownVoices,
	}
}

// applySettingsPatch performs the read-modify-write of config.json under the
// same lock as the voice write and then applies the result to the live bridge.
// It returns the HTTP status and the body of the answer.
func applySettingsPatch(path string, deps settingsDeps, patch settingsPatch) (int, any) {
	if patch.Agents == nil && patch.WakeWords == nil {
		return http.StatusBadRequest, map[string]string{"error": "nothing to change: send agents, wake_words or both"}
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
	// the disk: this also fills the defaults a role inherits from llm_*.
	if err := next.validateAgents(); err != nil {
		return http.StatusBadRequest, map[string]string{"error": err.Error()}
	}
	if err := next.validateWakeWords(); err != nil {
		return http.StatusBadRequest, map[string]string{"error": err.Error()}
	}

	members := make(map[string]any, 3)
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
	if reflect.DeepEqual(before.Agents, next.Agents) &&
		reflect.DeepEqual(before.WakeWords, next.WakeWords) &&
		reflect.DeepEqual(before.LocalVoice, next.LocalVoice) {
		return http.StatusOK, settingsPayload(next, deps, true)
	}

	if err := writeConfigMembers(path, ".settings-prev", members); err != nil {
		return http.StatusInternalServerError, map[string]string{"error": err.Error()}
	}
	// Re-read through the strict parser: the file on disk, not the request, must
	// be what the process starts from after the next restart.
	confirmed, err := readConfig(path)
	if err != nil {
		if restoreErr := restoreConfig(path, ".settings-prev"); restoreErr != nil {
			slog.Error("settings_restore_failed", "error", restoreErr)
			return http.StatusInternalServerError, map[string]string{"error": "written configuration was rejected and the previous file cannot be restored"}
		}
		return http.StatusBadRequest, map[string]string{"error": "written configuration was rejected: " + err.Error()}
	}
	if err := applySettings(deps, confirmed); err != nil {
		// The file is valid but the live engine refused it. Put both back, so a
		// restart and the running process keep telling the same story.
		if restoreErr := restoreConfig(path, ".settings-prev"); restoreErr != nil {
			slog.Error("settings_restore_failed", "error", restoreErr)
			return http.StatusInternalServerError, map[string]string{"error": err.Error() + "; the previous file cannot be restored"}
		}
		if previous, readErr := readConfig(path); readErr != nil {
			slog.Error("settings_rollback_read_failed", "error", readErr)
		} else if applyErr := applySettings(deps, previous); applyErr != nil {
			slog.Error("settings_rollback_apply_failed", "error", applyErr)
		}
		return http.StatusInternalServerError, map[string]string{"error": err.Error()}
	}
	slog.Info("settings_applied", "agents", len(confirmed.Agents), "wake_words", len(confirmed.WakeWords.Words), "dry_run", confirmed.WakeWords.DryRun)
	return http.StatusOK, settingsPayload(confirmed, deps, false)
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
	payload := settingsSnapshot(cfg)
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
// be applied, plus the vocabularies its forms need.
func (p *panelServer) apiSettings(w http.ResponseWriter, _ *http.Request) {
	cfg, err := readConfig(p.configPath)
	if err != nil {
		writePanelJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writePanelJSON(w, http.StatusOK, settingsSnapshot(cfg))
}

// apiSettingsUpdate is the panel's write path to agents and wake_words. It
// shares applySettingsPatch with the file handling, so validation, snapshot and
// hot-apply are one code path.
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

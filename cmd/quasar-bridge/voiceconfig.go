package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"quasarbridge/internal/bridge"
)

// voiceState is the single source of truth for the local voice settings. The
// configuration file is the durable copy, this is the value every new answer
// uses. The revision lets callers tell two successive hot-applies apart.
type voiceState struct {
	value atomic.Pointer[bridge.VoiceConfig]
	rev   atomic.Uint64
	// applyMu serializes the read-modify-write of config.json, so two panel
	// requests or a SIGHUP reload cannot lose one another's update.
	applyMu sync.Mutex
}

func (s *voiceState) Load() bridge.VoiceConfig {
	if cfg := s.value.Load(); cfg != nil {
		return *cfg
	}
	return bridge.VoiceConfig{}
}

func (s *voiceState) Store(cfg bridge.VoiceConfig) uint64 {
	c := cfg
	s.value.Store(&c)
	return s.rev.Add(1)
}

// SetIfChanged stores cfg only when it differs from the value in force, so a
// reload or a repeated apply does not bump the revision for nothing.
func (s *voiceState) SetIfChanged(cfg bridge.VoiceConfig) (uint64, bool) {
	if reflect.DeepEqual(s.Load(), cfg) {
		return s.rev.Load(), false
	}
	return s.Store(cfg), true
}

func (s *voiceState) Revision() uint64 { return s.rev.Load() }

// voicePatch is a partial update: an absent field keeps its current value, so
// a panel can change one setting without resending the whole section.
type voicePatch struct {
	Voice      *string            `json:"voice"`
	Emotion    *string            `json:"emotion"`
	Speed      *float64           `json:"speed"`
	Volume     *float64           `json:"volume"`
	Candidates *[]string          `json:"candidates"`
	Routes     *map[string]string `json:"routes"`
}

func (p voicePatch) apply(current bridge.VoiceConfig) bridge.VoiceConfig {
	if p.Voice != nil {
		current.Voice = *p.Voice
	}
	if p.Emotion != nil {
		current.Emotion = *p.Emotion
	}
	if p.Speed != nil {
		current.Speed = *p.Speed
	}
	if p.Volume != nil {
		current.Volume = *p.Volume
	}
	if p.Candidates != nil {
		current.Candidates = *p.Candidates
	}
	if p.Routes != nil {
		current.Routes = *p.Routes
	}
	// encoding/json/v2 round-trips an absent map as an empty one; keep the two
	// forms indistinguishable so a reload sees identical values.
	if len(current.Routes) == 0 {
		current.Routes = nil
	}
	if len(current.Candidates) == 0 {
		current.Candidates = nil
	}
	return current
}

// writeLocalVoice replaces only the local_voice member of the configuration
// file. See writeConfigMembers for the guarantees; the snapshot keeps the name
// the earlier releases documented.
func writeLocalVoice(path string, voice bridge.VoiceConfig) error {
	return writeConfigMembers(path, ".voice-prev", map[string]any{"local_voice": voice})
}

// writeConfigMembers replaces only the named members of the configuration file:
// every other member keeps the exact bytes it had, so a hand-edited documented
// field is never re-serialized from our struct. The previous file is kept next
// to it under snapshotSuffix and the new one is swapped in atomically.
func writeConfigMembers(path, snapshotSuffix string, members map[string]any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read configuration file")
	}
	var top map[string]jsontext.Value
	if err := json.Unmarshal(raw, &top); err != nil {
		return errors.New("invalid configuration JSON")
	}
	for name, value := range members {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		top[name] = jsontext.Value(encoded)
	}
	data, err := json.Marshal(top, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path+snapshotSuffix, raw); err != nil {
		return fmt.Errorf("cannot keep the previous configuration: %w", err)
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, keeping the mode of the file it replaces.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// reloadVoice re-reads local_voice from disk under the hot-apply lock and pushes
// it to the live session, so a SIGHUP and a panel write cannot interleave.
func (s *voiceState) reloadVoice(path string, live func() *bridge.Native) (uint64, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	next, err := readConfig(path)
	if err != nil {
		return 0, err
	}
	// A hand-edited config may still name a voice the dispenser rejects; the
	// candidate chain covers it, but the operator should know why the chosen
	// voice is not heard.
	if unknown := next.LocalVoice.UnknownVoices(); len(unknown) > 0 {
		slog.Warn("local_voice_unsupported", "voices", unknown, "supported", bridge.KnownVoicesList())
	}
	revision, changed := s.SetIfChanged(next.LocalVoice)
	if !changed {
		return revision, nil
	}
	if native := live(); native != nil {
		native.SetVoice(next.LocalVoice)
	}
	slog.Info("local_voice_reloaded", "revision", revision, "voice", next.LocalVoice.Voice, "emotion", next.LocalVoice.Emotion, "speed", next.LocalVoice.Speed, "volume", next.LocalVoice.Volume)
	return revision, nil
}

// voiceConfigHandler serves the local voice section. Reading is open to the
// same callers as /healthz; writing is a hot-apply: config.json is updated and
// the live session picks the new voice up without a restart or a reconnect.
func voiceConfigHandler(path *string, state *voiceState, live func() *bridge.Native) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeProbeJSON(w, http.StatusOK, map[string]any{"local_voice": state.Load(), "revision": state.Revision()})
			return
		}
		if !requestIsLoopback(r) {
			writeProbeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
		if err != nil {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		var patch voicePatch
		if err := json.Unmarshal(body, &patch); err != nil {
			writeProbeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		status, payload := applyVoicePatch(*path, state, patch, live)
		writeProbeJSON(w, status, payload)
	}
}

// applyVoicePatch performs the read-modify-write of config.json under the state
// lock and returns the HTTP status and body of the answer.
func applyVoicePatch(path string, state *voiceState, patch voicePatch, live func() *bridge.Native) (int, any) {
	state.applyMu.Lock()
	defer state.applyMu.Unlock()
	next, err := readConfig(path)
	if err != nil {
		return http.StatusInternalServerError, map[string]string{"error": err.Error()}
	}
	applied := patch.apply(next.LocalVoice)
	if err := applied.Validate(); err != nil {
		return http.StatusBadRequest, map[string]string{"error": err.Error()}
	}
	if unknown := applied.UnknownVoices(); len(unknown) > 0 {
		return http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unsupported voice %s: the synthesizer accepts %s",
				strings.Join(unknown, ", "), bridge.KnownVoicesList()),
		}
	}
	if reflect.DeepEqual(applied, next.LocalVoice) {
		return http.StatusOK, map[string]any{"local_voice": applied, "revision": state.Revision(), "unchanged": true}
	}
	if err := writeLocalVoice(path, applied); err != nil {
		return http.StatusInternalServerError, map[string]string{"error": err.Error()}
	}
	// Re-read through the strict parser: the file on disk, not the request,
	// must be what the process starts from after the next restart.
	confirmed, err := readConfig(path)
	if err != nil {
		backup, readErr := os.ReadFile(path + ".voice-prev")
		if readErr == nil {
			readErr = writeFileAtomic(path, backup)
		}
		if readErr != nil {
			return http.StatusInternalServerError, map[string]string{"error": "written configuration was rejected and the previous file cannot be restored"}
		}
		return http.StatusBadRequest, map[string]string{"error": "written configuration was rejected: " + err.Error()}
	}
	revision := state.Store(confirmed.LocalVoice)
	if native := live(); native != nil {
		native.SetVoice(confirmed.LocalVoice)
	}
	slog.Info("local_voice_applied", "revision", revision, "voice", confirmed.LocalVoice.Voice, "emotion", confirmed.LocalVoice.Emotion, "speed", confirmed.LocalVoice.Speed, "volume", confirmed.LocalVoice.Volume, "candidates", confirmed.LocalVoice.Candidates)
	return http.StatusOK, map[string]any{"local_voice": confirmed.LocalVoice, "revision": revision}
}

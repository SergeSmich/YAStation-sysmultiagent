package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// The station speaks through one ALSA mixer control, and the bridge runs as
// root on the station, so it can move it. The speech protocol also carries a
// per-utterance volume, but a tone at 58% and 92% with the mixer proved that the
// protocol field changes nothing audible while the mixer does, and that the
// station's own volume manager does not rewrite the mixer after a request.
const (
	stationVolumeCard    = "0"
	stationVolumeControl = "Master"
)

// stationVolumeFile keeps the last level the operator set. It is a file of its
// own on purpose: config.json is read with unknown members rejected, so a new
// key there would make the previous binary refuse to start and would block a
// rollback.
const stationVolumeFile = "station_volume.json"

var stationVolumePercent = regexp.MustCompile(`\[(\d{1,3})%\]`)

// stationMixer runs amixer under a short deadline. Tests replace it, because the
// test machine has no station mixer.
var stationMixer = func(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "amixer", args...).CombinedOutput()
}

// readStationVolume returns the mixer level in percent as it is right now.
func readStationVolume() (int, error) {
	out, err := stationMixer("-c", stationVolumeCard, "sget", stationVolumeControl)
	if err != nil {
		return 0, fmt.Errorf("amixer sget %s: %w", stationVolumeControl, err)
	}
	matches := stationVolumePercent.FindAllStringSubmatch(string(out), -1)
	if len(matches) == 0 {
		return 0, errors.New("amixer sget: no level in answer")
	}
	// Both channels report the same level, so the first bracket is enough and
	// does not depend on how many channels the control has.
	level, err := strconv.Atoi(matches[0][1])
	if err != nil || level < 0 || level > 100 {
		return 0, errors.New("amixer sget: unreadable level")
	}
	return level, nil
}

// writeStationVolume moves the mixer level and lets amixer reject a value the
// control does not accept.
func writeStationVolume(percent int) error {
	if percent < 0 || percent > 100 {
		return errors.New("level must be within 0..100")
	}
	if _, err := stationMixer("-c", stationVolumeCard, "sset", stationVolumeControl, strconv.Itoa(percent)+"%"); err != nil {
		return fmt.Errorf("amixer sset %s: %w", stationVolumeControl, err)
	}
	return nil
}

// stationVolumeStore remembers the level in the bridge's own directory.
type stationVolumeStore struct {
	path string
}

func newStationVolumeStore(configPath string) stationVolumeStore {
	dir := filepath.Dir(configPath)
	if dir == "" {
		dir = "."
	}
	return stationVolumeStore{path: filepath.Join(dir, stationVolumeFile)}
}

// load returns the stored level; ok is false when there is nothing usable, which
// is the normal state until the operator moves the slider once.
func (s stationVolumeStore) load() (int, bool) {
	if s.path == "" {
		return 0, false
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return 0, false
	}
	var stored struct {
		Percent *int `json:"percent"`
	}
	if stdjson.Unmarshal(raw, &stored) != nil || stored.Percent == nil {
		return 0, false
	}
	if *stored.Percent < 0 || *stored.Percent > 100 {
		return 0, false
	}
	return *stored.Percent, true
}

func (s stationVolumeStore) save(percent int) error {
	if s.path == "" {
		return nil
	}
	raw, err := stdjson.Marshal(map[string]int{"percent": percent})
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

// applySavedStationVolume restores the level the operator set last time. A
// machine without amixer is not a reason to stop: the panel reports it instead.
func applySavedStationVolume(configPath string) {
	percent, ok := newStationVolumeStore(configPath).load()
	if !ok {
		return
	}
	// A development machine has no amixer and no station mixer: that is not a
	// failure worth a warning, and the panel reports the feature as unavailable.
	if _, err := exec.LookPath("amixer"); err != nil {
		slog.Debug("station_volume_skipped", "reason", "amixer not found")
		return
	}
	if err := writeStationVolume(percent); err != nil {
		slog.Warn("station_volume_apply_failed", "percent", percent, "error", err)
		return
	}
	slog.Info("station_volume_applied", "percent", percent)
}

// apiStationVolume reports the current mixer level and the stored one, so the
// slider opens where the speaker actually is.
func (p *panelServer) apiStationVolume(w http.ResponseWriter, r *http.Request) {
	percent, err := readStationVolume()
	if err != nil {
		writePanelJSON(w, http.StatusOK, map[string]any{"available": false, "error": err.Error()})
		return
	}
	saved, hasSaved := p.stationVolume.load()
	writePanelJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"percent":   percent,
		"saved":     saved,
		"has_saved": hasSaved,
	})
}

// apiStationVolumeUpdate is the overall level of the station: it moves music and
// speech together, the way the station's own volume buttons do.
func (p *panelServer) apiStationVolumeUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, panelMutationBodyLimit)
	var body struct {
		Percent *int `json:"percent"`
	}
	if err := stdjson.NewDecoder(r.Body).Decode(&body); err != nil {
		p.audit.record("station_volume", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if body.Percent == nil || *body.Percent < 0 || *body.Percent > 100 {
		p.audit.record("station_volume", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "percent must be 0..100"})
		return
	}
	if err := writeStationVolume(*body.Percent); err != nil {
		p.audit.record("station_volume", r, "error", map[string]any{"error": err.Error()})
		writePanelJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	percent, err := readStationVolume()
	if err != nil {
		// The mixer accepted the value but did not report it back. Keep the
		// requested number for the page, but do not store it: the next start
		// would restore a level the speaker may never have held.
		percent = *body.Percent
	} else if err := p.stationVolume.save(percent); err != nil {
		slog.Warn("station_volume_save_failed", "error", err)
	}
	p.audit.record("station_volume", r, "ok", map[string]any{"percent": percent})
	writePanelJSON(w, http.StatusOK, map[string]any{"available": true, "percent": percent})
}

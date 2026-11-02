package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubStationMixer replaces the amixer runner for one test and restores it after,
// because the real one would either fail or move the sound of the test machine.
func stubStationMixer(t *testing.T, fn func(args ...string) ([]byte, error)) *[][]string {
	t.Helper()
	old := stationMixer
	calls := make([][]string, 0, 2)
	stationMixer = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return fn(args...)
	}
	t.Cleanup(func() { stationMixer = old })
	return &calls
}

func TestReadStationVolumeParsesLevel(t *testing.T) {
	const answer = "Simple mixer control 'Master',0\n" +
		"  Front Left: Playback 147 [58%] [-26.19dB]\n" +
		"  Front Right: Playback 147 [58%] [-26.19dB]\n"
	calls := stubStationMixer(t, func(...string) ([]byte, error) {
		return []byte(answer), nil
	})

	level, err := readStationVolume()
	if err != nil {
		t.Fatalf("readStationVolume: %v", err)
	}
	if level != 58 {
		t.Fatalf("level = %d, want 58", level)
	}
	if got := strings.Join((*calls)[0], " "); got != "-c 0 sget Master" {
		t.Fatalf("amixer args = %q", got)
	}
}

func TestReadStationVolumeRejectsAnswerWithoutLevel(t *testing.T) {
	stubStationMixer(t, func(...string) ([]byte, error) {
		return []byte("Unable to find simple control 'Master',0\n"), nil
	})
	if _, err := readStationVolume(); err == nil {
		t.Fatal("readStationVolume accepted an answer without a level")
	}
}

func TestWriteStationVolumeLimits(t *testing.T) {
	calls := stubStationMixer(t, func(...string) ([]byte, error) {
		return nil, nil
	})
	for _, percent := range []int{-1, 101} {
		if err := writeStationVolume(percent); err == nil {
			t.Fatalf("writeStationVolume(%d) returned no error", percent)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("amixer was called %d times for invalid levels", len(*calls))
	}

	if err := writeStationVolume(80); err != nil {
		t.Fatalf("writeStationVolume(80): %v", err)
	}
	if got := strings.Join((*calls)[0], " "); got != "-c 0 sset Master 80%" {
		t.Fatalf("amixer args = %q", got)
	}
}

func TestStationVolumeStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newStationVolumeStore(filepath.Join(dir, "config.json"))
	if _, ok := store.load(); ok {
		t.Fatal("empty store reported a saved level")
	}
	if err := store.save(72); err != nil {
		t.Fatalf("save: %v", err)
	}
	level, ok := store.load()
	if !ok || level != 72 {
		t.Fatalf("load = %d, %v", level, ok)
	}
	info, err := os.Stat(filepath.Join(dir, stationVolumeFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// NTFS reports a fixed mode; the 0600 matters on the station, not in tests.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestStationVolumeStoreIgnoresBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stationVolumeFile)
	for _, body := range []string{"", "{", `{"percent":200}`, `{"percent":"громко"}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, ok := newStationVolumeStore(filepath.Join(dir, "config.json")).load(); ok {
			t.Fatalf("broken file %q reported a saved level", body)
		}
	}
}

func TestAPIStationVolumeUpdateAppliesAndSaves(t *testing.T) {
	level := 58
	calls := stubStationMixer(t, func(args ...string) ([]byte, error) {
		if len(args) >= 3 && args[2] == "sset" {
			switch args[len(args)-1] {
			case "80%":
				level = 80
			default:
				return nil, errors.New("unexpected level")
			}
			return []byte("Front Left: Playback 204 [80%] [-11.81dB]\n"), nil
		}
		return []byte("Front Left: Playback [" + itoa(level) + "%]\nFront Right: Playback [" + itoa(level) + "%]\n"), nil
	})
	dir := t.TempDir()
	p := &panelServer{stationVolume: newStationVolumeStore(filepath.Join(dir, "config.json"))}

	recorder := httptest.NewRecorder()
	p.apiStationVolumeUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/panel/station/volume",
		strings.NewReader(`{"percent":80}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"percent":80`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("amixer calls = %d, want sset and sget", len(*calls))
	}
	if saved, ok := p.stationVolume.load(); !ok || saved != 80 {
		t.Fatalf("saved = %d, %v", saved, ok)
	}
}

func TestAPIStationVolumeUpdateDoesNotSaveUnconfirmedLevel(t *testing.T) {
	// sset is accepted, sget fails: the page may show the requested number, but
	// the next start must not restore a level the mixer never reported.
	stubStationMixer(t, func(args ...string) ([]byte, error) {
		if len(args) >= 3 && args[2] == "sset" {
			return nil, nil
		}
		return nil, errors.New("sget failed")
	})
	p := &panelServer{stationVolume: newStationVolumeStore(filepath.Join(t.TempDir(), "config.json"))}
	recorder := httptest.NewRecorder()
	p.apiStationVolumeUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/panel/station/volume",
		strings.NewReader(`{"percent":80}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"percent":80`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if _, ok := p.stationVolume.load(); ok {
		t.Fatal("an unconfirmed level was stored")
	}
}

func TestAPIStationVolumeUpdateRejectsBadPercent(t *testing.T) {
	calls := stubStationMixer(t, func(...string) ([]byte, error) {
		return nil, nil
	})
	p := &panelServer{stationVolume: newStationVolumeStore(filepath.Join(t.TempDir(), "config.json"))}
	for _, body := range []string{`{}`, `{"percent":-1}`, `{"percent":101}`, `{"percent":"громко"}`} {
		recorder := httptest.NewRecorder()
		p.apiStationVolumeUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/panel/station/volume",
			strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, recorder.Code)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("amixer was called %d times for invalid bodies", len(*calls))
	}
}

func TestAPIStationVolumeReportsUnavailable(t *testing.T) {
	stubStationMixer(t, func(...string) ([]byte, error) {
		return nil, errors.New("exec: \"amixer\": executable file not found in %PATH%")
	})
	p := &panelServer{stationVolume: newStationVolumeStore(filepath.Join(t.TempDir(), "config.json"))}
	recorder := httptest.NewRecorder()
	p.apiStationVolume(recorder, httptest.NewRequest(http.MethodGet, "/api/panel/station/volume", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"available":false`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

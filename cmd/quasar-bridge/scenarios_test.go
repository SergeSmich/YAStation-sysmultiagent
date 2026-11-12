package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quasarbridge/internal/bridge"
)

// newScenarioTestStore builds a store in its own directory, next to a config.json
// that is never created: the scenarios file lives beside it.
func newScenarioTestStore(t *testing.T, timezone string) *scenarioStore {
	t.Helper()
	return newScenarioStore(filepath.Join(t.TempDir(), "config.json"), timezone)
}

// scenarioTickInputs collects what one tick did.
type scenarioTickInputs struct {
	sounds  []string
	volumes []int
	playErr error
}

func newScenarioSchedulerForTest(store *scenarioStore, inputs *scenarioTickInputs) scenarioScheduler {
	return scenarioScheduler{
		store: store,
		loc:   time.UTC,
		playSound: func(ctx context.Context, source string) error {
			inputs.sounds = append(inputs.sounds, source)
			return inputs.playErr
		},
		setVolume: func(percent int) error {
			inputs.volumes = append(inputs.volumes, percent)
			return nil
		},
		now: time.Now,
	}
}

// TestScenarioTickFiresOncePerMinuteAndOnItsDays covers the schedule core:
// a scenario fires only in its own minute, only on its own days, at most once
// per minute, and never twice after a restart inside the same minute.
func TestScenarioTickFiresOncePerMinuteAndOnItsDays(t *testing.T) {
	store := newScenarioTestStore(t, "UTC")
	// 2026-11-16 is a Monday; the week runs to Sunday 22 November.
	if err := store.save([]scenarioEntry{{
		ID: "s-1", Enabled: true, Time: "07:30", Days: []int{1, 2, 3, 4, 5},
		Action: scenarioAction{Type: "sound", Source: "timer"},
	}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	inputs := &scenarioTickInputs{}
	sch := newScenarioSchedulerForTest(store, inputs)

	monday := time.Date(2026, 11, 16, 7, 30, 15, 0, time.UTC)
	sch.tick(context.Background(), monday)
	if len(inputs.sounds) != 1 || inputs.sounds[0] != "timer" {
		t.Fatalf("sounds = %v, want one timer", inputs.sounds)
	}
	// Same minute again — a restart inside the minute must not repeat it.
	sch.tick(context.Background(), monday.Add(20*time.Second))
	if len(inputs.sounds) != 1 {
		t.Fatalf("sounds after repeat = %v, want still one", inputs.sounds)
	}
	// Another day of the week, same minute: fires again, in a new minute.
	tuesday := time.Date(2026, 11, 17, 7, 30, 1, 0, time.UTC)
	sch.tick(context.Background(), tuesday)
	if len(inputs.sounds) != 2 {
		t.Fatalf("sounds after Tuesday = %v, want two", inputs.sounds)
	}
	// Saturday: outside the working days.
	saturday := time.Date(2026, 11, 21, 7, 30, 30, 0, time.UTC)
	sch.tick(context.Background(), saturday)
	if len(inputs.sounds) != 2 {
		t.Fatalf("sounds on Saturday = %v, want still two", inputs.sounds)
	}
	// 07:31: another minute, the scenario is not due.
	after := time.Date(2026, 11, 16, 7, 31, 0, 0, time.UTC)
	sch.tick(context.Background(), after)
	if len(inputs.sounds) != 2 {
		t.Fatalf("sounds at 07:31 = %v, want still two", inputs.sounds)
	}

	stored, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored = %+v, want one entry", stored)
	}
	if stored[0].LastFired != "2026-11-17T07:30" {
		t.Fatalf("last_fired = %q, want the Tuesday minute", stored[0].LastFired)
	}
}

// TestScenarioTickSkipsDisabledAndRespectsVolume covers the other action kind:
// a volume scenario moves the mixer independently of the sound path, and a
// disabled entry never fires.
func TestScenarioTickSkipsDisabledAndRespectsVolume(t *testing.T) {
	store := newScenarioTestStore(t, "UTC")
	if err := store.save([]scenarioEntry{{
		ID: "s-1", Enabled: false, Time: "06:00", Days: []int{1},
		Action: scenarioAction{Type: "sound", Source: "notification"},
	}, {
		ID: "s-2", Enabled: true, Time: "06:00", Days: []int{1},
		Action: scenarioAction{Type: "volume", Percent: 80},
	}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	inputs := &scenarioTickInputs{}
	sch := newScenarioSchedulerForTest(store, inputs)
	sch.tick(context.Background(), time.Date(2026, 11, 16, 6, 0, 2, 0, time.UTC))

	if len(inputs.sounds) != 0 {
		t.Fatalf("sounds = %v, want none for a disabled entry", inputs.sounds)
	}
	if len(inputs.volumes) != 1 || inputs.volumes[0] != 80 {
		t.Fatalf("volumes = %v, want one 80", inputs.volumes)
	}
}

// TestScenarioTickKeepsTickingAfterAFailedAction: a sound that cannot play
// (no speaker, tools disabled) must not stop the scheduler or the bridge.
func TestScenarioTickKeepsTickingAfterAFailedAction(t *testing.T) {
	store := newScenarioTestStore(t, "UTC")
	if err := store.save([]scenarioEntry{{
		ID: "s-1", Enabled: true, Time: "07:30", Days: []int{1},
		Action: scenarioAction{Type: "sound", Source: "timer"},
	}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	inputs := &scenarioTickInputs{playErr: context.DeadlineExceeded}
	sch := newScenarioSchedulerForTest(store, inputs)
	// First minute fails, the next Monday is a fresh minute: the sound is tried
	// again instead of being dropped for the week.
	sch.tick(context.Background(), time.Date(2026, 11, 16, 7, 30, 1, 0, time.UTC))
	sch.tick(context.Background(), time.Date(2026, 11, 23, 7, 30, 1, 0, time.UTC))
	if len(inputs.sounds) != 2 {
		t.Fatalf("sound attempts = %d, want two across the two days", len(inputs.sounds))
	}
}

// TestAPIScenariosRoundTrip: GET returns the list and the timezone, POST stores
// the whole valid list and answers with what was stored.
func TestAPIScenariosRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newScenarioStore(filepath.Join(dir, "config.json"), "Europe/Moscow")
	p := &panelServer{scenarios: store}

	recorder := httptest.NewRecorder()
	p.apiScenarios(recorder, httptest.NewRequest(http.MethodGet, "/api/panel/scenarios", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var empty map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &empty); err != nil {
		t.Fatalf("GET body: %v", err)
	}
	if _, ok := empty["scenarios"]; !ok {
		t.Fatalf("GET body has no scenarios: %s", recorder.Body.String())
	}

	body := `{"scenarios":[{"enabled":true,"time":"07:30","days":[1,2,3,4,5],
		"action":{"type":"sound","source":"timer"}}]}`
	recorder = httptest.NewRecorder()
	p.apiScenariosUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/panel/scenarios",
		strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var stored map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &stored); err != nil {
		t.Fatalf("POST body: %v", err)
	}
	if stored["timezone"] != "Europe/Moscow" {
		t.Fatalf("timezone = %v", stored["timezone"])
	}
	list, _ := stored["scenarios"].([]any)
	if len(list) != 1 {
		t.Fatalf("stored scenarios = %v", stored["scenarios"])
	}
	first, _ := list[0].(map[string]any)
	if first["id"] != "s-1" {
		t.Fatalf("server id = %v, want s-1", first["id"])
	}
	raw, err := os.ReadFile(filepath.Join(dir, scenarioFileName))
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if !strings.Contains(string(raw), `"id": "s-1"`) {
		t.Fatalf("file = %s", raw)
	}
}

// TestAPIScenariosRejectsBadLists: a broken time, a broken day, a broken percent
// and a broken action type are refused with 400, and the file is untouched.
func TestAPIScenariosRejectsBadLists(t *testing.T) {
	dir := t.TempDir()
	store := newScenarioStore(filepath.Join(dir, "config.json"), "Europe/Moscow")
	p := &panelServer{scenarios: store}
	// Seed the file, so "unchanged" has something to compare against.
	if err := store.save([]scenarioEntry{{
		ID: "s-1", Enabled: true, Time: "07:30", Days: []int{1},
		Action: scenarioAction{Type: "sound", Source: "timer"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(dir, scenarioFileName))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	bad := []string{
		`{"scenarios":[{"enabled":true,"time":"7:30","days":[1],"action":{"type":"sound","source":"timer"}}]}`,
		`{"scenarios":[{"enabled":true,"time":"24:00","days":[1],"action":{"type":"sound","source":"timer"}}]}`,
		`{"scenarios":[{"enabled":true,"time":"07:30","days":[],"action":{"type":"sound","source":"timer"}}]}`,
		`{"scenarios":[{"enabled":true,"time":"07:30","days":[8],"action":{"type":"sound","source":"timer"}}]}`,
		`{"scenarios":[{"enabled":true,"time":"07:30","days":[1],"action":{"type":"sound","source":"sirena"}}]}`,
		`{"scenarios":[{"enabled":true,"time":"07:30","days":[1],"action":{"type":"volume","percent":101}}]}`,
		`{"scenarios":[{"enabled":true,"time":"07:30","days":[1],"action":{"type":"sleep"}}]}`,
	}
	for _, body := range bad {
		recorder := httptest.NewRecorder()
		p.apiScenariosUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/panel/scenarios",
			strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), `"error"`) {
			t.Fatalf("body %s: no Russian error message: %s", body, recorder.Body.String())
		}
		after, err := os.ReadFile(filepath.Join(dir, scenarioFileName))
		if err != nil {
			t.Fatalf("read after %s: %v", body, err)
		}
		if string(after) != string(before) {
			t.Fatalf("body %s: file changed on a rejected list", body)
		}
	}
}

// TestAPIScenariosAcceptsEmptyList: the empty schedule is a valid state, and the
// file holds an empty list rather than nothing at all.
func TestAPIScenariosAcceptsEmptyList(t *testing.T) {
	dir := t.TempDir()
	store := newScenarioStore(filepath.Join(dir, "config.json"), "Europe/Moscow")
	p := &panelServer{scenarios: store}

	recorder := httptest.NewRecorder()
	p.apiScenariosUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/panel/scenarios",
		strings.NewReader(`{"scenarios":[]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	entries, err := store.load()
	if err != nil || len(entries) != 0 {
		t.Fatalf("load = %v, %v; want empty", entries, err)
	}
}

// TestAPIScenariosGetLeaksNothing: GET answers exactly the timezone and the
// list — no internal fields of an entry reach the page.
func TestAPIScenariosGetLeaksNothing(t *testing.T) {
	dir := t.TempDir()
	store := newScenarioStore(filepath.Join(dir, "config.json"), "Europe/Moscow")
	p := &panelServer{scenarios: store}
	if err := store.save([]scenarioEntry{{
		ID: "s-1", Enabled: true, Time: "07:30", Days: []int{1},
		Action: scenarioAction{Type: "volume", Percent: 66}, LastFired: "2026-11-16T07:30",
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	recorder := httptest.NewRecorder()
	p.apiScenarios(recorder, httptest.NewRequest(http.MethodGet, "/api/panel/scenarios", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("GET answered %d keys: %s", len(payload), recorder.Body.String())
	}
	if _, ok := payload["timezone"]; !ok {
		t.Fatal("GET has no timezone")
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(payload["scenarios"], &list); err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	for _, key := range []string{"id", "enabled", "time", "days", "action", "last_fired"} {
		if _, ok := list[0][key]; !ok {
			t.Fatalf("entry is missing %q: %v", key, list[0])
		}
	}
	if _, ok := list[0]["renamed_field"]; ok {
		t.Fatal("GET leaked an unexpected field")
	}
}

// TestScenarioFileWrittenWithMode0600: the schedule is operator state next to
// the configuration, not a secret — but it is still written privately.
func TestScenarioFileWrittenWithMode0600(t *testing.T) {
	dir := t.TempDir()
	store := newScenarioStore(filepath.Join(dir, "config.json"), "Europe/Moscow")
	if err := store.save([]scenarioEntry{{
		ID: "s-1", Enabled: true, Time: "07:30", Days: []int{1},
		Action: scenarioAction{Type: "sound", Source: "timer"},
	}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, scenarioFileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

// TestScenarioSoundPlayer: the scheduler's sound action needs a live session and
// assistant tools. Both missing are reported, not fatal.
func TestScenarioSoundPlayer(t *testing.T) {
	disabled := scenarioSoundPlayer(nil, false)
	if err := disabled(context.Background(), "timer"); err == nil {
		t.Fatal("disabled tools accepted a sound")
	}
	noSession := scenarioSoundPlayer(func() *bridge.Native { return nil }, true)
	if err := noSession(context.Background(), "timer"); err == nil {
		t.Fatal("a missing session accepted a sound")
	}
}

// TestValidateScenariosMessages: the validation messages are the ones the panel
// shows, in Russian.
func TestValidateScenariosMessages(t *testing.T) {
	err := validateScenarios([]scenarioEntry{{Time: "7:30", Days: []int{1},
		Action: scenarioAction{Type: "sound", Source: "timer"}}})
	if err == nil || !strings.Contains(err.Error(), "ЧЧ:ММ") {
		t.Fatalf("bad time: %v", err)
	}
	err = validateScenarios([]scenarioEntry{{Time: "07:30", Days: []int{},
		Action: scenarioAction{Type: "sound", Source: "timer"}}})
	if err == nil || !strings.Contains(err.Error(), "день недели") {
		t.Fatalf("empty days: %v", err)
	}
	err = validateScenarios([]scenarioEntry{{Time: "07:30", Days: []int{1},
		Action: scenarioAction{Type: "volume", Percent: 101}}})
	if err == nil || !strings.Contains(err.Error(), "0 до 100") {
		t.Fatalf("bad percent: %v", err)
	}
}

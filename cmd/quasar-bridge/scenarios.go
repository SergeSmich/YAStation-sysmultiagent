package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	// The station may ship without system tzdata, and the schedule is written in
	// device_context.timezone (an IANA name): embedding the database keeps
	// LoadLocation working on every machine.
	_ "time/tzdata"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/sound"
)

// scenarioFileName is the storage of the scheduled scenarios: one file next to
// config.json, mode 0600, written only through writeFileAtomic. config.json
// itself is not extended.
const scenarioFileName = "scenarios.json"

// scenarioAction is exactly one of two kinds: a sound signal (notification,
// success, error or timer) or a station volume level.
type scenarioAction struct {
	Type    string `json:"type"`
	Source  string `json:"source,omitempty"`
	Percent int    `json:"percent,omitempty"`
}

// scenarioEntry is one scheduled scenario. LastFired stores the moment of the
// last firing in the station timezone, minute precision: it is the guard that
// fires a scenario at most once per minute and does not repeat it after a
// restart inside the same minute.
type scenarioEntry struct {
	ID        string         `json:"id"`
	Enabled   bool           `json:"enabled"`
	Time      string         `json:"time"`
	Days      []int          `json:"days"`
	Action    scenarioAction `json:"action"`
	LastFired string         `json:"last_fired"`
}

// scenarioStore reads and writes the scenarios file. The timezone name is kept
// here so the panel can show the schedule the scheduler runs in without
// opening the configuration again.
type scenarioStore struct {
	path     string
	timezone string
	mu       sync.Mutex
}

func newScenarioStore(configPath, timezone string) *scenarioStore {
	dir := filepath.Dir(configPath)
	if dir == "" {
		dir = "."
	}
	return &scenarioStore{path: filepath.Join(dir, scenarioFileName), timezone: timezone}
}

func (s *scenarioStore) load() ([]scenarioEntry, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []scenarioEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var top struct {
		Scenarios []scenarioEntry `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	if top.Scenarios == nil {
		top.Scenarios = []scenarioEntry{}
	}
	return top.Scenarios, nil
}

func (s *scenarioStore) save(entries []scenarioEntry) error {
	if entries == nil {
		entries = []scenarioEntry{}
	}
	top := struct {
		Scenarios []scenarioEntry `json:"scenarios"`
	}{entries}
	data, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data)
}

// saveValidated validates the incoming full list, assigns the ids the server
// owns, and stores it. A rejected list never reaches the file: the panel
// handler answers 400 before any write.
func (s *scenarioStore) saveValidated(entries []scenarioEntry) error {
	if err := validateScenarios(entries); err != nil {
		return err
	}
	assignScenarioIDs(entries)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(entries)
}

// setLastFired records firing moments without dropping edits made in between:
// it re-reads the current list, updates the ids that fired, and stores it.
func (s *scenarioStore) setLastFired(fired []scenarioEntry, when string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.load()
	if err != nil {
		return err
	}
	byID := map[string]*scenarioEntry{}
	for i := range current {
		byID[current[i].ID] = &current[i]
	}
	for _, sc := range fired {
		if entry, ok := byID[sc.ID]; ok {
			entry.LastFired = when
		}
	}
	return s.save(current)
}

var scenarioTimeRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// validateScenarios checks the contract of the panel list: time HH:MM in the
// 24h clock, a non-empty set of ISO weekdays 1..7 (1 = Monday), and an action
// of exactly one of the two kinds. The messages are shown to the operator.
func validateScenarios(entries []scenarioEntry) error {
	for i := range entries {
		sc := &entries[i]
		if !scenarioTimeRE.MatchString(sc.Time) {
			return fmt.Errorf("сценарий %d: время должно быть в формате ЧЧ:ММ", i+1)
		}
		if len(sc.Days) == 0 {
			return fmt.Errorf("сценарий %d: выберите хотя бы один день недели", i+1)
		}
		for _, day := range sc.Days {
			if day < 1 || day > 7 {
				return fmt.Errorf("сценарий %d: день недели должен быть от 1 до 7 (1 = понедельник)", i+1)
			}
		}
		switch sc.Action.Type {
		case "sound":
			switch sc.Action.Source {
			case "notification", "success", "error", "timer":
			default:
				return fmt.Errorf("сценарий %d: звук должен быть notification, success, error или timer", i+1)
			}
		case "volume":
			if sc.Action.Percent < 0 || sc.Action.Percent > 100 {
				return fmt.Errorf("сценарий %d: громкость должна быть от 0 до 100", i+1)
			}
		default:
			return fmt.Errorf("сценарий %d: тип действия должен быть sound или volume", i+1)
		}
	}
	return nil
}

// assignScenarioIDs fills the ids the server owns; the panel never invents them.
func assignScenarioIDs(entries []scenarioEntry) {
	used := map[string]bool{}
	for _, sc := range entries {
		if sc.ID != "" {
			used[sc.ID] = true
		}
	}
	next := 1
	for i := range entries {
		if entries[i].ID != "" {
			continue
		}
		for used[fmt.Sprintf("s-%d", next)] {
			next++
		}
		entries[i].ID = fmt.Sprintf("s-%d", next)
		used[entries[i].ID] = true
		next++
	}
}

// scenarioScheduler plays the scheduled scenarios. One goroutine, a 25s tick,
// station local time in device_context.timezone. Actions run outside the list
// lock: a slow sound does not block the next tick.
type scenarioScheduler struct {
	store     *scenarioStore
	loc       *time.Location
	playSound func(ctx context.Context, source string) error
	setVolume func(percent int) error
	now       func() time.Time
}

func scenarioLocation(name string) *time.Location {
	if name == "" {
		name = "Europe/Moscow"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// validTimezone only checks the shape; the name may still be unknown
		// here. Fall back to UTC instead of failing the bridge.
		slog.Warn("scenario timezone unusable, schedule runs in UTC", "timezone", name, "error", err)
		return time.UTC
	}
	return loc
}

func (s scenarioScheduler) run(ctx context.Context) {
	s.tick(ctx, s.now())
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx, s.now())
		}
	}
}

// tick fires the scenarios due at now. A scenario fires at most once per
// minute (LastFired is minute precision), only on its own weekdays, and not
// again after a restart inside the same minute.
func (s scenarioScheduler) tick(ctx context.Context, now time.Time) {
	now = now.In(s.loc)
	minute := now.Format("2006-01-02T15:04")
	entries, err := s.store.load()
	if err != nil {
		slog.Warn("scenario store unreadable", "error", err)
		return
	}
	var due []scenarioEntry
	for _, sc := range entries {
		if !sc.Enabled {
			continue
		}
		hour, minuteOf, ok := parseScenarioTime(sc.Time)
		if !ok {
			continue
		}
		if now.Hour() != hour || now.Minute() != minuteOf {
			continue
		}
		if !scenarioDayMatches(sc.Days, now) {
			continue
		}
		if sc.LastFired == minute {
			continue
		}
		due = append(due, sc)
	}
	if len(due) == 0 {
		return
	}
	// Actions run outside the list lock: playback may take a moment, and the
	// next tick must not wait for it.
	for _, sc := range due {
		switch sc.Action.Type {
		case "sound":
			if err := s.playSound(ctx, sc.Action.Source); err != nil {
				// A disabled assistant tool is a configuration matter, not a
				// crash: warn, keep the bridge alive, the next tick retries
				// only in a new minute.
				slog.Warn("scenario sound skipped", "id", sc.ID, "source", sc.Action.Source, "error", err)
			}
		case "volume":
			if err := s.setVolume(sc.Action.Percent); err != nil {
				slog.Warn("scenario volume failed", "id", sc.ID, "percent", sc.Action.Percent, "error", err)
			}
		}
	}
	if err := s.store.setLastFired(due, minute); err != nil {
		slog.Warn("scenario last_fired not saved", "error", err)
	}
}

func parseScenarioTime(value string) (int, int, bool) {
	match := scenarioTimeRE.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, false
	}
	var hour, minute int
	if _, err := fmt.Sscanf(match[1], "%d", &hour); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(match[2], "%d", &minute); err != nil {
		return 0, 0, false
	}
	return hour, minute, true
}

// isoWeekday maps Sunday=0 to ISO 7; Monday is 1.
func isoWeekday(t time.Time) int {
	day := int(t.Weekday())
	if day == 0 {
		return 7
	}
	return day
}

func scenarioDayMatches(days []int, now time.Time) bool {
	day := isoWeekday(now)
	for _, candidate := range days {
		if candidate == day {
			return true
		}
	}
	return false
}

// scenarioSoundPlayer turns the scheduler's sound action into a playback on
// the live session. A scenario fires on its own clock, so there is no speaker
// request to queue the directive on: the sound goes as a protocol push, the
// same way a proactive cloud message plays. Assistant tools disabled or no
// live session is a configuration matter: the player reports the reason, and
// the scheduler logs it and keeps ticking.
func scenarioSoundPlayer(live func() *bridge.Native, soundEnabled bool) func(ctx context.Context, source string) error {
	return func(ctx context.Context, source string) error {
		if !soundEnabled {
			return errors.New("assistant tools are disabled")
		}
		native := live()
		if native == nil {
			return errors.New("voice session is unavailable")
		}
		return native.PushSound(ctx, sound.Request{Sound: source})
	}
}

// apiScenarios returns the schedule the panel edits, together with the station
// timezone it runs in, so the card can label the local times.
func (p *panelServer) apiScenarios(w http.ResponseWriter, r *http.Request) {
	entries, err := p.scenarios.load()
	if err != nil {
		slog.Warn("scenario store unreadable", "error", err)
		writePanelJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось прочитать сценарии"})
		return
	}
	writePanelJSON(w, http.StatusOK, map[string]any{"timezone": p.scenarios.timezone, "scenarios": entries})
}

// apiScenariosUpdate stores the full list the panel submits. The list is
// validated whole and stored whole: a rejected list never touches the file.
func (p *panelServer) apiScenariosUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, panelMutationBodyLimit)
	var body struct {
		Scenarios []scenarioEntry `json:"scenarios"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		p.audit.record("scenarios", r, "bad_request", nil)
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректный запрос"})
		return
	}
	if err := p.scenarios.saveValidated(body.Scenarios); err != nil {
		p.audit.record("scenarios", r, "rejected", map[string]any{"error": err.Error()})
		writePanelJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	stored, err := p.scenarios.load()
	if err != nil {
		slog.Warn("scenario store unreadable after save", "error", err)
		writePanelJSON(w, http.StatusOK, map[string]any{"timezone": p.scenarios.timezone, "scenarios": body.Scenarios})
		return
	}
	p.audit.record("scenarios", r, "ok", map[string]any{"count": len(stored)})
	writePanelJSON(w, http.StatusOK, map[string]any{"timezone": p.scenarios.timezone, "scenarios": stored})
}

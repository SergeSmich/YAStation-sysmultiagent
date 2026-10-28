package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The station is a 1 GB device: the sampler keeps one day of ten-second points
// (8 640 of them, a few hundred kilobytes), and every view is aggregated down to
// at most this many buckets before it leaves the process.
const (
	panelMetricsInterval  = 10 * time.Second
	panelMetricsKeep      = 24 * time.Hour
	panelMetricsMaxPoints = 720
	panelMetricsSaveEvery = 5 * time.Minute
	panelMetricsEventKeep = 200
	panelMetricsRangeMin  = 300
	panelMetricsRangeMax  = int(panelMetricsKeep / time.Second)
	panelMetricsMinTemp   = 75.0
	panelMetricsHotTemp   = 85.0
	panelMetricsMemShare  = 0.85
	panelMetricsDiskShare = 0.9
	panelMetricsLoadLimit = 4.0
	panelMetricsWeakRSSI  = -75.0
)

// panelMetricPoint is one sample of the station. Every value is optional: a
// missing file, a missing interface or a first run without a previous read all
// end up as null rather than as a misleading zero.
type panelMetricPoint struct {
	Time   int64    `json:"t"`
	Uptime *float64 `json:"up,omitempty"`
	CPU    *float64 `json:"cpu,omitempty"`
	Temp   *float64 `json:"temp,omitempty"`
	Mem    *float64 `json:"mem,omitempty"`
	Disk   *float64 `json:"disk,omitempty"`
	RSSI   *float64 `json:"rssi,omitempty"`
	Load   *float64 `json:"load,omitempty"`
	Rx     *float64 `json:"rx,omitempty"`
	Tx     *float64 `json:"tx,omitempty"`
}

type panelMetricsTotals struct {
	Mem  float64 `json:"mem,omitempty"`
	Disk float64 `json:"disk,omitempty"`
}

type panelMetricsEvent struct {
	Time int64  `json:"t"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type panelMetricsAlert struct {
	Level string `json:"level"`
	Key   string `json:"key"`
	Text  string `json:"text"`
}

// panelMetricsHistory is the file on disk. `interval` and `keep` are stored so a
// file written by an older build with a different cadence can be discarded
// instead of being mixed into the new series.
type panelMetricsHistory struct {
	Interval int                 `json:"interval"`
	Keep     int                 `json:"keep"`
	Saved    int64               `json:"saved,omitempty"`
	Totals   panelMetricsTotals  `json:"totals"`
	Points   []panelMetricPoint  `json:"points"`
	Events   []panelMetricsEvent `json:"events,omitempty"`
}

// panelMetricsSample is what the platform sampler hands back for one tick: the
// point itself plus the capacities needed to turn used megabytes into a share.
type panelMetricsSample struct {
	Point  panelMetricPoint
	Totals panelMetricsTotals
}

// panelMetrics collects the station metrics on its own ticker and serves
// aggregated views. The panel must stay useful when collection fails, so a
// sampler error never fails the HTTP handler: it just leaves fields null.
type panelMetrics struct {
	mu       sync.RWMutex
	path     string
	interval time.Duration
	keep     time.Duration
	now      func() time.Time
	sample   func(context.Context) panelMetricsSample

	points   []panelMetricPoint
	events   []panelMetricsEvent
	totals   panelMetricsTotals
	lastUp   float64
	haveUp   bool
	lastSave time.Time
	active   map[string]bool
}

func newPanelMetrics(path string) *panelMetrics {
	sampler := newProcSampler(filepath.Dir(path))
	m := &panelMetrics{
		path:     path,
		interval: panelMetricsInterval,
		keep:     panelMetricsKeep,
		now:      time.Now,
		sample:   sampler.sample,
		active:   map[string]bool{},
	}
	m.load()
	return m
}

// start samples once right away, so the panel has something to draw, and then on
// every tick until the context is cancelled. The final save keeps the last few
// minutes across a restart.
func (m *panelMetrics) start(ctx context.Context) {
	m.tick(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.save(true)
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

func (m *panelMetrics) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	sample := m.sample(ctx)
	if sample.Point.Time == 0 {
		sample.Point.Time = m.now().Unix()
	}
	m.add(sample)
}

func (m *panelMetrics) add(sample panelMetricsSample) {
	point := sample.Point
	m.mu.Lock()
	defer m.mu.Unlock()

	// A decreasing uptime means the station rebooted: the gap in the series is
	// the truth, an event explains it.
	if point.Uptime != nil {
		if m.haveUp && *point.Uptime+30 < m.lastUp {
			m.eventLocked("reboot", "станция перезагружалась")
		}
		m.lastUp, m.haveUp = *point.Uptime, true
	}
	if sample.Totals.Mem > 0 {
		m.totals.Mem = sample.Totals.Mem
	}
	if sample.Totals.Disk > 0 {
		m.totals.Disk = sample.Totals.Disk
	}
	m.points = append(m.points, point)
	m.trimLocked()
	m.alertTransitionsLocked(point)
	m.saveLocked(false)
}

func (m *panelMetrics) trimLocked() {
	cutoff := m.now().Add(-m.keep).Unix()
	drop := 0
	for drop < len(m.points) && m.points[drop].Time < cutoff {
		drop++
	}
	if drop > 0 {
		m.points = append([]panelMetricPoint(nil), m.points[drop:]...)
	}
	// A clock jump must not leave the slice unbounded.
	if limit := int(m.keep/m.interval) + 8; len(m.points) > limit {
		m.points = append([]panelMetricPoint(nil), m.points[len(m.points)-limit:]...)
	}
	if len(m.events) > panelMetricsEventKeep {
		m.events = append([]panelMetricsEvent(nil), m.events[len(m.events)-panelMetricsEventKeep:]...)
	}
}

func (m *panelMetrics) eventLocked(kind, text string) {
	m.events = append(m.events, panelMetricsEvent{Time: m.now().Unix(), Kind: kind, Text: text})
	slog.Info("panel_metrics_event", "kind", kind, "text", text)
}

// alertTransitionsLocked records the moments the station crossed a threshold.
// The alert list itself is recomputed from the latest point on every request, so
// a restart cannot leave a stale warning behind.
func (m *panelMetrics) alertTransitionsLocked(point panelMetricPoint) {
	current := map[string]bool{}
	for _, alert := range m.alertsLocked(point) {
		current[alert.Key] = true
		if !m.active[alert.Key] {
			m.eventLocked("alert", alert.Text)
		}
	}
	for key := range m.active {
		if !current[key] {
			delete(m.active, key)
			m.eventLocked("clear", "порог снят: "+key)
		}
	}
	m.active = current
}

func (m *panelMetrics) alertsLocked(point panelMetricPoint) []panelMetricsAlert {
	alerts := make([]panelMetricsAlert, 0, 5)
	if point.Temp != nil && *point.Temp >= panelMetricsMinTemp {
		level := "warn"
		if *point.Temp >= panelMetricsHotTemp {
			level = "critical"
		}
		alerts = append(alerts, panelMetricsAlert{Level: level, Key: "temp", Text: "температура " + formatMetric(*point.Temp) + " °C"})
	}
	if point.Mem != nil && m.totals.Mem > 0 && *point.Mem/m.totals.Mem >= panelMetricsMemShare {
		alerts = append(alerts, panelMetricsAlert{Level: "warn", Key: "mem", Text: "память занята на " + formatMetric(*point.Mem/m.totals.Mem*100) + "%"})
	}
	if point.Disk != nil && m.totals.Disk > 0 && *point.Disk/m.totals.Disk >= panelMetricsDiskShare {
		alerts = append(alerts, panelMetricsAlert{Level: "warn", Key: "disk", Text: "диск занят на " + formatMetric(*point.Disk/m.totals.Disk*100) + "%"})
	}
	if point.Load != nil && *point.Load >= panelMetricsLoadLimit {
		alerts = append(alerts, panelMetricsAlert{Level: "warn", Key: "load", Text: "load average " + formatMetric(*point.Load)})
	}
	if point.RSSI != nil && *point.RSSI <= panelMetricsWeakRSSI {
		alerts = append(alerts, panelMetricsAlert{Level: "warn", Key: "rssi", Text: "слабый Wi-Fi: " + formatMetric(*point.RSSI) + " dBm"})
	}
	return alerts
}

func formatMetric(value float64) string {
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64)
}

func (m *panelMetrics) latestLocked() (panelMetricPoint, bool) {
	if len(m.points) == 0 {
		return panelMetricPoint{}, false
	}
	return m.points[len(m.points)-1], true
}

// doc aggregates the series into at most panelMetricsMaxPoints buckets inside
// the requested range. Empty buckets stay null: the UI draws a gap instead of
// inventing a line through missing data.
func (m *panelMetrics) doc(rangeSeconds int) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	intervalSec := int64(m.interval / time.Second)
	if intervalSec < 1 {
		intervalSec = 1
	}
	if rangeSeconds < panelMetricsRangeMin {
		rangeSeconds = panelMetricsRangeMin
	}
	if limit := int(m.keep / time.Second); rangeSeconds > limit {
		rangeSeconds = limit
	}
	now := m.now()
	from := now.Add(-time.Duration(rangeSeconds) * time.Second)
	step := int64(rangeSeconds) / panelMetricsMaxPoints
	if step < intervalSec {
		step = intervalSec
	}
	count := rangeSeconds/int(step) + 1
	if count > panelMetricsMaxPoints {
		count = panelMetricsMaxPoints
	}

	buckets := make([][]panelMetricPoint, count)
	for _, point := range m.points {
		if point.Time < from.Unix() || point.Time > now.Unix() {
			continue
		}
		index := int((point.Time - from.Unix()) / step)
		if index < 0 {
			continue
		}
		if index >= count {
			index = count - 1
		}
		buckets[index] = append(buckets[index], point)
	}

	fields := make(map[string][]any, 8)
	for _, name := range []string{"cpu", "temp", "mem", "disk", "rssi", "load", "rx", "tx"} {
		fields[name] = make([]any, count)
	}
	times := make([]any, count)
	for i, samples := range buckets {
		times[i] = from.Unix() + int64(i)*step
		fields["cpu"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.CPU })
		fields["temp"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.Temp })
		fields["mem"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.Mem })
		fields["disk"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.Disk })
		fields["rssi"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.RSSI })
		fields["load"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.Load })
		fields["rx"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.Rx })
		fields["tx"][i] = bucketAverage(samples, func(p panelMetricPoint) *float64 { return p.Tx })
	}

	events := make([]panelMetricsEvent, len(m.events))
	copy(events, m.events)
	latest, hasLatest := m.latestLocked()
	alerts := make([]panelMetricsAlert, 0, 5)
	var uptime *float64
	if hasLatest {
		alerts = m.alertsLocked(latest)
		uptime = latest.Uptime
	}
	doc := map[string]any{
		"interval": int(panelMetricsInterval / time.Second),
		"step":     step,
		"from":     from.Unix(),
		"to":       now.Unix(),
		"range":    rangeSeconds,
		"totals":   m.totals,
		"t":        times,
		"events":   events,
		"alerts":   alerts,
		"uptime":   uptime,
		"points":   len(m.points),
	}
	for name, series := range fields {
		doc[name] = series
	}
	return doc
}

func bucketAverage(samples []panelMetricPoint, field func(panelMetricPoint) *float64) *float64 {
	if len(samples) == 0 {
		return nil
	}
	sum, count := 0.0, 0
	for _, sample := range samples {
		if value := field(sample); value != nil {
			sum += *value
			count++
		}
	}
	if count == 0 {
		return nil
	}
	value := math.Round(sum/float64(count)*10) / 10
	return &value
}

func (m *panelMetrics) saveLocked(force bool) {
	// An empty path means the series lives in memory only: unit tests and a
	// panel built without the metrics section must not touch the disk.
	if m.path == "" {
		return
	}
	if !force && m.now().Sub(m.lastSave) < panelMetricsSaveEvery {
		return
	}
	m.lastSave = m.now()
	history := panelMetricsHistory{
		Interval: int(m.interval / time.Second),
		Keep:     int(m.keep / time.Second),
		Saved:    m.now().Unix(),
		Totals:   m.totals,
		Points:   m.points,
		Events:   m.events,
	}
	raw, err := stdjson.Marshal(history)
	if err != nil {
		return
	}
	if err := writeFileAtomic(m.path, raw); err != nil {
		slog.Warn("panel_metrics_save_failed", "error", err)
	}
}

func (m *panelMetrics) save(force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveLocked(force)
}

// load reads the previous series. A file from another build, a truncated file or
// a missing one all result in an empty series: the panel is auxiliary and must
// never refuse to start because of its own cache.
func (m *panelMetrics) load() {
	if m.path == "" {
		return
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return
	}
	var history panelMetricsHistory
	if err := stdjson.Unmarshal(raw, &history); err != nil {
		slog.Warn("panel_metrics_load_failed", "error", err)
		return
	}
	if history.Interval != int(m.interval/time.Second) || history.Keep != int(m.keep/time.Second) {
		slog.Info("panel_metrics_discarded", "interval", history.Interval, "keep", history.Keep)
		return
	}
	cutoff := m.now().Add(-m.keep).Unix()
	ceiling := m.now().Add(time.Minute).Unix()
	points := make([]panelMetricPoint, 0, len(history.Points))
	for _, point := range history.Points {
		if point.Time < cutoff || point.Time > ceiling {
			continue
		}
		points = append(points, point)
	}
	m.points = points
	m.totals = history.Totals
	m.events = history.Events
	if len(m.events) > panelMetricsEventKeep {
		m.events = m.events[len(m.events)-panelMetricsEventKeep:]
	}
	if len(points) > 0 {
		if uptime := points[len(points)-1].Uptime; uptime != nil {
			m.lastUp, m.haveUp = *uptime, true
		}
	}
	slog.Info("panel_metrics_loaded", "points", len(points))
}

// apiMetrics serves the aggregated series for one range. Only aggregate numbers
// leave the process, and only to a logged-in browser.
func (p *panelServer) apiMetrics(w http.ResponseWriter, r *http.Request) {
	rangeSeconds := panelQueryInt(r, "range", 3600, panelMetricsRangeMin, panelMetricsRangeMax)
	doc := p.metrics.doc(rangeSeconds)
	doc["bridge_alerts"] = panelBridgeAlerts(p.snapshot())
	writePanelJSON(w, http.StatusOK, doc)
}

// panelBridgeAlerts turns the live counters into the same shape as the station
// alerts, so the UI has one list to render.
func panelBridgeAlerts(snapshot map[string]any) []panelMetricsAlert {
	alerts := make([]panelMetricsAlert, 0, 3)
	if snapshot == nil {
		return alerts
	}
	number := func(key string) float64 {
		switch value := snapshot[key].(type) {
		case float64:
			return value
		case int64:
			return float64(value)
		case int:
			return float64(value)
		}
		return 0
	}
	if stalls := number("upstream_stalls_total"); stalls > 0 {
		alerts = append(alerts, panelMetricsAlert{Level: "warn", Key: "upstream_stalls", Text: "мост терял кадры: " + formatMetric(stalls)})
	}
	if failures := number("upstream_ping_failures_total"); failures > 0 {
		alerts = append(alerts, panelMetricsAlert{Level: "warn", Key: "upstream_ping", Text: "неудачных ping: " + formatMetric(failures)})
	}
	if last := number("upstream_last_frame_ms"); last >= 5000 {
		alerts = append(alerts, panelMetricsAlert{Level: "warn", Key: "upstream_frame", Text: "последний кадр от облака " + formatMetric(last/1000) + " с назад"})
	}
	return alerts
}

// procSampler reads the same node files the reference panel uses. Every read is
// optional: on a machine without /proc the sampler returns a point with only a
// timestamp instead of failing the loop.
type procSampler struct {
	root     string
	diskPath string
	wifiDev  string

	read   func(string) ([]byte, error)
	statfs func(string) (usedMB, totalMB float64, err error)
	exec   func(string, ...string) ([]byte, error)
	now    func() time.Time

	prevIdle  float64
	prevTotal float64
	haveCPU   bool

	prevRx float64
	prevTx float64
	prevAt int64
	haveRx bool
}

func newProcSampler(diskPath string) *procSampler {
	return &procSampler{
		diskPath: diskPath,
		wifiDev:  "wlan0",
		read:     os.ReadFile,
		statfs:   stationDiskUsage,
		now:      time.Now,
		exec: func(name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, name, args...).Output()
		},
	}
}

func (s *procSampler) sample(ctx context.Context) panelMetricsSample {
	sample := panelMetricsSample{Point: panelMetricPoint{Time: s.now().Unix()}}
	if ctx.Err() != nil {
		return sample
	}
	point := &sample.Point
	if raw, err := s.read(s.stationPath("/proc/uptime")); err == nil {
		if value, err := firstField(string(raw)); err == nil {
			point.Uptime = &value
		}
	}
	if raw, err := s.read(s.stationPath("/proc/stat")); err == nil {
		if idle, total, ok := parseProcStat(string(raw)); ok {
			if s.haveCPU && total > s.prevTotal {
				share := 100 * (1 - (idle-s.prevIdle)/(total-s.prevTotal))
				share = math.Round(clampMetric(share, 0, 100)*10) / 10
				point.CPU = &share
			}
			s.prevIdle, s.prevTotal, s.haveCPU = idle, total, true
		}
	}
	if raw, err := s.read(s.stationPath("/proc/loadavg")); err == nil {
		if value, err := firstField(string(raw)); err == nil {
			value = math.Round(value*100) / 100
			point.Load = &value
		}
	}
	if raw, err := s.read(s.stationPath("/proc/meminfo")); err == nil {
		if used, total, ok := parseMeminfo(string(raw)); ok {
			sample.Totals.Mem = total
			point.Mem = &used
		}
	}
	if used, total, err := s.statfs(s.diskPath); err == nil {
		sample.Totals.Disk = total
		point.Disk = &used
	}
	if raw, err := s.read(s.stationPath("/sys/class/thermal/thermal_zone0/temp")); err == nil {
		if value, err := firstField(string(raw)); err == nil {
			value = math.Round(value) / 1000
			point.Temp = &value
		}
	}
	if s.wifiDev != "" {
		if raw, err := s.exec("iw", "dev", s.wifiDev, "link"); err == nil {
			if rssi, ok := parseIWSignal(string(raw)); ok {
				point.RSSI = &rssi
			}
		}
	}
	if raw, err := s.read(s.stationPath("/proc/net/dev")); err == nil {
		if rx, tx, ok := parseNetDev(string(raw), s.wifiDev); ok {
			if s.haveRx && point.Time > s.prevAt {
				perByte := 8 / float64(point.Time-s.prevAt) / 1000
				rxRate := math.Round(math.Max(0, (rx-s.prevRx)*perByte))
				txRate := math.Round(math.Max(0, (tx-s.prevTx)*perByte))
				point.Rx, point.Tx = &rxRate, &txRate
			}
			s.prevRx, s.prevTx, s.prevAt, s.haveRx = rx, tx, point.Time, true
		}
	}
	return sample
}

func (s *procSampler) stationPath(name string) string {
	if s.root == "" {
		return name
	}
	return filepath.Join(s.root, name)
}

func firstField(raw string) (float64, error) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return 0, errors.New("empty value")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func clampMetric(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// parseProcStat reads the aggregate cpu line: user nice system idle iowait irq
// softirq [steal]. Idle time is idle+iowait, the total is their sum.
func parseProcStat(raw string) (idle, total float64, ok bool) {
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		if len(fields) < 7 {
			return 0, 0, false
		}
		counter := make([]float64, 0, 8)
		for _, field := range fields[:min(len(fields), 8)] {
			value, err := strconv.ParseFloat(field, 64)
			if err != nil {
				return 0, 0, false
			}
			counter = append(counter, value)
		}
		for _, value := range counter {
			total += value
		}
		return counter[3] + counter[4], total, true
	}
	return 0, 0, false
}

// parseMeminfo returns the used megabytes and the total megabytes.
func parseMeminfo(raw string) (used, total float64, ok bool) {
	var totalKB, availableKB float64
	var haveTotal, haveAvailable bool
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, haveTotal = value, true
		case "MemAvailable:":
			availableKB, haveAvailable = value, true
		}
	}
	if !haveTotal || !haveAvailable || totalKB <= 0 {
		return 0, 0, false
	}
	return math.Round((totalKB - availableKB) / 1024), math.Round(totalKB / 1024), true
}

// parseIWSignal reads `signal: -58 dBm` from `iw dev wlan0 link`.
func parseIWSignal(raw string) (float64, bool) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "signal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

// parseNetDev returns the received and transmitted bytes of one interface.
func parseNetDev(raw, device string) (rx, tx float64, ok bool) {
	if device == "" {
		return 0, 0, false
	}
	for _, line := range strings.Split(raw, "\n") {
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		if strings.TrimSpace(line[:colon]) != device {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			return 0, 0, false
		}
		received, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0, 0, false
		}
		transmitted, err := strconv.ParseFloat(fields[8], 64)
		if err != nil {
			return 0, 0, false
		}
		return received, transmitted, true
	}
	return 0, 0, false
}

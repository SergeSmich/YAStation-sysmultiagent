package main

import (
	"context"
	stdjson "encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// panelTestMetrics builds a collector with a frozen clock and no file behind it.
func panelTestMetrics(now time.Time) *panelMetrics {
	m := newPanelMetrics("")
	m.now = func() time.Time { return now }
	return m
}

func metricSeries(t *testing.T, doc map[string]any, name string) []any {
	t.Helper()
	series, ok := doc[name].([]any)
	if !ok {
		t.Fatalf("%s is %T", name, doc[name])
	}
	return series
}

func metricAt(t *testing.T, series []any, index int) (float64, bool) {
	t.Helper()
	if index < 0 || index >= len(series) {
		t.Fatalf("index %d out of %d", index, len(series))
	}
	switch value := series[index].(type) {
	case *float64:
		// A gap is a typed nil pointer, so it has to be checked before the
		// dereference: the value is nil while the type is not.
		if value == nil {
			return 0, false
		}
		return *value, true
	case nil:
		return 0, false
	}
	t.Fatalf("unexpected value %T", series[index])
	return 0, false
}

func metricPointer(value float64) *float64 { return &value }

// TestPanelMetricsDocAggregatesAndKeepsGaps pins the two promises the charts
// rely on: no view is longer than panelMetricsMaxPoints, and a minute without
// samples stays a hole instead of being filled with a zero.
func TestPanelMetricsDocAggregatesAndKeepsGaps(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	m := panelTestMetrics(clock)
	start := clock.Add(-6 * time.Minute)
	for i := 0; i < 36; i++ {
		if i >= 12 && i < 18 {
			continue // one minute of silence
		}
		m.add(panelMetricsSample{Point: panelMetricPoint{
			Time: start.Add(time.Duration(i) * 10 * time.Second).Unix(),
			CPU:  metricPointer(float64(10 + i)),
		}})
	}

	doc := m.doc(3600)
	if doc["range"].(int) != 3600 {
		t.Fatalf("range = %v", doc["range"])
	}
	cpu := metricSeries(t, doc, "cpu")
	times := metricSeries(t, doc, "t")
	if len(cpu) != len(times) || len(cpu) != 361 {
		t.Fatalf("len(cpu)=%d len(t)=%d, want 361", len(cpu), len(times))
	}
	// from = clock-3600, so the sample written at clock-360+i*10 lands in 324+i.
	if value, ok := metricAt(t, cpu, 324); !ok || value != 10 {
		t.Fatalf("cpu[324] = %v %v, want 10", value, ok)
	}
	if value, ok := metricAt(t, cpu, 359); !ok || value != 45 {
		t.Fatalf("cpu[359] = %v %v, want 45", value, ok)
	}
	for _, index := range []int{336, 340, 341, 360, 0} {
		if value, ok := metricAt(t, cpu, index); ok {
			t.Fatalf("cpu[%d] = %v, want a gap", index, value)
		}
	}
	if value, ok := metricAt(t, cpu, 335); !ok || value != 21 {
		t.Fatalf("cpu[335] = %v %v, want 21", value, ok)
	}

	// A day is aggregated into at most 720 buckets.
	day := m.doc(86400)
	if series := metricSeries(t, day, "cpu"); len(series) != panelMetricsMaxPoints {
		t.Fatalf("24h series = %d points, want %d", len(series), panelMetricsMaxPoints)
	}
	// A range below the floor is clamped instead of producing a one-point chart.
	if got := m.doc(5)["range"].(int); got != panelMetricsRangeMin {
		t.Fatalf("range floor = %d, want %d", got, panelMetricsRangeMin)
	}
	if got := m.doc(panelMetricsRangeMax * 2)["range"].(int); got != panelMetricsRangeMax {
		t.Fatalf("range ceiling = %d, want %d", got, panelMetricsRangeMax)
	}
}

// TestPanelMetricsBucketsAverageSamples checks that a bucket holding several
// samples reports their mean and that a bucket without values is a gap even when
// the neighbouring metrics are present.
func TestPanelMetricsBucketsAverageSamples(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	m := panelTestMetrics(clock)
	for i, value := range []float64{1, 2, 3} {
		m.add(panelMetricsSample{Point: panelMetricPoint{
			Time: clock.Add(-time.Duration(i) * time.Second).Unix(),
			CPU:  metricPointer(value),
			Temp: metricPointer(40),
		}})
	}
	doc := m.doc(panelMetricsRangeMin)
	if got := doc["step"].(int64); got != 10 {
		t.Fatalf("step = %d, want 10", got)
	}
	cpu := metricSeries(t, doc, "cpu")
	last := len(cpu) - 1
	// One-second samples land twice in a ten-second bucket: the average of 2 and
	// 3 is reported for that bucket, not one of the two.
	if value, ok := metricAt(t, cpu, last); !ok || value != 1 {
		t.Fatalf("cpu[last] = %v %v, want 1", value, ok)
	}
	if value, ok := metricAt(t, cpu, last-1); !ok || value != 2.5 {
		t.Fatalf("cpu[last-1] = %v %v, want 2.5", value, ok)
	}
	mem := metricSeries(t, doc, "mem")
	for i := range mem {
		if value, ok := metricAt(t, mem, i); ok {
			t.Fatalf("mem[%d] = %v, want gap", i, value)
		}
	}
}

func TestPanelMetricsSavedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.json")
	m := panelTestMetrics(time.Now())
	m.path = path
	m.add(panelMetricsSample{Point: panelMetricPoint{Time: time.Now().Unix(), CPU: metricPointer(12.5)}, Totals: panelMetricsTotals{Mem: 1000, Disk: 2000}})
	m.save(true)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("metrics.json mode = %o, want 600", perm)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var history panelMetricsHistory
	if err := stdjson.Unmarshal(raw, &history); err != nil {
		t.Fatal(err)
	}
	if history.Interval != int(panelMetricsInterval/time.Second) || len(history.Points) != 1 {
		t.Fatalf("history = %d points, interval %d", len(history.Points), history.Interval)
	}

	reloaded := newPanelMetrics(path)
	if reloaded.totals.Mem != 1000 || reloaded.totals.Disk != 2000 {
		t.Fatalf("totals = %+v", reloaded.totals)
	}
	if len(reloaded.points) != 1 {
		t.Fatalf("points = %d, want 1", len(reloaded.points))
	}
	if value := reloaded.points[0].CPU; value == nil || *value != 12.5 {
		t.Fatalf("cpu = %v", value)
	}
}

// TestPanelMetricsDiscardsForeignFile keeps a file written by another cadence
// from being mixed into the new series.
func TestPanelMetricsDiscardsForeignFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.json")
	raw, err := stdjson.Marshal(panelMetricsHistory{
		Interval: 60,
		Keep:     3600,
		Points:   []panelMetricPoint{{Time: time.Now().Unix(), CPU: metricPointer(50)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m := newPanelMetrics(path)
	if len(m.points) != 0 {
		t.Fatalf("points = %d, want 0", len(m.points))
	}

	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := newPanelMetrics(broken); len(m.points) != 0 {
		t.Fatalf("broken file loaded %d points", len(m.points))
	}
}

// TestPanelMetricsAlertsAndEvents records the moment a threshold is crossed and
// the moment it clears, and never repeats the same warning while it lasts.
func TestPanelMetricsAlertsAndEvents(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	m := panelTestMetrics(clock)
	hot := panelMetricsSample{Point: panelMetricPoint{Time: clock.Unix(), Temp: metricPointer(80), Mem: metricPointer(900)}, Totals: panelMetricsTotals{Mem: 1000}}
	m.add(hot)
	m.add(hot)
	events := m.doc(panelMetricsRangeMin)["events"].([]panelMetricsEvent)
	var alerts, clears int
	for _, event := range events {
		switch event.Kind {
		case "alert":
			alerts++
		case "clear":
			clears++
		}
	}
	if alerts != 2 || clears != 0 {
		t.Fatalf("alerts=%d clears=%d, want 2 alerts: %+v", alerts, clears, events)
	}
	doc := m.doc(panelMetricsRangeMin)
	latest := doc["alerts"].([]panelMetricsAlert)
	if len(latest) != 2 || latest[0].Key != "temp" || latest[0].Level != "warn" {
		t.Fatalf("alerts = %+v", latest)
	}

	cool := panelMetricsSample{Point: panelMetricPoint{Time: clock.Add(time.Minute).Unix(), Temp: metricPointer(50), Mem: metricPointer(100)}, Totals: panelMetricsTotals{Mem: 1000}}
	m.add(cool)
	doc = m.doc(panelMetricsRangeMin)
	if got := doc["alerts"].([]panelMetricsAlert); len(got) != 0 {
		t.Fatalf("alerts after cooling = %+v", got)
	}
	clears = 0
	for _, event := range m.doc(panelMetricsRangeMin)["events"].([]panelMetricsEvent) {
		if event.Kind == "clear" {
			clears++
		}
	}
	if clears != 2 {
		t.Fatalf("clears = %d, want 2", clears)
	}
}

// TestPanelMetricsRebootEvent explains a gap in the uptime series.
func TestPanelMetricsRebootEvent(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	m := panelTestMetrics(clock)
	m.add(panelMetricsSample{Point: panelMetricPoint{Time: clock.Unix(), Uptime: metricPointer(5000)}})
	m.add(panelMetricsSample{Point: panelMetricPoint{Time: clock.Add(10 * time.Second).Unix(), Uptime: metricPointer(12)}})
	events := m.doc(panelMetricsRangeMin)["events"].([]panelMetricsEvent)
	if len(events) != 1 || events[0].Kind != "reboot" {
		t.Fatalf("events = %+v", events)
	}
}

func TestParseProcStat(t *testing.T) {
	idle, total, ok := parseProcStat("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4 5 6 7 8\n")
	if !ok {
		t.Fatal("parse failed")
	}
	if idle != 850 || total != 1000 {
		t.Fatalf("idle=%v total=%v, want 850/1000", idle, total)
	}
	if _, _, ok := parseProcStat("intr 1 2 3\n"); ok {
		t.Fatal("parsed a file without the cpu line")
	}
	if _, _, ok := parseProcStat("cpu  1 2 3\n"); ok {
		t.Fatal("parsed a short cpu line")
	}
}

func TestParseMeminfo(t *testing.T) {
	used, total, ok := parseMeminfo("MemTotal:       1024000 kB\nMemFree:  1 kB\nMemAvailable:    512000 kB\n")
	if !ok {
		t.Fatal("parse failed")
	}
	if used != 500 || total != 1000 {
		t.Fatalf("used=%v total=%v, want 500/1000", used, total)
	}
	if _, _, ok := parseMeminfo("MemTotal: 1000 kB\n"); ok {
		t.Fatal("accepted meminfo without MemAvailable")
	}
}

func TestParseIWSignal(t *testing.T) {
	raw := "Connected to 11:22:33:44:55:66 (on wlan0)\n\tSSID: home\n\tsignal: -58 dBm\n\ttx bitrate: 72.2 MBit/s\n"
	if value, ok := parseIWSignal(raw); !ok || value != -58 {
		t.Fatalf("signal = %v %v, want -58", value, ok)
	}
	if _, ok := parseIWSignal("Not connected.\n"); ok {
		t.Fatal("parsed a signal from a disconnected interface")
	}
}

func TestParseNetDev(t *testing.T) {
	raw := "Inter-|   Receive                                                |  Transmit\n face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n  eth0: 100 1 0 0 0 0 0 0 200 2 0 0 0 0 0 0\n wlan0: 1000 1 0 0 0 0 0 0 500 2 0 0 0 0 0 0\n"
	rx, tx, ok := parseNetDev(raw, "wlan0")
	if !ok || rx != 1000 || tx != 500 {
		t.Fatalf("rx=%v tx=%v ok=%v, want 1000/500", rx, tx, ok)
	}
	if _, _, ok := parseNetDev(raw, "wlan9"); ok {
		t.Fatal("parsed a missing interface")
	}
	if _, _, ok := parseNetDev(raw, ""); ok {
		t.Fatal("parsed an empty interface name")
	}
}

// TestProcSamplerReadsStation feeds the sampler the same files the station has,
// so the parsing and the CPU delta are covered without /proc.
func TestProcSamplerReadsStation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	stat := "cpu  100 0 50 800 50 0 0 0 0 0\n"
	netDev := "  eth0: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n wlan0: 1000 1 0 0 0 0 0 0 500 1 0 0 0 0 0 0\n"
	sampler := &procSampler{
		diskPath: "/data/opt/quasar-bridge",
		wifiDev:  "wlan0",
		now:      func() time.Time { return now },
		read: func(name string) ([]byte, error) {
			switch name {
			case "/proc/uptime":
				return []byte("12345.67 99999.00\n"), nil
			case "/proc/stat":
				return []byte(stat), nil
			case "/proc/loadavg":
				return []byte("0.42 0.51 0.48 1/123 456\n"), nil
			case "/proc/meminfo":
				return []byte("MemTotal:       1024000 kB\nMemAvailable:    512000 kB\n"), nil
			case "/sys/class/thermal/thermal_zone0/temp":
				return []byte("58700\n"), nil
			case "/proc/net/dev":
				return []byte(netDev), nil
			}
			return nil, errors.New("missing " + name)
		},
		statfs: func(string) (float64, float64, error) { return 300, 2400, nil },
		exec: func(name string, args ...string) ([]byte, error) {
			if name != "iw" || len(args) != 3 {
				return nil, errors.New("unexpected command")
			}
			return []byte("Connected to 11:22:33:44:55:66 (on wlan0)\n\tsignal: -58 dBm\n"), nil
		},
	}

	first := sampler.sample(context.Background())
	if first.Point.Uptime == nil || *first.Point.Uptime != 12345.67 {
		t.Fatalf("uptime = %v", first.Point.Uptime)
	}
	if first.Point.CPU != nil {
		t.Fatalf("first sample reported cpu %v without a previous read", *first.Point.CPU)
	}
	if first.Point.Load == nil || *first.Point.Load != 0.42 {
		t.Fatalf("load = %v", first.Point.Load)
	}
	if first.Point.Mem == nil || *first.Point.Mem != 500 {
		t.Fatalf("mem = %v", first.Point.Mem)
	}
	if first.Totals.Mem != 1000 || first.Totals.Disk != 2400 {
		t.Fatalf("totals = %+v", first.Totals)
	}
	if first.Point.Disk == nil || *first.Point.Disk != 300 {
		t.Fatalf("disk = %v", first.Point.Disk)
	}
	if first.Point.Temp == nil || *first.Point.Temp != 58.7 {
		t.Fatalf("temp = %v", first.Point.Temp)
	}
	if first.Point.RSSI == nil || *first.Point.RSSI != -58 {
		t.Fatalf("rssi = %v", first.Point.RSSI)
	}
	if first.Point.Rx != nil || first.Point.Tx != nil {
		t.Fatalf("first sample reported traffic without a previous read")
	}

	now = now.Add(10 * time.Second)
	stat = "cpu  200 0 100 1600 100 0 0 0 0 0\n"
	netDev = "  eth0: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n wlan0: 1125000 1 0 0 0 0 0 0 500 1 0 0 0 0 0 0\n"
	second := sampler.sample(context.Background())
	if second.Point.CPU == nil || *second.Point.CPU != 15 {
		t.Fatalf("cpu = %v, want 15", second.Point.CPU)
	}
	if second.Point.Rx == nil || *second.Point.Rx != 899 {
		t.Fatalf("rx = %v, want 899", second.Point.Rx)
	}
	if second.Point.Tx == nil || *second.Point.Tx != 0 {
		t.Fatalf("tx = %v, want 0", second.Point.Tx)
	}
}

func TestProcSamplerSurvivesMissingFiles(t *testing.T) {
	sampler := &procSampler{
		now:    time.Now,
		read:   func(string) ([]byte, error) { return nil, errors.New("absent") },
		statfs: func(string) (float64, float64, error) { return 0, 0, errors.New("absent") },
		exec:   func(string, ...string) ([]byte, error) { return nil, errors.New("absent") },
	}
	sample := sampler.sample(context.Background())
	if sample.Point.Time == 0 {
		t.Fatal("sample has no timestamp")
	}
	if sample.Point.CPU != nil || sample.Point.Mem != nil || sample.Point.RSSI != nil {
		t.Fatalf("missing files produced values: %+v", sample.Point)
	}
}

func TestPanelBridgeAlerts(t *testing.T) {
	snapshot := map[string]any{
		"upstream_stalls_total":        int64(2),
		"upstream_ping_failures_total": 0,
		"upstream_last_frame_ms":       int64(12000),
	}
	alerts := panelBridgeAlerts(snapshot)
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v", alerts)
	}
	if alerts[0].Key != "upstream_stalls" || alerts[1].Key != "upstream_frame" {
		t.Fatalf("alerts = %+v", alerts)
	}
	if got := panelBridgeAlerts(nil); len(got) != 0 {
		t.Fatalf("nil snapshot = %+v", got)
	}
	healthy := map[string]any{"upstream_stalls_total": 0, "upstream_ping_failures_total": 0, "upstream_last_frame_ms": 400}
	if got := panelBridgeAlerts(healthy); len(got) != 0 {
		t.Fatalf("healthy snapshot = %+v", got)
	}
}

// TestPanelMetricsEndpoint checks the route the UI calls: a session gets the
// aggregate document, and the bridge counters arrive in the same alert list.
func TestPanelMetricsEndpoint(t *testing.T) {
	panel, _, _ := panelTestServer(t)
	cookie, _ := panelLogin(t, panel)

	request := httptest.NewRequest(http.MethodGet, "/api/panel/metrics?range=900", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics = %d %s", recorder.Code, recorder.Body.String())
	}
	var doc map[string]any
	if err := stdjson.Unmarshal(recorder.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"interval", "step", "from", "to", "range", "totals", "t", "cpu", "temp", "mem", "disk", "rssi", "load", "rx", "tx", "events", "alerts", "bridge_alerts"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("metrics lacks %s: %s", key, recorder.Body.String())
		}
	}
	if got := doc["range"].(float64); got != 900 {
		t.Fatalf("range = %v, want 900", got)
	}
	if series, ok := doc["cpu"].([]any); !ok || len(series) != 91 {
		t.Fatalf("cpu series = %T len %d, want 91", doc["cpu"], len(series))
	}
}

// TestPanelMetricsRejectsBadRange keeps a hand-written query from asking for a
// range the collector cannot serve.
func TestPanelMetricsRejectsBadRange(t *testing.T) {
	panel, _, _ := panelTestServer(t)
	cookie, _ := panelLogin(t, panel)

	request := httptest.NewRequest(http.MethodGet, "/api/panel/metrics?range=1", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	panel.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "\"range\":300") {
		t.Fatalf("range was not clamped: %s", recorder.Body.String())
	}
}

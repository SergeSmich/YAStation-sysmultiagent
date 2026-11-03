package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"quasarbridge/internal/sysadmin"
)

// The station remembers which processes appeared after each program it started,
// so «убей это», «убей последнее» and «и похожие» close the right ones.

const (
	maxLaunchRecs = 10
	launchRecTTL  = 6 * time.Hour
	launchSettle  = 50 * time.Second // the snapshot of processes lands about this long after a start
)

type launchRec struct {
	ID     int
	Host   string
	Target string
	At     time.Time
	Procs  []sysadmin.LaunchedProc
}

type launchedLog struct {
	mu   sync.Mutex
	seq  int
	recs []launchRec
}

func (l *launchedLog) add(host, target string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.recs = append(l.recs, launchRec{ID: l.seq, Host: host, Target: target, At: time.Now()})
	if len(l.recs) > maxLaunchRecs {
		l.recs = l.recs[len(l.recs)-maxLaunchRecs:]
	}
	return l.seq
}

func (l *launchedLog) setProcs(id int, procs []sysadmin.LaunchedProc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.recs {
		if l.recs[i].ID == id {
			l.recs[i].Procs = procs
		}
	}
}

// watch records a start and, a few seconds later, which processes came up.
func (l *launchedLog) watch(h *sysadminHub, host, target string) {
	id := l.add(host, target)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		res, err := h.Exec().Exec(ctx, host, sysadmin.LaunchedProcsPS(45), 30*time.Second)
		if err != nil {
			slog.Warn("sysadmin_launched_snapshot_failed", "err", err)
			return
		}
		procs := sysadmin.ParseLaunchedProcs(res.Stdout)
		l.setProcs(id, procs)
		slog.Info("sysadmin_launched", "host", host, "target", target, "procs", len(procs))
	}()
}

func (l *launchedLog) snapshot(host string) []launchRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []launchRec
	for _, r := range l.recs {
		if r.Host == host && time.Since(r.At) < launchRecTTL {
			out = append(out, r)
		}
	}
	return out
}

func (l *launchedLog) forget(host string, names map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.recs {
		if l.recs[i].Host != host {
			continue
		}
		var keep []sysadmin.LaunchedProc
		for _, p := range l.recs[i].Procs {
			if !names[p.Name] {
				keep = append(keep, p)
			}
		}
		l.recs[i].Procs = keep
	}
}

func launchedView(recs []launchRec) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		var procs []map[string]any
		for _, p := range r.Procs {
			procs = append(procs, map[string]any{"pid": p.PID, "name": p.Name, "path": p.Path})
		}
		out = append(out, map[string]any{"target": r.Target, "at": r.At.Format("15:04:05"), "processes": procs})
	}
	return out
}

// stopLaunched closes programs the station started. what: last | all | list | a name.
func (s *sysadminToolset) stopLaunched(ctx context.Context, origin, host, what string) (any, error) {
	if err := s.hostExists(host); err != nil {
		return nil, err
	}
	if !s.hub.HostIsWindows(host) {
		return nil, errors.New("работает только на хостах с Windows")
	}
	recs := s.hub.launched.snapshot(host)
	what = strings.ToLower(strings.TrimSpace(what))
	if what == "list" {
		return map[string]any{"launched": launchedView(recs)}, nil
	}
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	switch what {
	case "", "last":
		for i := len(recs) - 1; i >= 0 && len(names) == 0; i-- {
			if len(recs[i].Procs) == 0 && time.Since(recs[i].At) < launchSettle {
				return nil, errors.New("запуск был только что, процессы ещё определяются: повтори через несколько секунд или назови имя процесса")
			}
			for _, p := range recs[i].Procs {
				add(p.Name)
			}
		}
	case "all":
		for _, r := range recs {
			for _, p := range r.Procs {
				add(p.Name)
			}
		}
	default:
		q := strings.TrimSuffix(what, ".exe")
		for _, r := range recs {
			for _, p := range r.Procs {
				if strings.Contains(p.Name, q) || strings.Contains(strings.ToLower(p.Path), q) || strings.Contains(strings.ToLower(r.Target), q) {
					add(p.Name)
				}
			}
		}
		if len(names) == 0 {
			if n, err := sysadmin.NormProcName(q); err == nil {
				add(n)
			}
		}
	}
	if len(names) == 0 {
		return nil, errors.New("не помню запущенных процессов: найди процесс через Get-Process по маске имени и закрой по имени")
	}
	var results []any
	var stopped []string
	for _, n := range names {
		cmd, err := sysadmin.StopProcessPS(n)
		if err != nil {
			results = append(results, map[string]any{"name": n, "error": err.Error()})
			continue
		}
		r, err := s.execCommand(ctx, origin, host, cmd, 20*time.Second)
		if err != nil {
			results = append(results, map[string]any{"name": n, "error": err.Error()})
			continue
		}
		stopped = append(stopped, n)
		results = append(results, map[string]any{"name": n, "result": r})
	}
	done := map[string]bool{}
	for _, n := range stopped {
		done[n] = true
	}
	s.hub.launched.forget(host, done)
	return map[string]any{"stopped": stopped, "results": results}, nil
}

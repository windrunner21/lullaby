package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// watcher samples a process and its children from the voice server's own
// process, so the sampling costs neither engine anything. Drivers mark what
// the engine is doing; samples and CPU time are kept by state.
type watcher struct {
	pid int

	mu    sync.Mutex
	state string
	pss   map[string][]float64
	cpu   map[string]time.Duration
	wall  map[string]time.Duration
}

var (
	watchersMu sync.Mutex
	watchers   = map[int]*watcher{}
)

func treeCPU(pid int) time.Duration {
	own, waited := ticks(pid)
	total := own + waited
	for _, c := range childrenOf(pid) {
		o, _ := ticks(c)
		total += o
	}
	return total
}

func treePSS(pid int) float64 {
	total := pss(pid)
	for _, c := range childrenOf(pid) {
		total += pss(c)
	}
	return total
}

func (w *watcher) run() {
	lastCPU, lastAt := treeCPU(w.pid), time.Now()
	for range time.Tick(250 * time.Millisecond) {
		cpu, at, mem := treeCPU(w.pid), time.Now(), treePSS(w.pid)
		w.mu.Lock()
		if w.state != "" {
			w.cpu[w.state] += cpu - lastCPU
			w.wall[w.state] += at.Sub(lastAt)
			w.pss[w.state] = append(w.pss[w.state], mem)
		}
		w.mu.Unlock()
		lastCPU, lastAt = cpu, at
	}
}

// mark says what the engine with pid is doing from now on: "idle",
// "playing", "after" (just after a song, while its ffmpeg goes) or "" for
// times that don't count.
func mark(w http.ResponseWriter, r *http.Request) {
	pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
	watchersMu.Lock()
	wt, ok := watchers[pid]
	if !ok {
		wt = &watcher{pid: pid, pss: map[string][]float64{}, cpu: map[string]time.Duration{}, wall: map[string]time.Duration{}}
		watchers[pid] = wt
		go wt.run()
	}
	watchersMu.Unlock()
	wt.mu.Lock()
	wt.state = r.URL.Query().Get("state")
	wt.mu.Unlock()
}

func usageOf(w http.ResponseWriter, r *http.Request) {
	pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
	watchersMu.Lock()
	wt := watchers[pid]
	watchersMu.Unlock()
	if wt == nil {
		http.NotFound(w, r)
		return
	}
	wt.mu.Lock()
	defer wt.mu.Unlock()
	stat := func(s []float64) (mean, peak float64) {
		for _, v := range s {
			mean += v
			peak = max(peak, v)
		}
		return mean / float64(max(len(s), 1)), peak
	}
	idleMean, _ := stat(wt.pss["idle"])
	playMean, playPeak := stat(wt.pss["playing"])
	percent := func(states ...string) float64 {
		var cpu, wall time.Duration
		for _, s := range states {
			cpu += wt.cpu[s]
			wall += wt.wall[s]
		}
		if wall == 0 {
			return 0
		}
		return 100 * cpu.Seconds() / wall.Seconds()
	}
	json.NewEncoder(w).Encode(map[string]any{
		"idleMB": idleMean, "playingMeanMB": playMean, "playingPeakMB": playPeak,
		"idleCPUPercent": percent("idle"), "playingCPUPercent": percent("playing", "after"),
	})
}

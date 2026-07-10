package handlers

import (
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// Debug reports this process's own runtime/resource usage (not the whole OS).
type Debug struct {
	version string
	started time.Time
	proc    *process.Process
	mu      sync.Mutex
}

func NewDebug(version string, started time.Time) *Debug {
	p, _ := process.NewProcess(int32(os.Getpid()))
	if p != nil {
		_, _ = p.Percent(0) // prime the CPU baseline so the first read is real
	}
	return &Debug{version: version, started: started, proc: p}
}

func (h *Debug) Get(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	var gc debug.GCStats
	debug.ReadGCStats(&gc)

	cpuPct := 0.0
	var rss uint64
	if h.proc != nil {
		h.mu.Lock()
		if v, err := h.proc.Percent(0); err == nil {
			cpuPct = v
		}
		if mi, err := h.proc.MemoryInfo(); err == nil && mi != nil {
			rss = mi.RSS
		}
		h.mu.Unlock()
	}

	lastGC := ""
	if !gc.LastGC.IsZero() {
		lastGC = gc.LastGC.Format(time.RFC3339)
	}

	writeData(w, map[string]any{
		"process": map[string]any{
			"cpu_percent": round1(cpuPct),
			"rss":         rss,
			"pid":         os.Getpid(),
		},
		"memory": map[string]any{
			"heap_alloc":   m.HeapAlloc,
			"heap_sys":     m.HeapSys,
			"heap_objects": m.HeapObjects,
			"stack_inuse":  m.StackInuse,
			"sys":          m.Sys,
			"total_alloc":  m.TotalAlloc,
			"live_objects": m.Mallocs - m.Frees,
		},
		"gc": map[string]any{
			"num_gc":          m.NumGC,
			"gc_cpu_fraction": m.GCCPUFraction,
			"last_gc":         lastGC,
			"next_gc_target":  m.NextGC,
			"pause_total_ns":  m.PauseTotalNs,
		},
		"goroutines": runtime.NumGoroutine(),
		"build": map[string]any{
			"version":    h.version,
			"go_version": runtime.Version(),
			"os":         runtime.GOOS,
			"arch":       runtime.GOARCH,
			"num_cpu":    runtime.NumCPU(),
			"max_procs":  runtime.GOMAXPROCS(0),
		},
		"uptime_seconds": int64(time.Since(h.started).Seconds()),
		"now":            time.Now().Format(time.RFC3339),
	})
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

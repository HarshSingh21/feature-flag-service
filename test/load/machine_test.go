package load

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Honesty requirements, docs/05-consistency-and-e2e.md §3.4.
//
// "A benchmark without a machine is a number without units." Every reporting
// entry point prints this block before any measurement.
// ---------------------------------------------------------------------------

// machineInfo describes the box the numbers came from.
type machineInfo struct {
	CPU        string
	NumCPU     int
	GOMAXPROCS int
	GoVer      string
	OSArch     string
	Race       bool
}

func describeMachine() machineInfo {
	return machineInfo{
		CPU:        cpuModel(),
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		GoVer:      runtime.Version(),
		OSArch:     runtime.GOOS + "/" + runtime.GOARCH,
		Race:       raceEnabled,
	}
}

// cpuModel degrades gracefully: darwin via sysctl, linux via /proc/cpuinfo,
// anything else reports "unknown". A missing CPU model must never fail a
// benchmark — it is context, not a measurement.
func cpuModel() string {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				return s
			}
		}
	case "linux":
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				name, val, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				switch strings.TrimSpace(name) {
				case "model name", "Model", "cpu model":
					if s := strings.TrimSpace(val); s != "" {
						return s
					}
				}
			}
		}
	}
	return "unknown"
}

// reportMachine prints the machine block plus the standing caveat that a
// single-box measurement EXTRAPOLATES to fleet capacity rather than measuring
// one.
func reportMachine(t *testing.T) {
	m := describeMachine()
	t.Logf("")
	t.Logf("machine ----------------------------------------------------------")
	t.Logf("  cpu           %s", m.CPU)
	t.Logf("  NumCPU        %d   (GOMAXPROCS %d)", m.NumCPU, m.GOMAXPROCS)
	t.Logf("  go            %s", m.GoVer)
	t.Logf("  platform      %s", m.OSArch)
	t.Logf("  race detector %t", m.Race)
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		t.Logf("  module        %s %s", bi.Main.Path, bi.Main.Version)
	}
	t.Logf("  NOTE: these are SINGLE-MACHINE numbers. Fleet capacity figures below")
	t.Logf("        are EXTRAPOLATED from this box by linear scaling. Nothing here")
	t.Logf("        measures a fleet, and linear scaling is an assumption, not a")
	t.Logf("        result: it ignores per-pod GOMAXPROCS limits, noisy neighbours")
	t.Logf("        and the fact that a real pod spends most of its cores on the")
	t.Logf("        application, not on flag evaluation.")
	t.Logf("")
}

// ---------------------------------------------------------------------------
// Sinks. A benchmark whose result is unused can be eliminated wholesale by the
// compiler and will report an impossibly fast number. Every scenario feeds its
// result into one of these package-level variables.
// ---------------------------------------------------------------------------

var (
	sinkBool   bool
	sinkInt64  int64
	sinkString string
	sinkAny    any
)

// ---------------------------------------------------------------------------
// GC accounting, for L4.
// ---------------------------------------------------------------------------

type gcStats struct {
	NumGC      uint32
	TotalPause time.Duration
	MaxPause   time.Duration
	Truncated  bool // more than 256 collections: PauseNs wrapped
}

func readMemStats() *runtime.MemStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &ms
}

// gcBetween summarises the collections that happened between two MemStats
// reads. runtime.MemStats.PauseNs is a 256-entry circular buffer, so a run with
// more than 256 collections can only report the most recent 256 — which is
// stated rather than silently ignored.
func gcBetween(before, after *runtime.MemStats) gcStats {
	g := gcStats{
		NumGC:      after.NumGC - before.NumGC,
		TotalPause: time.Duration(after.PauseTotalNs - before.PauseTotalNs),
	}
	first := before.NumGC
	if after.NumGC-first > 256 {
		first = after.NumGC - 256
		g.Truncated = true
	}
	for i := first; i < after.NumGC; i++ {
		if p := time.Duration(after.PauseNs[i%256]); p > g.MaxPause {
			g.MaxPause = p
		}
	}
	return g
}

func (g gcStats) String() string {
	s := fmt.Sprintf("NumGC=%d totalPause=%s maxPause=%s", g.NumGC, g.TotalPause.Round(time.Microsecond), g.MaxPause.Round(time.Microsecond))
	if g.Truncated {
		s += " (pause buffer wrapped; max is over the last 256 collections)"
	}
	return s
}

// ---------------------------------------------------------------------------
// Heap measurement, for L6.
// ---------------------------------------------------------------------------

// liveHeap reports HeapAlloc after the heap has settled.
//
// The GC count is not superstition and it is not tunable. runtime.GC() returns
// once the mark phase is done, but SWEEPING is lazy — HeapAlloc still counts
// objects that were proven dead and not yet reclaimed. The next GC finishes the
// previous one's sweep. Three collections with no allocation in between leave
// nothing unswept, so the reading is the live set.
//
// The symmetry matters more than the count. Measuring "before" with two
// collections and "after" with one under-reported a 5,000-flag snapshot by 76%
// during development, because the before-reading was inflated by unswept
// garbage from the previous scenario and the after-reading was not.
func liveHeap() uint64 {
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// heapDelta reports the HeapAlloc growth caused by building and RETAINING
// whatever build returns.
//
// This measures RETAINED HEAP, not RSS. It excludes the Go runtime's own
// overhead, heap fragmentation, spans returned to but not released by the OS,
// goroutine stacks and any off-heap mapping. Real resident set for a pod will
// be larger — this is the number to compare against a "snapshot costs ~N MB"
// design claim, not the number to size a container limit from.
func heapDelta(build func() any) uint64 {
	sinkAny = nil
	before := liveHeap()

	v := build()
	sinkAny = v

	after := liveHeap()
	runtime.KeepAlive(v)

	if after < before {
		return 0
	}
	return after - before
}

func mib(b uint64) float64 { return float64(b) / (1024 * 1024) }

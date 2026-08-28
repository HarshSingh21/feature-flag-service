package load

import (
	"flag"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Why this exists at all.
//
// testing.B reports a MEAN (ns/op). P1 and P3 are p99 criteria and a mean
// cannot be turned into one — a benchmark that averages 30 µs is equally
// consistent with a flat 30 µs and with 99% at 5 µs plus a 2.5 ms tail. So the
// percentile scenarios run here instead: a fixed-duration load with every
// operation's latency recorded, then sorted.
//
// The cost of that honesty is two time.Now() calls inside the timed region,
// roughly 30-60 ns on this class of machine. On a ~30 µs batch that is under
// 0.2% and it inflates rather than flatters the number. It is reported, not
// subtracted: subtracting a constant you estimated is how a benchmark starts
// measuring its own assumptions.
// ---------------------------------------------------------------------------

var (
	loadDuration = flag.Duration("load.duration", 2*time.Second,
		"wall-clock duration of each percentile scenario (L2-L5)")
	churnInterval = flag.Duration("load.churn", 100*time.Millisecond,
		"L4 config swap interval")
)

// scenarioDuration honours -short so that `go test ./...` stays fast while a
// deliberate run gets statistically useful sample counts.
func scenarioDuration() time.Duration {
	if testing.Short() {
		return 250 * time.Millisecond
	}
	return *loadDuration
}

// latencyRun is one completed fixed-duration load scenario.
type latencyRun struct {
	Name       string
	Workers    int
	Ops        int64
	EvalsPerOp int
	Elapsed    time.Duration
	Samples    []int64 // sorted, nanoseconds
	GC         gcStats
	Churns     int64 // config swaps applied during the run, L4 only
	FinalGen   int64 // client generation at the end of the run, L4 only
}

func (r latencyRun) pct(q float64) time.Duration {
	if len(r.Samples) == 0 {
		return 0
	}
	// Nearest-rank. With 10^5-10^6 samples the choice of interpolation rule is
	// far below the run-to-run noise, so the simplest defensible rule wins.
	i := int(q * float64(len(r.Samples)))
	if i >= len(r.Samples) {
		i = len(r.Samples) - 1
	}
	if i < 0 {
		i = 0
	}
	return time.Duration(r.Samples[i])
}

func (r latencyRun) p50() time.Duration  { return r.pct(0.50) }
func (r latencyRun) p95() time.Duration  { return r.pct(0.95) }
func (r latencyRun) p99() time.Duration  { return r.pct(0.99) }
func (r latencyRun) p999() time.Duration { return r.pct(0.999) }
func (r latencyRun) max() time.Duration {
	if len(r.Samples) == 0 {
		return 0
	}
	return time.Duration(r.Samples[len(r.Samples)-1])
}

func (r latencyRun) mean() time.Duration {
	if len(r.Samples) == 0 {
		return 0
	}
	var sum int64
	for _, v := range r.Samples {
		sum += v
	}
	return time.Duration(sum / int64(len(r.Samples)))
}

// opsPerSec is requests (batches, or single evaluations) per second.
func (r latencyRun) opsPerSec() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Ops) / r.Elapsed.Seconds()
}

// evalsPerSec is the unit that actually matters. docs/03-lld.md §1: "the unit
// of load is evaluations, not requests — this is the number most capacity plans
// get wrong."
func (r latencyRun) evalsPerSec() float64 { return r.opsPerSec() * float64(r.EvalsPerOp) }

// nsPerEval is the per-flag cost hidden inside a batch's ns/op.
func (r latencyRun) nsPerEval() float64 {
	if r.EvalsPerOp == 0 {
		return 0
	}
	return float64(r.mean()) / float64(r.EvalsPerOp)
}

func (r latencyRun) report(t *testing.T) {
	t.Logf("%s", r.Name)
	t.Logf("  workers          %d", r.Workers)
	t.Logf("  duration         %s", r.Elapsed.Round(time.Millisecond))
	t.Logf("  iterations       %d ops (%d samples, %d evaluations/op)", r.Ops, len(r.Samples), r.EvalsPerOp)
	t.Logf("  latency  p50     %s", fmtDur(r.p50()))
	t.Logf("           p95     %s", fmtDur(r.p95()))
	t.Logf("           p99     %s", fmtDur(r.p99()))
	t.Logf("           p999    %s", fmtDur(r.p999()))
	t.Logf("           max     %s", fmtDur(r.max()))
	t.Logf("           mean    %s", fmtDur(r.mean()))
	t.Logf("  throughput       %s ops/sec = %s evaluations/sec", human(r.opsPerSec()), human(r.evalsPerSec()))
	t.Logf("  per evaluation   %.0f ns (mean)", r.nsPerEval())
	t.Logf("  gc               %s", r.GC)
	if r.Churns > 0 {
		t.Logf("  config swaps     %d pushed (every %s), client ended at generation %d",
			r.Churns, *churnInterval, r.FinalGen)
	}
	t.Logf("")
}

func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Millisecond:
		return fmt.Sprintf("%.3f ms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.2f µs", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%d ns", d.Nanoseconds())
	}
}

func human(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// runLoad drives workers goroutines for d, recording every operation's latency.
//
// mkOp is called ONCE PER WORKER and returns that worker's operation closure.
// Per-worker rather than shared so a worker can own its own result buffer:
// sharing one would measure cache-line ping-pong between cores instead of the
// read path, which is the classic way a "does it contend?" benchmark answers
// its own question wrongly.
func runLoad(name string, workers int, d time.Duration, evalsPerOp int, mkOp func(worker int) func()) latencyRun {
	if workers < 1 {
		workers = 1
	}

	// Warm up outside the timed region: first-touch page faults, map bucket
	// promotion and branch predictors all belong to setup, not to the design.
	warm := mkOp(0)
	for i := 0; i < 1000; i++ {
		warm()
	}
	runtime.GC()

	perWorker := make([][]int64, workers)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Estimated capacity keeps append out of the timed region for the common
	// case. It is only an estimate; a grow is a few microseconds and shows up
	// in the tail honestly rather than being hidden.
	const estCap = 1 << 17

	gcBefore := readMemStats()
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			op := mkOp(w)
			buf := make([]int64, 0, estCap)
			for {
				select {
				case <-stop:
					perWorker[w] = buf
					return
				default:
				}
				// Batched between stop checks so the channel receive is not
				// itself a measurable share of a microsecond-scale operation.
				for i := 0; i < 32; i++ {
					t0 := time.Now()
					op()
					buf = append(buf, int64(time.Since(t0)))
				}
			}
		}(w)
	}

	timer := time.NewTimer(d)
	<-timer.C
	close(stop)
	wg.Wait()
	elapsed := time.Since(start)
	gcAfter := readMemStats()

	total := 0
	for _, b := range perWorker {
		total += len(b)
	}
	all := make([]int64, 0, total)
	for _, b := range perWorker {
		all = append(all, b...)
	}
	slices.Sort(all)

	return latencyRun{
		Name:       name,
		Workers:    workers,
		Ops:        int64(len(all)),
		EvalsPerOp: evalsPerOp,
		Elapsed:    elapsed,
		Samples:    all,
		GC:         gcBetween(gcBefore, gcAfter),
	}
}

// timerOverhead measures what two time.Now() calls around an empty operation
// cost on this machine, so the latency numbers can be read with the instrument
// error visible instead of assumed away.
func timerOverhead() time.Duration {
	const n = 200000
	var total int64
	for i := 0; i < n; i++ {
		t0 := time.Now()
		total += int64(time.Since(t0))
	}
	sinkInt64 += total
	return time.Duration(total / n)
}

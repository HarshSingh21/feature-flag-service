package load

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pass criteria, docs/05-consistency-and-e2e.md §3.3.
//
// "A failing pass criterion is a finding, not a test to relax. If P1 fails, the
// sub-millisecond claim in the design is wrong and the design changes — the
// number does not get quietly restated."
//
// So this test FAILS when a criterion is missed. Nothing here is tuned to pass;
// every threshold is copied from the spec table.
// ---------------------------------------------------------------------------

type criterion struct {
	ID      string
	Claim   string
	Target  string
	Actual  string
	Pass    bool
	Skipped bool
	Note    string
}

func (c criterion) verdict() string {
	switch {
	case c.Skipped:
		return "SKIP"
	case c.Pass:
		return "PASS"
	default:
		return "FAIL"
	}
}

func reportCriteria(t *testing.T, cs []criterion) {
	t.Logf("")
	t.Logf("pass criteria (docs/05-consistency-and-e2e.md §3.3) ---------------")
	t.Logf("  %-3s %-4s %-38s %-22s %s", "ID", "", "criterion", "target", "measured")
	t.Logf("  %s", strings.Repeat("-", 100))
	for _, c := range cs {
		t.Logf("  %-3s %-4s %-38s %-22s %s", c.ID, c.verdict(), c.Claim, c.Target, c.Actual)
		if c.Note != "" {
			t.Logf("      %s", c.Note)
		}
	}
	t.Logf("  %s", strings.Repeat("-", 100))
}

// TestPassCriteria is the whole suite in one runnable unit: it drives every
// scenario, prints the machine, prints each scenario's metrics, then evaluates
// P1-P5 and fails on any miss.
//
//	go test ./test/load/ -run TestPassCriteria -v
//	go test ./test/load/ -run TestPassCriteria -v -load.duration=10s
func TestPassCriteria(t *testing.T) {
	if raceEnabled {
		t.Skip("skipped under -race: every criterion here is a latency, a " +
			"throughput or an allocation count, and a race-instrumented binary " +
			"reports ThreadSanitizer's numbers rather than the design's. Run " +
			"without -race.")
	}

	reportMachine(t)
	d := scenarioDuration()
	t.Logf("scenario duration  %s per percentile scenario (-load.duration)", d)
	t.Logf("churn interval     %s (-load.churn)", *churnInterval)
	t.Logf("timer overhead     %s per measured operation (two time.Now calls,", fmtDur(timerOverhead()))
	t.Logf("                   included in every latency below, never subtracted)")
	t.Logf("")

	// --- L1 -------------------------------------------------------------
	plain := l1Alloc(t)
	rollout := l1AllocRollout(t)
	t.Logf("L1  single BoolValue on a cached snapshot")
	t.Logf("  plain flag       %.1f ns/op   %d allocs/op   %d B/op   (%d iterations)",
		plain.NsPerOp, plain.AllocsPerOp, plain.BytesPerOp, plain.Iterations)
	t.Logf("  rollout flag     %.1f ns/op   %d allocs/op   %d B/op   (%d iterations)",
		rollout.NsPerOp, rollout.AllocsPerOp, rollout.BytesPerOp, rollout.Iterations)
	batch := l2BatchAllocs(t)
	t.Logf("  batch of 100     %.1f ns/op   %d allocs/op   %d B/op   (%d iterations, BatchAppend)",
		batch.NsPerOp, batch.AllocsPerOp, batch.BytesPerOp, batch.Iterations)
	t.Logf("")

	// --- L2, L3, L4, L5 --------------------------------------------------
	l2 := scenarioL2(t, d)
	l2.report(t)

	l3 := scenarioL3(t, d)
	l3.report(t)

	l4 := scenarioL4(t, d)
	l4.report(t)

	l5 := scenarioL5(t, d)
	l5.report(t)

	// --- L6 --------------------------------------------------------------
	sizes := []int{1000, 5000, 20000}
	if testing.Short() {
		sizes = []int{1000, 5000}
	}
	points := scenarioL6(t, sizes)
	reportL6(t, points)

	// --- extrapolation ---------------------------------------------------
	reportExtrapolation(t, l2, l3, l5)
	reportAllocationPressure(t, batch, l3)

	// --- criteria --------------------------------------------------------
	var cs []criterion

	// P1 — L2 p99 under 1 ms.
	p1 := l2.p99() < time.Millisecond
	cs = append(cs, criterion{
		ID: "P1", Claim: "L2 p99 latency, batch of 100",
		Target: "< 1.000 ms", Actual: fmtDur(l2.p99()), Pass: p1,
		Note: fmt.Sprintf("margin %.0fx; p999 %s, max %s over %d samples",
			float64(time.Millisecond)/float64(max64(l2.p99(), 1)), fmtDur(l2.p999()), fmtDur(l2.max()), len(l2.Samples)),
	})

	// P2 — L1 allocations per op = 0, on the read path's floor.
	p2 := plain.AllocsPerOp == 0
	cs = append(cs, criterion{
		ID: "P2", Claim: "L1 allocations/op, plain flag",
		Target: "= 0", Actual: fmt.Sprintf("%d allocs/op", plain.AllocsPerOp), Pass: p2,
		Note: fmt.Sprintf("rollout flag on the same path: %d allocs/op, %d B/op. A batch of 100 "+
			"from the realistic corpus: %d allocs/op, %d B/op. NOT covered by P2 as written, "+
			"but see the allocation-pressure block above",
			rollout.AllocsPerOp, rollout.BytesPerOp, batch.AllocsPerOp, batch.BytesPerOp),
	})

	// P3 — L4 p99 within 20% of L3 p99.
	limit := time.Duration(float64(l3.p99()) * 1.20)
	p3 := l4.p99() <= limit
	deltaPct := 0.0
	if l3.p99() > 0 {
		deltaPct = (float64(l4.p99())/float64(l3.p99()) - 1) * 100
	}
	cs = append(cs, criterion{
		ID: "P3", Claim: "L4 p99 vs L3 p99 (swap not a latency event)",
		Target: fmt.Sprintf("<= %s (+20%%)", fmtDur(limit)),
		Actual: fmt.Sprintf("%s (%+.1f%%)", fmtDur(l4.p99()), deltaPct), Pass: p3,
		Note: fmt.Sprintf("L3 gc %s | L4 gc %s | %d swaps applied, client reached generation %d",
			l3.GC, l4.GC, l4.Churns, l4.FinalGen),
	})

	// P4 — L3 throughput extrapolates to the 2.4M evaluations/sec peak.
	//
	// Read strictly: does the measured read path reach the fleet's whole peak?
	// One box clearing 2.4M/sec on its own means the fleet figure needs no
	// extrapolation at all to be safe, which is a stronger statement than
	// "40 pods together could".
	achieved := l3.evalsPerSec()
	p4 := achieved >= peakEvalsPerSec
	cs = append(cs, criterion{
		ID: "P4", Claim: "L3 achieved evaluations/sec",
		Target: fmt.Sprintf(">= %s", human(peakEvalsPerSec)),
		Actual: fmt.Sprintf("%s evals/sec on %d cores", human(achieved), l3.Workers), Pass: p4,
		Note: fmt.Sprintf("SINGLE MACHINE. %.2fx the entire fleet peak on one box; the fleet "+
			"figure is extrapolated, not measured", achieved/peakEvalsPerSec),
	})

	// P5 — L6 at 5k flags under 10 MB. Evaluated against the LARGER of the two
	// snapshot representations, because both are "the snapshot at 5k flags" and
	// picking the smaller would be choosing the number that flatters the claim.
	var p5Point memPoint
	for _, p := range points {
		if p.Flags == 5000 {
			p5Point = p
		}
	}
	worst := p5Point.MemSnap
	which := "client MemSnapshot"
	if p5Point.Resolved > worst {
		worst = p5Point.Resolved
		which = "service ResolvedSnapshot"
	}
	p5 := worst < 10*1024*1024
	cs = append(cs, criterion{
		ID: "P5", Claim: "L6 snapshot heap at 5,000 flags",
		Target: "< 10.00 MB", Actual: fmt.Sprintf("%.2f MB (%s)", mib(worst), which), Pass: p5,
		Note: fmt.Sprintf("client MemSnapshot %.2f MB | service ResolvedSnapshot %.2f MB | "+
			"design claim ~6 MB; retained heap, not RSS",
			mib(p5Point.MemSnap), mib(p5Point.Resolved)),
	})

	reportCriteria(t, cs)

	for _, c := range cs {
		if !c.Pass && !c.Skipped {
			t.Errorf("%s FAILED: %s — target %s, measured %s. This is a finding about "+
				"the design, not a test to relax.", c.ID, c.Claim, c.Target, c.Actual)
		}
	}
}

func max64(d time.Duration, floor time.Duration) time.Duration {
	if d < floor {
		return floor
	}
	return d
}

// reportExtrapolation restates the design's capacity arithmetic against the
// measured per-evaluation costs, so the reader can see which of the LLD's
// numbers moved and in which direction.
func reportExtrapolation(t *testing.T, l2, l3, l5 latencyRun) {
	typicalNs := l2.nsPerEval()
	worstNs := l5.nsPerEval()

	t.Logf("capacity extrapolation (docs/03-lld.md §4.1) ---------------------")
	t.Logf("  measured per-evaluation cost, typical corpus   %.3f µs   (design says ~0.3 µs)", typicalNs/1000)
	t.Logf("  measured per-evaluation cost, worst-case flag   %.3f µs   (design says ~3.4 µs)", worstNs/1000)
	t.Logf("")
	t.Logf("  typical steady state  %s evals/s x %.3f µs = %.3f cores",
		human(typicalEvalsPerSec), typicalNs/1000, coresFor(typicalNs, typicalEvalsPerSec))
	t.Logf("  peak realistic        %s evals/s x %.3f µs = %.3f cores",
		human(peakEvalsPerSec), typicalNs/1000, coresFor(typicalNs, peakEvalsPerSec))
	t.Logf("  peak pathological     %s evals/s x %.3f µs = %.3f cores   (design says ~8.16)",
		human(peakEvalsPerSec), worstNs/1000, coresFor(worstNs, peakEvalsPerSec))
	t.Logf("")
	t.Logf("  These are single-machine costs multiplied by a fleet-wide rate. They say")
	t.Logf("  how many cores of CPU the fleet spends on evaluation IN TOTAL; they do not")
	t.Logf("  say that any one pod can supply them, and they assume evaluation scales")
	t.Logf("  linearly with cores, which L3 supports on this box and nothing here proves")
	t.Logf("  for a machine with a different memory hierarchy.")
	t.Logf("")
}

// reportAllocationPressure converts per-operation allocation counts into the
// rate the collector actually sees at the measured throughput.
//
// docs/05-consistency-and-e2e.md §3.2 states the reason this matters in one
// line: "a single allocation per evaluation is 2.4M allocs/sec at peak and
// turns the GC into the bottleneck". The read path is zero-allocation for a
// plain flag and one allocation for a rollout flag, so the real figure depends
// entirely on what fraction of a corpus carries a rollout.
func reportAllocationPressure(t *testing.T, batch allocResult, l3 latencyRun) {
	perOpAllocs := float64(batch.AllocsPerOp)
	perOpBytes := float64(batch.BytesPerOp)
	ops := l3.opsPerSec()

	t.Logf("allocation pressure on the read path -----------------------------")
	t.Logf("  batch of 100 (20%% rollout flags)   %.0f allocs, %.0f B", perOpAllocs, perOpBytes)
	t.Logf("  at L3's measured %s batches/sec    %s allocs/sec, %.0f MB/sec of garbage",
		human(ops), human(perOpAllocs*ops), perOpBytes*ops/(1024*1024))
	t.Logf("  at the design's peak of %s evaluations/sec (%s batches/sec):", human(peakEvalsPerSec), human(peakEvalsPerSec/batchFlags))
	t.Logf("      %s allocs/sec, %.0f MB/sec of garbage",
		human(perOpAllocs*peakEvalsPerSec/batchFlags), perOpBytes*peakEvalsPerSec/batchFlags/(1024*1024))
	t.Logf("  SOURCE: internal/core/bucket.go NamespaceStrategy.Key builds the bucket")
	t.Logf("  key with a strings.Builder, one string per rollout evaluation. A plain")
	t.Logf("  flag and a rule-matched flag both allocate nothing.")
	t.Logf("  OBSERVED: L3 ran %d garbage collections in %s of load. A read path that",
		l3.GC.NumGC, l3.Elapsed.Round(time.Millisecond))
	t.Logf("  allocated nothing on every flag shape would have run close to none.")
	t.Logf("")
}

// TestL6SnapshotMemory reports the memory curve on its own, for when only the
// sizing question is being asked.
//
//	go test ./test/load/ -run TestL6SnapshotMemory -v
func TestL6SnapshotMemory(t *testing.T) {
	if raceEnabled {
		t.Skip("skipped under -race: heap accounting under ThreadSanitizer is not " +
			"the heap this design ships")
	}
	reportMachine(t)
	sizes := []int{1000, 5000, 20000}
	if testing.Short() {
		sizes = []int{1000, 5000}
	}
	points := scenarioL6(t, sizes)
	reportL6(t, points)

	if n := snapshotLen(t, 1000); n != 1000 {
		t.Fatalf("resolved snapshot holds %d flags, want 1000 — the memory numbers "+
			"above would be measuring a snapshot that quietly dropped flags", n)
	}
	for _, p := range points {
		t.Logf("L6 %5d flags: client %.2f MB, service %.2f MB, ratio %.2fx",
			p.Flags, mib(p.MemSnap), mib(p.Resolved),
			float64(p.Resolved)/float64(maxU64(p.MemSnap, 1)))
	}
}

func maxU64(v, floor uint64) uint64 {
	if v < floor {
		return floor
	}
	return v
}

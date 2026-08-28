package load

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// ---------------------------------------------------------------------------
// The six scenarios of docs/05-consistency-and-e2e.md §3.1, as reusable
// drivers. The Benchmark* functions and TestPassCriteria both call these, so
// there is exactly one definition of what "L4" means.
// ---------------------------------------------------------------------------

// batchOp returns a per-worker closure evaluating one 100-flag batch against a
// pinned snapshot, reusing its own destination slice.
//
// BatchAppend rather than Batch on purpose: Batch allocates a fresh
// 100-element []core.Result per call, and a 12k RPS service pooling that slice
// is the shape the design documents. Both are measured; see BenchmarkL2Batch
// versus BenchmarkL2BatchAppend for what the pooling is worth.
func batchOp(c *client.Client, reqs []client.Request) func() {
	ctx := context.Background()
	dst := make([]core.Result, 0, len(reqs))
	return func() {
		dst = c.BatchAppend(ctx, dst, reqs)
		// Defeat dead-code elimination. Without a consumed result the whole
		// call is eligible for removal and the benchmark reports a number that
		// describes nothing.
		sinkBool = sinkBool != dst[len(dst)-1].Value.IsUnknown()
	}
}

// ---------------------------------------------------------------------------
// L2 — batch of 100 flags, one snapshot pin, no contention. THE HEADLINE.
// ---------------------------------------------------------------------------

func scenarioL2(t *testing.T, d time.Duration) latencyRun {
	t.Helper()
	c, _ := newLoadClient(t, typicalSnapshot(1))
	defer closeClient(c)
	reqs := typicalBatch(batchFlags)
	return runLoad("L2  batch of 100, single goroutine, no churn",
		1, d, batchFlags, func(int) func() { return batchOp(c, reqs) })
}

// ---------------------------------------------------------------------------
// L3 — L2 across GOMAXPROCS goroutines. Proves the read path does not contend.
// ---------------------------------------------------------------------------

func scenarioL3(t *testing.T, d time.Duration) latencyRun {
	t.Helper()
	c, _ := newLoadClient(t, typicalSnapshot(1))
	defer closeClient(c)
	reqs := typicalBatch(batchFlags)
	workers := activeWorkers()
	return runLoad("L3  batch of 100, all cores saturated, no churn",
		workers, d, batchFlags, func(int) func() { return batchOp(c, reqs) })
}

// ---------------------------------------------------------------------------
// L4 — L3 with a writer swapping the snapshot every 100 ms.
//
// THE LOAD-BEARING SCENARIO. Two claims are on trial: that an atomic pointer
// swap is invisible to readers (so a config change is not a latency event), and
// that the garbage produced by discarding a generation every 100 ms is
// affordable.
//
// The writer builds a genuinely NEW snapshot each time rather than rotating
// pre-built ones. Rotating would swap pointers without producing garbage, which
// would delete the second half of the question.
// ---------------------------------------------------------------------------

func scenarioL4(t *testing.T, d time.Duration) latencyRun {
	t.Helper()
	c, src := newLoadClient(t, typicalSnapshot(1))
	defer closeClient(c)
	reqs := typicalBatch(batchFlags)
	workers := activeWorkers()

	var churns atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(*churnInterval)
		defer tick.Stop()
		gen := int64(1)
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				gen++
				if src.push(typicalSnapshot(gen)) {
					churns.Add(1)
				}
			}
		}
	}()

	run := runLoad("L4  batch of 100, all cores saturated, config swap every "+churnInterval.String(),
		workers, d, batchFlags, func(int) func() { return batchOp(c, reqs) })

	close(stop)
	wg.Wait()

	run.Churns = churns.Load()
	run.FinalGen = c.Generation()
	return run
}

// ---------------------------------------------------------------------------
// L5 — the pathological corner: 20 rules x 4 conditions, batch of 100.
// ---------------------------------------------------------------------------

func scenarioL5(t *testing.T, d time.Duration) latencyRun {
	t.Helper()
	snap := client.NewMemSnapshot(loadEnv, 1, pathologicalFlags(batchFlags))
	c, _ := newLoadClient(t, snap)
	defer closeClient(c)
	reqs := pathologicalBatch(batchFlags)
	return runLoad("L5  batch of 100 worst-case flags (20 rules x 4 conditions), single goroutine",
		1, d, batchFlags, func(int) func() { return batchOp(c, reqs) })
}

// ---------------------------------------------------------------------------
// L6 — snapshot resident memory at 1k / 5k / 20k flags.
// ---------------------------------------------------------------------------

type memPoint struct {
	Flags    int
	MemSnap  uint64 // client.MemSnapshot: what a CLIENT holds
	Resolved uint64 // config.ResolvedSnapshot: what the SERVICE holds
}

func scenarioL6(t *testing.T, sizes []int) []memPoint {
	t.Helper()
	out := make([]memPoint, 0, len(sizes))
	for _, n := range sizes {
		// The corpus is built INSIDE the closure on purpose. A snapshot copies
		// the flag structs but SHARES their rule slices, condition slices,
		// value slices and key strings with whatever built them, so measuring
		// against a corpus that stays live outside the measurement charges the
		// snapshot for the copies and nothing else — it under-reports the real
		// footprint of a snapshot decoded from the wire, which owns the lot.
		// Built inside, the temporary []core.Flag is collected and everything
		// the snapshot actually retains is counted.
		memBytes := heapDelta(func() any {
			return client.NewMemSnapshot(loadEnv, 1, typicalFlags(n))
		})

		// The service-side snapshot is isolated by differencing: Store.Set
		// clones the layer it accepts, so the with-environment measurement is
		// charged for a clone the snapshot does not own, and the
		// without-environment measurement holds exactly that clone and nothing
		// else. The KeepAlive is load bearing — if layer died between the two
		// readings, the second one would under-report and the difference would
		// be inflated by a whole base layer.
		layer := baseLayer(n)
		withSnap := heapDelta(func() any { return storeWithSnapshot(t, layer) })
		withoutSnap := heapDelta(func() any { return storeWithoutSnapshot(layer) })
		runtime.KeepAlive(layer)
		var resolved uint64
		if withSnap > withoutSnap {
			resolved = withSnap - withoutSnap
		}

		out = append(out, memPoint{Flags: n, MemSnap: memBytes, Resolved: resolved})
	}
	sinkAny = nil
	return out
}

func reportL6(t *testing.T, points []memPoint) {
	t.Logf("L6  snapshot resident memory")
	t.Logf("  flags   client MemSnapshot        service ResolvedSnapshot")
	for _, p := range points {
		t.Logf("  %-6d  %7.2f MB (%4.0f B/flag)   %7.2f MB (%5.0f B/flag)",
			p.Flags,
			mib(p.MemSnap), float64(p.MemSnap)/float64(p.Flags),
			mib(p.Resolved), float64(p.Resolved)/float64(p.Flags))
	}
	t.Logf("  MEASURES RETAINED HEAP (runtime.MemStats.HeapAlloc delta after a forced GC),")
	t.Logf("  NOT RSS. Runtime overhead, fragmentation, stacks and unreturned spans are")
	t.Logf("  excluded; a pod's actual resident set is larger.")
	t.Logf("")
}

// ---------------------------------------------------------------------------
// L1 — the floor: one BoolValue on a cached snapshot.
//
// No percentile harness. Two time.Now() calls cost more than the operation
// itself here, so a per-op timestamp would measure the clock, not the design.
// L1 is reported through testing.B, which is the right instrument for an
// operation this small, and P2 (allocations per op) is exactly what testing.B
// measures well.
// ---------------------------------------------------------------------------

type allocResult struct {
	NsPerOp     float64
	AllocsPerOp int64
	BytesPerOp  int64
	Iterations  int
}

func measureAllocs(fn func(b *testing.B)) allocResult {
	res := testing.Benchmark(fn)
	return allocResult{
		NsPerOp:     float64(res.NsPerOp()),
		AllocsPerOp: res.AllocsPerOp(),
		BytesPerOp:  res.AllocedBytesPerOp(),
		Iterations:  res.N,
	}
}

// l1Alloc measures the floor path: a plain flag, no rules, no rollout.
func l1Alloc(tb testing.TB) allocResult {
	c, _ := newLoadClient(tb, typicalSnapshot(1))
	defer closeClient(c)
	ctx := context.Background()
	ec := loadContext()
	key := plainFlagKey()
	return measureAllocs(func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = c.BoolValue(ctx, key, false, ec)
		}
	})
}

// l1AllocRollout measures the same call against a flag carrying a percentage
// rollout. Not a pass criterion; the most important finding in the suite.
func l1AllocRollout(tb testing.TB) allocResult {
	c, _ := newLoadClient(tb, typicalSnapshot(1))
	defer closeClient(c)
	ctx := context.Background()
	ec := loadContext()
	key := rolloutFlagKey()
	return measureAllocs(func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkBool = c.BoolValue(ctx, key, false, ec)
		}
	})
}

// closeClient releases a scenario's client as soon as the scenario ends rather
// than at the end of the test.
//
// Without it, every client built earlier in TestPassCriteria keeps its snapshot
// and its churn generations reachable for the whole run, so each later scenario
// marks a larger live heap than the one before it and L4 would be compared
// against an L3 that ran on a smaller heap. Close is idempotent, so the
// registered cleanup still runs harmlessly.
func closeClient(c *client.Client) { _ = c.Close() }

// l2BatchAllocs measures allocations for one 100-flag batch on the pooled
// (BatchAppend) path. Not a pass criterion — P2 covers the single-flag floor —
// but it is what turns "one allocation per rollout evaluation" into a garbage
// rate the GC has to keep up with.
func l2BatchAllocs(tb testing.TB) allocResult {
	c, _ := newLoadClient(tb, typicalSnapshot(1))
	defer closeClient(c)
	reqs := typicalBatch(batchFlags)
	op := batchOp(c, reqs)
	return measureAllocs(func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			op()
		}
	})
}

// activeWorkers is the goroutine count for the saturating scenarios.
func activeWorkers() int { return runtime.GOMAXPROCS(0) }

// ---------------------------------------------------------------------------
// Extrapolation, stated as arithmetic so the reader can check it.
// ---------------------------------------------------------------------------

// coresFor returns the core count needed to sustain evalsPerSec evaluations per
// second at the measured per-evaluation cost.
func coresFor(nsPerEval float64, evalsPerSec float64) float64 {
	return nsPerEval * evalsPerSec / 1e9
}

// peakEvalsPerSec is docs/03-lld.md §1: 24,000 RPS x 100 flags.
const peakEvalsPerSec = 2_400_000.0

// typicalEvalsPerSec is 12,000 RPS x 30 flags.
const typicalEvalsPerSec = 360_000.0

// storeSnapshotForSizing is used by the L6 report to confirm the resolved
// snapshot actually contains what it claims to.
func snapshotLen(t *testing.T, n int) int {
	t.Helper()
	s := storeWithSnapshot(t, baseLayer(n))
	snap, ok := s.Snapshot(loadEnv)
	if !ok {
		t.Fatalf("no snapshot published for %s", loadEnv)
	}
	return snap.Len()
}

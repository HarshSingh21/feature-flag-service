package load

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// ---------------------------------------------------------------------------
// testing.B scenarios.
//
// Every one of these:
//   - calls b.ReportAllocs(), because allocations per operation are a pass
//     criterion, not a curiosity: one allocation per evaluation at the 2.4M/sec
//     peak is 2.4M allocations/sec and the GC becomes the bottleneck;
//   - calls b.ResetTimer() after setup, so snapshot construction never lands
//     inside the timed region;
//   - feeds its result into a package-level sink, so the compiler cannot delete
//     the work and report an impossibly fast number;
//   - reports evals/sec, and for batch scenarios ns/eval, because ns/op on a
//     100-flag batch hides the per-flag cost that the capacity plan is built on.
// ---------------------------------------------------------------------------

// reportRates attaches the two derived metrics every scenario owes the reader.
func reportRates(b *testing.B, evalsPerOp int) {
	secs := b.Elapsed().Seconds()
	if secs <= 0 {
		return
	}
	evals := float64(b.N) * float64(evalsPerOp)
	b.ReportMetric(evals/secs, "evals/sec")
	if evalsPerOp > 1 {
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/evals, "ns/eval")
	}
}

func skipUnderRace(b *testing.B) {
	if raceEnabled {
		b.Skip("skipped under -race: the race detector instruments every memory " +
			"access, so throughput and latency from this binary describe " +
			"ThreadSanitizer, not the design")
	}
}

// ---------------------------------------------------------------------------
// L1 — floor cost. Single flag, cached snapshot, no contention.
// ---------------------------------------------------------------------------

// BenchmarkL1BoolValuePlain is the scenario the design's ~0.3 µs figure refers
// to: a flag with no rules and no rollout, resolved through the client's typed
// accessor. P2 (0 allocations per op) is evaluated against this.
func BenchmarkL1BoolValuePlain(b *testing.B) {
	c, _ := newLoadClient(b, typicalSnapshot(1))
	ctx := context.Background()
	ec := loadContext()
	key := plainFlagKey()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = c.BoolValue(ctx, key, false, ec)
	}
	b.StopTimer()
	reportRates(b, 1)
}

// BenchmarkL1BoolValueRollout is the same call against a flag carrying a
// percentage rollout — 20% of the realistic corpus. The bucket key is built
// with a strings.Builder (internal/core/bucket.go NamespaceStrategy.Key), so
// this path is expected to allocate. It is measured separately rather than
// averaged into L1 because the two numbers mean different things.
func BenchmarkL1BoolValueRollout(b *testing.B) {
	c, _ := newLoadClient(b, typicalSnapshot(1))
	ctx := context.Background()
	ec := loadContext()
	key := rolloutFlagKey()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = c.BoolValue(ctx, key, false, ec)
	}
	b.StopTimer()
	reportRates(b, 1)
}

// BenchmarkL1BoolValueRules is the third shape in the corpus: two targeting
// rules, the second of which matches.
func BenchmarkL1BoolValueRules(b *testing.B) {
	c, _ := newLoadClient(b, typicalSnapshot(1))
	ctx := context.Background()
	ec := loadContext()
	key := ruleFlagKey()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = c.BoolValue(ctx, key, false, ec)
	}
	b.StopTimer()
	reportRates(b, 1)
}

// BenchmarkL1EnginePlain measures the engine directly, beneath the client's
// entry boundary (the recover frame, the atomic load, the metrics branch). The
// difference between this and BenchmarkL1BoolValuePlain is what the client
// costs per call, which is the number that decides whether the batch API is
// mandatory or merely tidy.
func BenchmarkL1EnginePlain(b *testing.B) {
	ev := newEngine()
	snap := typicalSnapshot(1)
	ec := loadContext()
	key := plainFlagKey()
	def := core.Bool(false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := ev.Evaluate(snap, key, ec, core.TypeBool, def)
		sinkBool = sinkBool != res.Value.IsUnknown()
	}
	b.StopTimer()
	reportRates(b, 1)
}

// BenchmarkL1Unbatched100 is the counterfactual the design uses to justify the
// batch API: 100 individual client calls instead of one batch. If this is not
// meaningfully worse than L2, the "batch API is mandatory" claim is decoration.
func BenchmarkL1Unbatched100(b *testing.B) {
	c, _ := newLoadClient(b, typicalSnapshot(1))
	ctx := context.Background()
	ec := loadContext()
	// Keys are precomputed: flagKey() calls strconv.Itoa, and building the key
	// inside the loop would charge the design for 100 string allocations per
	// iteration that a real call site does not make.
	keys := make([]string, batchFlags)
	for f := range keys {
		keys[f] = flagKey(f)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for f := 0; f < batchFlags; f++ {
			sinkBool = c.BoolValue(ctx, keys[f], false, ec)
		}
	}
	b.StopTimer()
	reportRates(b, batchFlags)
}

// ---------------------------------------------------------------------------
// L2 — batch of 100, one pin. THE HEADLINE SCENARIO.
// ---------------------------------------------------------------------------

// BenchmarkL2Batch100 uses Batch, which allocates a fresh result slice per
// call. This is the API most call sites will actually use.
func BenchmarkL2Batch100(b *testing.B) {
	c, _ := newLoadClient(b, typicalSnapshot(1))
	ctx := context.Background()
	reqs := typicalBatch(batchFlags)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := c.Batch(ctx, reqs)
		sinkBool = sinkBool != out[len(out)-1].Value.IsUnknown()
	}
	b.StopTimer()
	reportRates(b, batchFlags)
}

// BenchmarkL2BatchAppend100 uses BatchAppend with a pooled destination slice —
// the shape a 12k RPS service should use. The delta against BenchmarkL2Batch100
// is what the pooling buys.
func BenchmarkL2BatchAppend100(b *testing.B) {
	c, _ := newLoadClient(b, typicalSnapshot(1))
	reqs := typicalBatch(batchFlags)
	op := batchOp(c, reqs)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		op()
	}
	b.StopTimer()
	reportRates(b, batchFlags)
}

// ---------------------------------------------------------------------------
// L3 — L2 across all cores.
// ---------------------------------------------------------------------------

func BenchmarkL3Batch100Parallel(b *testing.B) {
	skipUnderRace(b)
	c, _ := newLoadClient(b, typicalSnapshot(1))
	reqs := typicalBatch(batchFlags)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		op := batchOp(c, reqs)
		for pb.Next() {
			op()
		}
	})
	b.StopTimer()
	reportRates(b, batchFlags)
}

// ---------------------------------------------------------------------------
// L4 — L3 with a writer swapping the snapshot every 100 ms.
// ---------------------------------------------------------------------------

func BenchmarkL4Batch100ParallelWithChurn(b *testing.B) {
	skipUnderRace(b)
	c, src := newLoadClient(b, typicalSnapshot(1))
	reqs := typicalBatch(batchFlags)

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

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		op := batchOp(c, reqs)
		for pb.Next() {
			op()
		}
	})
	b.StopTimer()

	close(stop)
	wg.Wait()

	reportRates(b, batchFlags)
	b.ReportMetric(float64(churns.Load()), "swaps")
}

// ---------------------------------------------------------------------------
// L5 — the pathological corner.
// ---------------------------------------------------------------------------

func BenchmarkL5Batch100Pathological(b *testing.B) {
	snap := client.NewMemSnapshot(loadEnv, 1, pathologicalFlags(batchFlags))
	c, _ := newLoadClient(b, snap)
	reqs := pathologicalBatch(batchFlags)
	op := batchOp(c, reqs)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		op()
	}
	b.StopTimer()
	reportRates(b, batchFlags)
}

func BenchmarkL5Batch100PathologicalParallel(b *testing.B) {
	skipUnderRace(b)
	snap := client.NewMemSnapshot(loadEnv, 1, pathologicalFlags(batchFlags))
	c, _ := newLoadClient(b, snap)
	reqs := pathologicalBatch(batchFlags)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		op := batchOp(c, reqs)
		for pb.Next() {
			op()
		}
	})
	b.StopTimer()
	reportRates(b, batchFlags)
}

// BenchmarkL5SingleWorstFlag isolates one worst-case evaluation, which is the
// figure docs/03-lld.md §4.1 states as "~3.4 µs worst realistic".
func BenchmarkL5SingleWorstFlag(b *testing.B) {
	snap := client.NewMemSnapshot(loadEnv, 1, pathologicalFlags(1))
	c, _ := newLoadClient(b, snap)
	ctx := context.Background()
	ec := loadContext()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkBool = c.BoolValue(ctx, "worst-0", false, ec)
	}
	b.StopTimer()
	reportRates(b, 1)
}

// ---------------------------------------------------------------------------
// L6 — snapshot construction cost. The RESIDENT SIZE is measured in
// TestL6SnapshotMemory; this is the build cost, which bounds how fast a
// config change can be published.
// ---------------------------------------------------------------------------

func BenchmarkL6BuildMemSnapshot5k(b *testing.B) {
	flags := typicalFlags(5000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkAny = client.NewMemSnapshot(loadEnv, int64(i), flags)
	}
}

func BenchmarkL6BuildResolvedSnapshot5k(b *testing.B) {
	layer := baseLayer(5000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := storeWithSnapshot(b, layer)
		snap, _ := s.Snapshot(loadEnv)
		sinkAny = snap
	}
}

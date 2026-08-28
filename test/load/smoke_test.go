package load

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// ---------------------------------------------------------------------------
// The measurements are only worth anything if the thing being measured is
// actually doing the work. A batch of 100 keys that are all missing from the
// snapshot returns 100 FLAG_NOT_FOUND results very fast indeed, and every
// number in this suite would be a number about a map miss.
//
// These two tests are the guard, and unlike everything else here they DO run
// under -race: they assert on behaviour, not on timing, so ThreadSanitizer
// makes them stricter rather than meaningless. They are also the only
// race-coverage this package's own harness gets.
// ---------------------------------------------------------------------------

// assertResolves fails unless every result in the batch is a real configured
// answer carrying the snapshot's generation.
func assertResolves(tb testing.TB, c *client.Client, reqs []client.Request, wantGen int64) {
	tb.Helper()
	out := c.Batch(context.Background(), reqs)
	if len(out) != len(reqs) {
		tb.Fatalf("batch returned %d results for %d requests", len(out), len(reqs))
	}
	for i, r := range out {
		if r.Reason.IsFallback() {
			tb.Fatalf("req[%d] %q resolved to fallback reason %s — the benchmark corpus and "+
				"the request set disagree, and every latency in this suite would be measuring "+
				"a miss instead of an evaluation", i, reqs[i].Flag, r.Reason)
		}
		if r.Reason == core.ReasonUnknown {
			tb.Fatalf("req[%d] %q carries no reason", i, reqs[i].Flag)
		}
		if r.Generation != wantGen {
			tb.Fatalf("req[%d] %q reports generation %d, want %d — a batch must read exactly "+
				"one generation (invariant CACHE-1)", i, reqs[i].Flag, r.Generation, wantGen)
		}
	}
}

// TestCorpusResolves proves both corpora are real work: every flag in every
// batch resolves to a configured value from one generation.
func TestCorpusResolves(t *testing.T) {
	t.Run("typical", func(t *testing.T) {
		c, _ := newLoadClient(t, typicalSnapshot(7))
		defer closeClient(c)
		assertResolves(t, c, typicalBatch(batchFlags), 7)
	})

	t.Run("pathological", func(t *testing.T) {
		snap := client.NewMemSnapshot(loadEnv, 9, pathologicalFlags(batchFlags))
		c, _ := newLoadClient(t, snap)
		defer closeClient(c)
		assertResolves(t, c, pathologicalBatch(batchFlags), 9)
	})

	t.Run("worst case flag evaluates every condition", func(t *testing.T) {
		// If the pathological flag matched a rule, the 20-rule cost would never
		// be paid and L5 would be measuring a one-rule flag. It must fall all
		// the way through to the rollout.
		snap := client.NewMemSnapshot(loadEnv, 1, pathologicalFlags(1))
		c, _ := newLoadClient(t, snap)
		defer closeClient(c)
		res := c.BoolDetail(context.Background(), "worst-0", false, loadContext())
		if res.Reason != core.ReasonRolloutIn && res.Reason != core.ReasonRolloutOut {
			t.Fatalf("worst-case flag resolved with reason %s, want a rollout reason: a rule "+
				"matched, so L5 is not measuring 20 rules x 4 conditions", res.Reason)
		}
	})
}

// TestHarnessUnderChurn exercises the L4 machinery briefly and asserts only on
// correctness: readers keep getting configured answers while the writer swaps
// generations underneath them, and the client's generation advances.
func TestHarnessUnderChurn(t *testing.T) {
	c, src := newLoadClient(t, typicalSnapshot(1))
	defer closeClient(c)
	reqs := typicalBatch(batchFlags)
	ctx := context.Background()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		gen := int64(1)
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				gen++
				src.push(typicalSnapshot(gen))
			}
		}
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	var reads int
	for time.Now().Before(deadline) {
		out := c.Batch(ctx, reqs)
		gen := out[0].Generation
		for i, r := range out {
			if r.Reason.IsFallback() {
				t.Fatalf("read %d req[%d] %q fell back to %s during a config swap",
					reads, i, reqs[i].Flag, r.Reason)
			}
			if r.Generation != gen {
				t.Fatalf("read %d returned generation %d at index 0 and %d at index %d — "+
					"a swap landed mid-batch and the snapshot was not pinned (CACHE-1)",
					reads, gen, r.Generation, i)
			}
		}
		reads++
	}

	close(stop)
	wg.Wait()

	if reads == 0 {
		t.Fatal("no reads completed")
	}
	if got := c.Generation(); got <= 1 {
		t.Fatalf("client generation is %d after 200ms of churn; the L4 writer is not "+
			"reaching the client, so L4 would measure L3 with extra goroutines", got)
	}
	t.Logf("%d batches read while the snapshot was swapped to generation %d", reads, c.Generation())
}

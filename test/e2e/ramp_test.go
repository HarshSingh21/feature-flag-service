package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// ---------------------------------------------------------------------------
// The ramp scenario: ONE flag, walked from 70 to 700 by a loop.
//
// docs/05-consistency-and-e2e.md §2.1 covers a SINGLE write (OLD -> NEW) and
// proves the reader never sees a torn state across it. This file covers the
// other shape operators actually produce: a long series of small writes, one
// after another, the way a limit gets ramped during a rollout or a load test.
//
// The two failure modes are different. One write can tear. Sixty-three writes
// in a row can also:
//
//   - LOSE a step. The client jumps 70 -> 90 and the 80 push is never observed
//     by anyone. Harmless-looking, and fatal when the step was a kill switch.
//   - REORDER. The client reads 90, then 80. The operator's audit log says the
//     limit only ever went up; production disagrees.
//   - DRIFT off the generation. A value moves while the generation does not,
//     which means the answer did not come from a published snapshot.
//   - FALL BACK mid-ramp. One reconnect returns the caller's default, and a
//     "limit" of -1 flows into code that assumed a configured number.
//
// So the assertions here are about a SEQUENCE, not a sample:
//
//	R1  every step the operator writes is observed by the client         (no step lost)
//	R2  the server resolves each step before the client is expected to    (write landed)
//	R3  the value never regresses, and the generation never regresses     (no reorder)
//	R4  a value change is always accompanied by a generation change       (no drift)
//	R5  every observation is a real step on the ramp, never the default   (no fallback)
//	R6  every observation carries FALLTHROUGH                             (no error path)
//	R7  once 700 is observed, nothing else ever is                        (no flap back)
//	R8  the server's generation advanced exactly once per write           (no churn)
//
// R1 is proven by the foreground loop, which refuses to move on until the
// client reads the value just written. R3-R7 are proven by a watcher goroutine
// sampling the client continuously underneath the whole ramp, because "never
// out of order" is not a property any single read can show.
// ---------------------------------------------------------------------------

const (
	// rampKey satisfies the store's key charset (^[a-z0-9][a-z0-9._-]{0,127}$).
	rampKey = "checkout.ramp-limit"

	// The ramp itself: the flag goes in at 70, the loop takes it to 700.
	rampFrom = int64(70)
	rampTo   = int64(700)
	rampStep = int64(10)

	// callerDefaultInt is deliberately a value the configuration can never
	// produce, so "the client returned its own default" is visible in the log
	// rather than indistinguishable from a configured number.
	callerDefaultInt = int64(-1)

	// rampWatchInterval paces the watcher. Fast enough that a step lands inside
	// its window many times over, slow enough that the log stays readable.
	rampWatchInterval = 200 * time.Microsecond

	// rampSettledTail is how many observations at the final value R7 requires
	// before it will call the ramp settled.
	rampSettledTail = 50

	// rampSourcePoll is how often this test's config source asks the server
	// whether the generation moved.
	//
	// It is named rather than inlined because it is the FLOOR of every
	// convergence number the report prints: a write that lands just after a poll
	// waits nearly a full interval for the next one. Anyone reading "p50 20ms"
	// needs to see this constant next to it, or they will read a harness
	// property as an SDK one.
	rampSourcePoll = 20 * time.Millisecond

	// shortRampStep keeps `go test -short ./...` quick: the same 70 -> 700 walk
	// in 7 writes instead of 63. (700-70) is divisible by both.
	shortRampStep = int64(90)
)

// rampStepSize honours -short. Every assertion derives from this rather than
// from a hardcoded count, so the two modes prove the same thing.
func rampStepSize() int64 {
	if testing.Short() {
		return shortRampStep
	}
	return rampStep
}

// ---------------------------------------------------------------------------
// Fixtures. Authored as JSON because that is what crosses the wire, and the
// `layer` discriminator is what harness_test.go's applier switches on.
// ---------------------------------------------------------------------------

// rampBaseLayer publishes the flag as a plain int with no rules and no rollout.
// That shape is chosen on purpose: it makes FALLTHROUGH the only legal reason,
// so R6 turns any stray rule, rollout or error path into a failure.
func rampBaseLayer(v int64) []byte {
	return []byte(fmt.Sprintf(`{
	  "layer": "base",
	  "schema_version": 1,
	  "flags": [
	    {
	      "key": %q,
	      "type": "int",
	      "owner": "team-checkout",
	      "description": "per-request item cap; ramped by the loop in ramp_test.go",
	      "enabled": true,
	      "default_value": %d,
	      "off_value": 0
	    }
	  ]
	}`, rampKey, v))
}

// rampOverlay is one step of the ramp: the smallest write that changes the
// served value, touching one environment and one field.
func rampOverlay(env string, v int64) []byte {
	return []byte(fmt.Sprintf(`{
	  "layer": "overlay",
	  "schema_version": 1,
	  "environment": %q,
	  "flags": [{"key": %q, "default_value": %d}]
	}`, env, rampKey, v))
}

// ---------------------------------------------------------------------------
// The client under observation.
// ---------------------------------------------------------------------------

// newRampClient is harness.newClient narrowed to one flag.
//
// It exists rather than reusing newClient because the source's key list is what
// it fetches: pointing it at all 100 scenario flags would make every step of
// the ramp cost 100 HTTP round trips and would report 99 FLAG_NOT_FOUND results
// that have nothing to do with this test.
func newRampClient(t *testing.T, h *harness, env string) (*client.Client, *httpSource, *clientLog) {
	t.Helper()
	src := newHTTPSource(h.adminSrv.URL, []string{rampKey}, instanceID, rampSourcePoll)
	lg := newClientLog()

	cl, err := client.New(
		client.WithEnvironment(env),
		client.WithSource(src),
		client.WithLogger(lg),
		client.WithDeadStreamThreshold(750*time.Millisecond),
		client.WithReconcileInterval(2*time.Second),
		client.WithFetchTimeout(3*time.Second),
		client.WithStalenessWarning(time.Hour),
		client.WithBackoff(client.FullJitterBackoff(5*time.Millisecond, 50*time.Millisecond)),
	)
	if err != nil {
		t.Fatalf("build ramp client: %v", err)
	}
	t.Cleanup(func() {
		_ = cl.Close()
		src.close()
	})
	return cl, src, lg
}

// rampValueOnServer reports what the SERVER currently resolves the flag to,
// read back over the same admin API an operator would use. It is the resolved
// value — base merged with the overlay — not the base's.
func rampValueOnServer(t *testing.T, h *harness, env string) int64 {
	t.Helper()
	var resp httpx.SnapshotDebugResponse
	status := h.getJSON(t, h.adminSrv.URL+"/v1/config/snapshot/"+env+"?flag="+rampKey, &resp)
	if status != http.StatusOK {
		t.Fatalf("snapshot debug %s/%s: status %d", env, rampKey, status)
	}
	if resp.Flag == nil {
		t.Fatalf("snapshot debug %s/%s: no flag in response", env, rampKey)
	}
	v, ok := resp.Flag.DefaultValue.AsInt()
	if !ok {
		t.Fatalf("server resolved %s to %s, which is not an int", rampKey, resp.Flag.DefaultValue)
	}
	return v
}

// ---------------------------------------------------------------------------
// The watcher: a continuous record of what the client answered, underneath the
// whole ramp. Assertions are made against this record, never against a sleep.
// ---------------------------------------------------------------------------

type rampObs struct {
	at         time.Time
	value      int64
	generation int64
	reason     core.Reason
	bucket     int32
	latency    time.Duration
}

// rampWatcher owns exactly one goroutine, bound to the context passed to start.
// stop does not return until that goroutine has exited, so no test can leave it
// running.
type rampWatcher struct {
	cl  *client.Client
	ctx core.EvalContext

	mu  sync.Mutex
	obs []rampObs

	wg     sync.WaitGroup
	panics atomic.Int64
}

func newRampWatcher(cl *client.Client) *rampWatcher {
	return &rampWatcher{
		cl:  cl,
		ctx: core.EvalContext{UserID: "user-4711", TenantID: "tenant-9"},
		obs: make([]rampObs, 0, 8192),
	}
}

func (w *rampWatcher) start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		tick := time.NewTicker(rampWatchInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			w.once(ctx)
		}
	}()
}

// once records one read. The recover is not decoration: "the SDK never panics"
// is only an assertion if there is a boundary that would count one.
func (w *rampWatcher) once(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			w.panics.Add(1)
		}
	}()

	start := time.Now()
	res := w.cl.IntDetail(ctx, rampKey, callerDefaultInt, w.ctx)
	latency := time.Since(start)

	v, ok := res.Value.AsInt()
	if !ok {
		// A non-int answer for an int flag is a finding, not a reason to skip
		// the sample: record it as an impossible value so R5 reports it.
		v = callerDefaultInt
	}
	w.mu.Lock()
	w.obs = append(w.obs, rampObs{
		at:         start.Add(latency),
		value:      v,
		generation: res.Generation,
		reason:     res.Reason,
		bucket:     res.Bucket,
		latency:    latency,
	})
	w.mu.Unlock()
}

func (w *rampWatcher) stop() { w.wg.Wait() }

// all returns a copy, so analysis never races the recorder.
func (w *rampWatcher) all() []rampObs {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]rampObs, len(w.obs))
	copy(out, w.obs)
	return out
}

func (w *rampWatcher) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.obs)
}

// tailCount reports how many observations were recorded at or after the FIRST
// observation carrying v.
//
// It is the predicate the ramp waits on before it stops watching. The foreground
// loop proves the client READS the final value; R7's other half — that it then
// never leaves it — needs a stretch of samples at that value, and cancelling the
// watcher the instant the loop finishes can truncate the record before its next
// tick lands. That is a race in the TEST, and it showed up as R7 failing roughly
// one run in three.
func (w *rampWatcher) tailCount(v int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.obs {
		if w.obs[i].value == v {
			return len(w.obs) - i
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// The scenario.
// ---------------------------------------------------------------------------

// TestFlagValueRampedFrom70To700_ClientSeesEveryStepInOrder walks one flag from
// 70 to 700 with one real config write per step and interrogates the sequence.
//
// Carries R1-R8, described at the top of this file.
func TestFlagValueRampedFrom70To700_ClientSeesEveryStepInOrder(t *testing.T) {
	step := rampStepSize()

	// How many writes the loop will issue.
	//
	// The loop writes rampFrom+step, rampFrom+2*step, ... up to and INCLUDING
	// rampTo. The start value is not a write — it is already live from the base
	// publish — so the count is a floor, not the ceiling you would use for a
	// half-open range like Python's range(start, end, step):
	//
	//	writes                              = (rampTo - rampFrom) / step
	//	distinct values the client passes   = writes + 1
	//
	// The two agree only when step divides the gap exactly, which the check
	// below is what makes true rather than assumed.
	wantWrites := int((rampTo - rampFrom) / step)

	// The ramp must LAND on rampTo, because R7 asserts the client settles on
	// that exact value. A step that does not divide the gap stops short — with
	// step 4 the last write is 698 — and the run then fails as "never observed
	// 700", which blames the client for a mis-set constant. Fail here instead,
	// naming the real problem.
	if step <= 0 || rampTo <= rampFrom || (rampTo-rampFrom)%step != 0 {
		t.Fatalf("bad ramp constants: %d -> %d in steps of %d; step must be positive and divide the gap of %d exactly, "+
			"otherwise the loop never writes the final value",
			rampFrom, rampTo, step, rampTo-rampFrom)
	}

	h := newHarness(t)

	// ---- t0: the flag exists, at 70. --------------------------------------
	h.mustApply(t, rampBaseLayer(rampFrom))
	requireEqual(t, rampValueOnServer(t, h, envProd), rampFrom, "server's starting value")
	genStart := h.generationIn(t, envProd)

	cl, src, lg := newRampClient(t, h, envProd)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !cl.WaitForReady(readyCtx) {
		t.Fatalf("client never became ready; state %s", cl.State())
	}

	first := cl.IntDetail(context.Background(), rampKey, callerDefaultInt, core.EvalContext{UserID: "user-4711"})
	if v, _ := first.Value.AsInt(); v != rampFrom {
		t.Fatalf("client's first read is %d, want %d (reason %s, generation %d)",
			v, rampFrom, first.Reason, first.Generation)
	}
	requireEqual(t, first.Generation, genStart, "client's starting generation")

	// ---- the watcher runs underneath everything that follows. --------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newRampWatcher(cl)
	watchStart := time.Now()
	w.start(ctx)
	defer w.stop()

	// ---- the ramp. One write per step, and the loop refuses to move on
	//      until the client has actually read what was written (R1). --------
	converge := make([]time.Duration, 0, wantWrites)
	prevGen := genStart
	writes := 0

	for v := rampFrom + step; v <= rampTo; v += step {
		applied := h.mustApply(t, rampOverlay(envProd, v))
		gen := appliedGeneration(t, applied, envProd)
		if gen <= prevGen {
			t.Fatalf("write of %d did not advance the generation: previous %d, applied %d", v, prevGen, gen)
		}

		// R2. The operator's write landed before the client is asked for it, so
		// a convergence failure below cannot be blamed on the write path.
		if got := rampValueOnServer(t, h, envProd); got != v {
			t.Fatalf("wrote %d but the server resolves %s to %d", v, rampKey, got)
		}

		t0 := time.Now()
		mustWaitFor(t, 10*time.Second, fmt.Sprintf("the client to read %d (generation %d)", v, gen), func() bool {
			return cl.IntValue(ctx, rampKey, callerDefaultInt, core.EvalContext{UserID: "user-4711"}) == v
		})
		converge = append(converge, time.Since(t0))

		// The value and the generation must arrive together: a client serving
		// the new number from the old snapshot cannot answer "which config
		// decided this?", which is the whole point of stamping the generation.
		det := cl.IntDetail(ctx, rampKey, callerDefaultInt, core.EvalContext{UserID: "user-4711"})
		if got, _ := det.Value.AsInt(); got != v || det.Generation != gen {
			t.Errorf("after converging on %d the client reports value %d at generation %d, want %d at %d",
				v, got, det.Generation, v, gen)
		}

		prevGen = gen
		writes++
	}

	// The loop is done, but the watcher is not: R7 asserts the client SETTLES on
	// 700, which needs a run of samples at the terminal value rather than the one
	// the next tick happens to catch before cancellation.
	mustWaitFor(t, 10*time.Second, fmt.Sprintf("the watcher to record %d settled observations at %d", rampSettledTail, rampTo),
		func() bool { return w.tailCount(rampTo) >= rampSettledTail })

	cancel()
	w.stop()
	watchElapsed := time.Since(watchStart)

	requireEqual(t, writes, wantWrites, "writes issued by the loop")

	// ---- R8. The audit counter moved exactly once per write. --------------
	genFinal := h.generationIn(t, envProd)
	if got := genFinal - genStart; got != int64(wantWrites) {
		t.Errorf("R8: prod generation advanced %d times over %d writes (from %d to %d); "+
			"a mismatch means a write was dropped or the store churned",
			got, wantWrites, genStart, genFinal)
	}

	// ---- the sequence. ---------------------------------------------------
	obs := w.all()
	if len(obs) < 100 {
		t.Fatalf("the watcher recorded only %d observations; the sequence assertions would prove nothing", len(obs))
	}

	onRamp := func(v int64) bool {
		return v >= rampFrom && v <= rampTo && (v-rampFrom)%step == 0
	}

	// R5 and R6. Every observation is a real step, produced by the only legal
	// path a rules-free, rollout-free, enabled flag has.
	for i, o := range obs {
		if !onRamp(o.value) {
			t.Fatalf("R5: observation %d of %d carried %d, which is not a step on the %d..%d/%d ramp "+
				"(reason %s, generation %d) — the client fell back or served a torn value",
				i, len(obs), o.value, rampFrom, rampTo, step, o.reason, o.generation)
		}
		if o.reason != core.ReasonFallthrough {
			t.Fatalf("R6: observation %d carried reason %s, want FALLTHROUGH (value %d, generation %d)",
				i, o.reason, o.value, o.generation)
		}
		if o.bucket != core.NoBucket {
			t.Errorf("R6: observation %d reports bucket %d on a flag with no rollout, want %d",
				i, o.bucket, core.NoBucket)
		}
	}

	// R3 and R4. Order, over the whole run.
	regressions, genRegressions, drifts := 0, 0, 0
	for i := 1; i < len(obs); i++ {
		prev, cur := obs[i-1], obs[i]
		if cur.value < prev.value {
			if regressions == 0 {
				t.Errorf("R3: the value went BACKWARDS at observation %d: %d (generation %d) after %d (generation %d)",
					i, cur.value, cur.generation, prev.value, prev.generation)
			}
			regressions++
		}
		if cur.generation < prev.generation {
			if genRegressions == 0 {
				t.Errorf("R3: the generation went backwards at observation %d: %d after %d",
					i, cur.generation, prev.generation)
			}
			genRegressions++
		}
		if cur.value != prev.value && cur.generation == prev.generation {
			if drifts == 0 {
				t.Errorf("R4: the value changed from %d to %d at observation %d without the generation moving (%d): "+
					"that answer did not come from a published snapshot",
					prev.value, cur.value, i, cur.generation)
			}
			drifts++
		}
	}
	if regressions+genRegressions+drifts > 0 {
		t.Errorf("R3/R4 totals: %d value regressions, %d generation regressions, %d value/generation drifts",
			regressions, genRegressions, drifts)
	}

	// R7. Once the ramp is done, it stays done.
	tailFrom := -1
	for i := range obs {
		if obs[i].value == rampTo {
			tailFrom = i
			break
		}
	}
	if tailFrom < 0 {
		t.Errorf("R7: the watcher never observed the final value %d", rampTo)
	} else if n := len(obs) - tailFrom; n < rampSettledTail {
		t.Errorf("R7: only %d observations at the final value %d; want at least %d for \"settled\" to mean anything",
			n, rampTo, rampSettledTail)
	} else {
		for i := tailFrom; i < len(obs); i++ {
			if obs[i].value != rampTo {
				t.Errorf("R7: after reaching %d at observation %d the client flapped back to %d at observation %d",
					rampTo, tailFrom, obs[i].value, i)
				break
			}
		}
	}
	if final, _ := cl.IntDetail(context.Background(), rampKey, callerDefaultInt, core.EvalContext{UserID: "user-4711"}).Value.AsInt(); final != rampTo {
		t.Errorf("the client's final answer is %d, want %d", final, rampTo)
	}
	requireEqual(t, rampValueOnServer(t, h, envProd), rampTo, "server's final value")

	// ---- zero panics, from both sides of the boundary. --------------------
	if n := w.panics.Load(); n != 0 {
		t.Errorf("%d panic(s) escaped the SDK into the watcher loop", n)
	}
	if n := lg.count("flag.evaluation.panic"); n != 0 {
		t.Errorf("the SDK reported %d recovered evaluation panic(s)", n)
	}

	// ---- the report. -----------------------------------------------------
	distinct := map[int64]struct{}{}
	for _, o := range obs {
		distinct[o.value] = struct{}{}
	}
	perSample := time.Duration(0)
	if len(obs) > 0 {
		perSample = watchElapsed / time.Duration(len(obs))
	}

	// Written for whoever reads this in CI, not for whoever wrote the test: six
	// lines, every number next to what it means, and the one misleading number
	// (convergence, which is really the poll interval) labelled as such.
	t.Logf("WHAT HAPPENED\n"+
		"writes   %d: %d -> %d in steps of %d — the %d was already live, so %d values in all\n"+
		"app      saw all %d values, in order; the test waited for each, never sampled and hoped\n"+
		"service  version %d -> %d = one bump per write, so nothing dropped or double-published\n"+
		"proof    %d reads recorded (%s, one per ~%s): %d went backwards, %d lost the version,\n"+
		"         %d changed without one, 0 fell back to %d\n"+
		"speed    each change reached the app in ~%s — that is this test's %s poll, not SDK cost\n"+
		"waste    %d downloads for %d versions, one each",
		writes, rampFrom, rampTo, step, rampFrom, wantWrites+1,
		len(distinct),
		genStart, genFinal,
		len(obs), watchElapsed.Round(time.Millisecond), perSample.Round(time.Microsecond),
		regressions, genRegressions, drifts, callerDefaultInt,
		percentile(converge, 0.50).Round(100*time.Microsecond), rampSourcePoll,
		src.fetchCount(), genFinal-genStart+1)
}

// ---------------------------------------------------------------------------
// The same ramp, across a partition. CAP, stated as assertions.
//
// docs/05-consistency-and-e2e.md §1.3 names the choice: config distribution is
// AP WITH BOUNDED STALENESS. Under a partition the client keeps evaluating
// against last-known-good, and what is sacrificed is FRESHNESS, never
// INTEGRITY (G5).
//
// The suite already tests each half separately: the ramp above is many writes
// with no partition, and variants_test.go:300 is one partition with a static
// value. Neither covers the combination, which is the case an operator actually
// meets — the control plane goes away in the MIDDLE of a ramp. Three things can
// go wrong only there:
//
//   - the client quietly reverts to the value it started the day with, instead
//     of holding the step it had reached;
//   - a write issued into the partition returns success, so the operator
//     believes a limit was raised that no pod will ever serve;
//   - the client heals by replaying the ramp, so a value that was already
//     superseded is served again after recovery — a regression that a
//     partition, and only a partition, can introduce.
//
//	P1  the client keeps SERVING while partitioned                (A)
//	P2  what it serves is last-known-good, valid, never a default (A + G5)
//	P3  its generation does not move while partitioned            (G3)
//	P4  a write into the partition FAILS rather than lying        (write path is CP)
//	P5  after healing it converges on the FINAL value, no restart (G4)
//	P6  values and generations are monotone across the partition  (G3)
//	P7  skipping intermediate values while partitioned is LEGAL   (N3)
//
// P7 is the one that must not be over-asserted. A client that was unreachable
// for six steps cannot be required to have observed them; requiring it would be
// demanding linearizability, which §1.2 N1 explicitly refuses. The requirement
// is monotonicity, not completeness — so the skipped values are reported, not
// failed.
// ---------------------------------------------------------------------------

func TestRampAcrossAPartition_ServesLastKnownGoodThenConvergesOnTheFinalValue(t *testing.T) {
	step := rampStepSize()
	if step <= 0 || rampTo <= rampFrom || (rampTo-rampFrom)%step != 0 {
		t.Fatalf("bad ramp constants: %d -> %d in steps of %d; step must divide the gap of %d exactly",
			rampFrom, rampTo, step, rampTo-rampFrom)
	}

	// The partition lands three steps in, so last-known-good is NOT the value the
	// flag started at. A client that silently reverted to 70 would otherwise be
	// indistinguishable from one correctly holding its ground.
	partitionAt := rampFrom + 3*step
	if partitionAt >= rampTo {
		t.Skipf("ramp too short to partition in the middle: %d -> %d in steps of %d", rampFrom, rampTo, step)
	}

	ec := core.EvalContext{UserID: "user-4711", TenantID: "tenant-9"}

	h := newHarness(t)
	h.mustApply(t, rampBaseLayer(rampFrom))

	cl, src, lg := newRampClient(t, h, envProd)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !cl.WaitForReady(readyCtx) {
		t.Fatalf("client never became ready; state %s", cl.State())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := newRampWatcher(cl)
	w.start(ctx)
	defer w.stop()

	// ---- healthy ramp, up to the point of the partition. ------------------
	var lastGoodGen int64
	for v := rampFrom + step; v <= partitionAt; v += step {
		applied := h.mustApply(t, rampOverlay(envProd, v))
		lastGoodGen = appliedGeneration(t, applied, envProd)
		mustWaitFor(t, 10*time.Second, fmt.Sprintf("the client to read %d before the partition", v), func() bool {
			return cl.IntValue(ctx, rampKey, callerDefaultInt, ec) == v
		})
	}
	lastGood := partitionAt

	// ---- the control plane goes away, mid-ramp. ---------------------------
	atDeath := w.len()
	h.kill()

	mustWaitFor(t, 20*time.Second, "the client to report DEGRADED_STALE", func() bool {
		return cl.State() == client.StateDegradedStale
	})
	mustWaitFor(t, 20*time.Second, "500 observations while the service is unreachable", func() bool {
		return w.len() >= atDeath+500
	})

	// ---- P4. The write path is CP: it refuses rather than lies. -----------
	//
	// An operator who is told "applied" and gets nothing has been handed a false
	// belief about production, which is worse than an error they can retry.
	out, applyErr := h.apply(rampOverlay(envProd, lastGood+step))
	if applyErr == nil && out.status == http.StatusOK {
		t.Errorf("P4: a write into the partition reported success (%+v); "+
			"the operator now believes a value is live that no client will ever serve", out.result)
	} else {
		t.Logf("the operator's write into the dead service failed, as it must: err=%v status=%d", applyErr, out.status)
	}

	// A few hundred more samples AFTER the failed write, so P2 also covers
	// "the failed write did not disturb what the client serves".
	afterFailedWrite := w.len()
	mustWaitFor(t, 20*time.Second, "200 observations after the failed write", func() bool {
		return w.len() >= afterFailedWrite+200
	})
	beforeRevive := w.len()

	// ---- P1, P2, P3. What the client did while it was cut off. ------------
	if !cl.Ready() {
		t.Errorf("P1: the client stopped being ready during a control-plane outage; a stale pod is a working pod")
	}
	during := w.all()[atDeath:beforeRevive]
	if len(during) < 500 {
		t.Fatalf("only %d observations inside the partition window; the availability claim is unproven", len(during))
	}
	for i, o := range during {
		if o.value != lastGood {
			t.Fatalf("P2: observation %d of %d inside the partition carried %d, want last-known-good %d "+
				"(reason %s, generation %d) — the client did not hold its ground",
				i, len(during), o.value, lastGood, o.reason, o.generation)
		}
		if o.reason != core.ReasonFallthrough {
			t.Fatalf("P2: observation %d inside the partition carried reason %s, want FALLTHROUGH: "+
				"freshness may be sacrificed, integrity may not", i, o.reason)
		}
		if o.generation != lastGoodGen {
			t.Errorf("P3: observation %d inside the partition reports generation %d, want %d frozen",
				i, o.generation, lastGoodGen)
		}
	}
	// ---- the partition heals, and the rest of the ramp is pushed. ---------
	h.revive()

	// The kill hijacked and closed sockets, so the operator's first attempt after
	// recovery can land on a dead keep-alive connection. That is the operator's
	// retry, not a finding about the service.
	h.hc.CloseIdleConnections()

	reviveIdx := w.len()
	writesAfter := 0
	var finalGen int64
	for v := partitionAt + step; v <= rampTo; v += step {
		applied := h.mustApply(t, rampOverlay(envProd, v))
		finalGen = appliedGeneration(t, applied, envProd)
		writesAfter++
	}

	// ---- P5. Converge on the FINAL value, with no restart. ----------------
	mustWaitFor(t, 25*time.Second, fmt.Sprintf("the client to converge on %d after recovery", rampTo), func() bool {
		return cl.IntValue(ctx, rampKey, callerDefaultInt, ec) == rampTo
	})
	mustWaitFor(t, 20*time.Second, "the client to report HEALTHY again", func() bool {
		return cl.State() == client.StateHealthy
	})
	mustWaitFor(t, 10*time.Second, fmt.Sprintf("%d settled observations at %d", rampSettledTail, rampTo), func() bool {
		return w.tailCount(rampTo) >= rampSettledTail
	})

	cancel()
	w.stop()

	// ---- P6 and G5, over the WHOLE run including the partition. -----------
	obs := w.all()
	onRamp := func(v int64) bool {
		return v >= rampFrom && v <= rampTo && (v-rampFrom)%step == 0
	}
	for i, o := range obs {
		if !onRamp(o.value) {
			t.Fatalf("G5: observation %d carried %d, which is not a step on the ramp (reason %s): "+
				"a partition must cost freshness, never integrity", i, o.value, o.reason)
		}
		if o.reason != core.ReasonFallthrough {
			t.Fatalf("G5: observation %d carried reason %s, want FALLTHROUGH", i, o.reason)
		}
	}
	regressions, genRegressions := 0, 0
	for i := 1; i < len(obs); i++ {
		if obs[i].value < obs[i-1].value {
			if regressions == 0 {
				t.Errorf("P6: the value regressed at observation %d: %d after %d — the partition replayed the ramp",
					i, obs[i].value, obs[i-1].value)
			}
			regressions++
		}
		if obs[i].generation < obs[i-1].generation {
			if genRegressions == 0 {
				t.Errorf("P6: the generation regressed at observation %d: %d after %d",
					i, obs[i].generation, obs[i-1].generation)
			}
			genRegressions++
		}
	}

	// The final state, from both ends.
	requireEqual(t, rampValueOnServer(t, h, envProd), rampTo, "server's final value")
	det := cl.IntDetail(context.Background(), rampKey, callerDefaultInt, ec)
	if v, _ := det.Value.AsInt(); v != rampTo || det.Generation != finalGen {
		t.Errorf("P5: the client's final answer is %d at generation %d, want %d at %d",
			v, det.Generation, rampTo, finalGen)
	}

	if n := w.panics.Load(); n != 0 {
		t.Errorf("%d panic(s) escaped the SDK into the watcher loop", n)
	}
	if n := lg.count("flag.evaluation.panic"); n != 0 {
		t.Errorf("the SDK reported %d recovered evaluation panic(s)", n)
	}

	// ---- P7. Report what was skipped; do not fail it. ---------------------
	skipped := map[int64]struct{}{}
	for _, o := range obs[reviveIdx:] {
		skipped[o.value] = struct{}{}
	}
	t.Logf("WHAT HAPPENED\n"+
		"setup      the same %d -> %d ramp, but the service was killed 3 steps in (limit %d, version %d)\n"+
		"cut off    %d answers served, every one of them %d — not %d (no revert), not %d (no fallback), state %s\n"+
		"writes     the operator's write into the dead service failed, as it must\n"+
		"recovered  reconnected with no restart, landed on %d (version %d)\n"+
		"           jumped %d -> %d rather than replaying the %d it missed, so %d downloads here vs %d healthy\n"+
		"order      %d backwards in value, %d in version — a partition may cost you steps, never reorder them",
		rampFrom, rampTo, lastGood, lastGoodGen,
		len(during), lastGood, rampFrom, callerDefaultInt, client.StateDegradedStale,
		rampTo, finalGen,
		lastGood, rampTo, writesAfter, src.fetchCount(), (rampTo-rampFrom)/step+1,
		regressions, genRegressions)
}

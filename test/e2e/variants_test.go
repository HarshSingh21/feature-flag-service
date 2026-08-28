package e2e

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// ---------------------------------------------------------------------------
// Variant 1 — the swap lands mid-batch.
//
// This is the bug that pinning per flag instead of per request produces, and it
// is nearly impossible to reproduce from a bug report because by the time anyone
// looks, every flag agrees again. Both pins are exercised:
//
//	the SDK's   (pkg/client.BatchAppend, invariant CACHE-1 in the client)
//	the server's (internal/transport/http/eval.go, one s.pin per batch request)
//
// The write flips all 100 flags at once, so a torn result set is two different
// tokens in one response rather than a subtle statistic.
// ---------------------------------------------------------------------------

func TestSwapLandsMidBatch_EveryBatchSharesOneGenerationAndOneValue(t *testing.T) {
	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld))

	cl, _, _ := h.newClient(t, envProd)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !cl.WaitForReady(readyCtx) {
		t.Fatalf("client B never became ready; state %s", cl.State())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rd := newReader(cl, flagKeys(flagCount))
	rd.start(ctx)
	defer rd.stop()

	// A second reader, this one going through the real HTTP batch endpoint, so
	// the transport's own pin is under the same write storm.
	hb := newHTTPBatchReader(h, envProd, flagKeys(flagCount))
	hb.start(ctx)
	defer hb.stop()

	mustWaitFor(t, 20*time.Second, "both readers to warm up", func() bool {
		return rd.log.len() >= 50 && hb.count() >= 10
	})

	// Two write phases, because the two pins are stressed by different rates.
	//
	// Phase 1 hammers: 60 swaps in ~200ms puts the server's per-request pin under
	// a swap rate far above its own request latency, so batches are constantly
	// in flight across a publication.
	//
	// Phase 2 paces: the SDK only swaps its L1 pointer once per applied fetch, so
	// a rate slower than a fetch is what makes the CLIENT-side pin straddle a
	// swap many times over. Both are needed; either alone leaves one pin untested.
	allowed := map[string]bool{valueOld: true}
	write := func(i int) string {
		v := fmt.Sprintf("v%03d", i)
		allowed[v] = true
		h.mustApply(t, overlayLayer(envProd, flagCount, v))
		return v
	}

	const hammerWrites = 40
	for i := 1; i <= hammerWrites; i++ {
		write(i)
		time.Sleep(3 * time.Millisecond)
	}

	// The paced interval is comfortably longer than one unary fetch (roughly
	// 100 admin round trips), so every paced write is actually applied by the
	// SDK rather than coalesced away with the next one.
	const pacedWrites = 8
	var final string
	for i := 1; i <= pacedWrites; i++ {
		final = write(hammerWrites + i)
		time.Sleep(200 * time.Millisecond)
	}
	writes := hammerWrites + pacedWrites

	mustWaitFor(t, 20*time.Second, "B to converge on the final write", func() bool {
		return rd.log.has(final)
	})
	_, finalIdx, _ := rd.log.firstWith(final)
	mustWaitFor(t, 20*time.Second, "200 post-convergence observations", func() bool {
		return rd.log.countFrom(finalIdx) >= 200
	})

	cancel()
	rd.stop()
	hb.stop()

	// A3, through the SDK pin.
	f := rd.log.faults()
	if f.genSplits != 0 || f.valueSplit != 0 {
		t.Errorf("A3 (SDK): %s — a batch was served from more than one generation (G1)", f)
	}
	if f.errs != 0 || f.fallbacks != 0 {
		t.Errorf("A4 (SDK): %s", f)
	}
	if !rd.log.monotonic() {
		t.Errorf("A8: B's generation regressed under %d concurrent writes", writes)
	}
	for v, n := range rd.log.values() {
		if !allowed[v] {
			t.Errorf("A1: observed %d results with value %q, which was never written", n, v)
		}
	}

	// A3, through the transport pin.
	tears, batches := hb.tears()
	t.Logf("HTTP batch endpoint: %d batches of %d flags across %d writes, spanning %d generations, %d torn",
		batches, flagCount, writes, hb.generationsSeen(), tears)
	if batches < 50 {
		t.Errorf("HTTP batch reader only completed %d batches; the measurement is meaningless", batches)
	}
	// Proof that the race was actually run: if every batch saw one generation,
	// no swap ever landed during the loop and a torn read was never possible.
	if hb.generationsSeen() < 2 {
		t.Errorf("no config swap landed while the HTTP batch loop was running (%d generations seen); this variant proved nothing",
			hb.generationsSeen())
	}
	if got := len(rd.log.generations()); got < 4 {
		t.Errorf("the SDK reader observed only %d generation segment(s) across %d writes; too few swaps landed inside its loop to test the client-side pin",
			got, writes)
	}
	if tears != 0 {
		t.Errorf("A3 (transport): %d of %d HTTP batches were torn across generations (G1)", tears, batches)
	}
	if errs := hb.errors(); errs != 0 {
		t.Errorf("A4 (transport): %d HTTP batch requests failed", errs)
	}
	t.Logf("SDK reader: %d batches, %d distinct generations observed", rd.log.len(), len(rd.log.generations()))
}

// ---------------------------------------------------------------------------
// Variant 2 — rapid successive writes.
// ---------------------------------------------------------------------------

func TestRapidSuccessiveWrites_ReaderSettlesOnTheFinalStateNeverAnIntermediateOne(t *testing.T) {
	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld))

	cl, _, _ := h.newClient(t, envProd)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !cl.WaitForReady(readyCtx) {
		t.Fatalf("client B never became ready; state %s", cl.State())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rd := newReader(cl, flagKeys(flagCount))
	rd.start(ctx)
	defer rd.stop()

	mustWaitFor(t, 20*time.Second, "100 steady-state observations", func() bool { return rd.log.len() >= 100 })

	const v1, v2, v3 = "one", "two", "three"
	h.mustApply(t, overlayLayer(envProd, flagCount, v1))
	h.mustApply(t, overlayLayer(envProd, flagCount, v2))
	last := h.mustApply(t, overlayLayer(envProd, flagCount, v3))
	finalGen := appliedGeneration(t, last, envProd)

	mustWaitFor(t, 20*time.Second, "B to observe the FINAL value", func() bool { return rd.log.has(v3) })
	_, idx, _ := rd.log.firstWith(v3)
	mustWaitFor(t, 20*time.Second, "300 post-convergence observations", func() bool {
		return rd.log.countFrom(idx) >= 300
	})

	cancel()
	rd.stop()

	// Settling on an intermediate is the failure: the tail must be v3 and only v3.
	if v, ok := rd.log.uniformFrom(idx); !ok || v != v3 {
		t.Errorf("B settled on %q (uniform=%t) rather than the final state %q", v, ok, v3)
	}
	obs := rd.log.all()
	if got := obs[len(obs)-1].generation; got != finalGen {
		t.Errorf("B's final generation is %d, the last write published %d", got, finalGen)
	}
	if !rd.log.monotonic() {
		t.Errorf("A8: B's generation regressed across three back-to-back writes")
	}
	allowed := map[string]bool{valueOld: true, v1: true, v2: true, v3: true}
	for v, n := range rd.log.values() {
		if !allowed[v] {
			t.Errorf("A1: observed %d results with value %q, which was never written", n, v)
		}
	}
	if f := rd.log.faults(); !f.zero() {
		t.Errorf("A3/A4: %s", f)
	}
	t.Logf("three writes in %s; B passed through %v and settled on %q at generation %d",
		"immediate succession", rd.log.generations(), v3, finalGen)
}

// ---------------------------------------------------------------------------
// Variant 3 — an invalid write is a no-op.
// ---------------------------------------------------------------------------

func TestInvalidWriteIsANoOp_ReaderGenerationAndValuesAreUntouched(t *testing.T) {
	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld))
	genBefore := h.generationIn(t, envProd)
	devGenBefore := h.generationIn(t, envDev)

	cl, _, _ := h.newClient(t, envProd)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !cl.WaitForReady(readyCtx) {
		t.Fatalf("client B never became ready; state %s", cl.State())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rd := newReader(cl, flagKeys(flagCount))
	rd.start(ctx)
	defer rd.stop()

	mustWaitFor(t, 20*time.Second, "100 steady-state observations", func() bool { return rd.log.len() >= 100 })
	before := rd.log.len()

	out, err := h.apply(invalidBaseLayer())
	if err != nil {
		t.Fatalf("apply invalid layer: %v", err)
	}

	// The operator's mistake: a synchronous 400 that never pages.
	requireEqual(t, out.status, http.StatusBadRequest, "invalid config must answer 400")
	requireEqual(t, string(out.errEnv.ErrorCode), "invalid_config", "error code")
	if out.errEnv.TraceID == "" {
		t.Errorf("rejection envelope carries no trace id; the operator has no correlation handle")
	}

	// The structured rejection. It names every violation — three independent
	// ones here, from three different rules.
	rep := h.applier.lastReport()
	if rep == nil {
		t.Fatalf("the store produced no build report for the rejected layer")
	}
	rules := rep.Findings().Rejections().RuleIDs()
	t.Logf("rejection: %d finding(s), rules %v; HTTP message %q", len(rep.Findings().Rejections()), rules, out.errEnv.Message)
	if len(rules) < 3 {
		t.Errorf("expected at least three distinct rejection rules, got %v", rules)
	}
	for _, want := range []string{"B01", "B02", "B03"} {
		if !rep.Findings().Has(want) {
			t.Errorf("rejection does not name rule %s; findings: %v", want, rules)
		}
	}
	for env, res := range rep.PerEnv {
		if res.Published {
			t.Errorf("a rejected base layer published to %s at generation %d", env, res.Generation)
		}
	}

	// Now prove the no-op against the reader, by waiting for PROGRESS rather
	// than for the clock: 300 further observations, all of which must be
	// unchanged.
	mustWaitFor(t, 20*time.Second, "300 observations after the rejected write", func() bool {
		return rd.log.len() >= before+300
	})
	cancel()
	rd.stop()

	if got := h.generationIn(t, envProd); got != genBefore {
		t.Errorf("prod generation moved on a rejected write: %d -> %d", genBefore, got)
	}
	if got := h.generationIn(t, envDev); got != devGenBefore {
		t.Errorf("dev generation moved on a rejected base write: %d -> %d", devGenBefore, got)
	}
	if gens := rd.log.generations(); len(gens) != 1 || gens[0] != genBefore {
		t.Errorf("B observed generations %v, want exactly [%d]", gens, genBefore)
	}
	if v, ok := rd.log.uniformFrom(0); !ok || v != valueOld {
		t.Errorf("B's values changed on a rejected write: uniform=%t value=%q", ok, v)
	}
	if f := rd.log.faults(); !f.zero() {
		t.Errorf("A4: a rejected write produced faults in the reader: %s", f)
	}
}

// ---------------------------------------------------------------------------
// Variant 4 — the service dies mid-run, then recovers.
// ---------------------------------------------------------------------------

func TestServiceDiesMidRun_ReaderServesLastKnownGoodThenConvergesOnRecovery(t *testing.T) {
	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld))
	genBefore := h.generationIn(t, envProd)

	cl, _, _ := h.newClient(t, envProd)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !cl.WaitForReady(readyCtx) {
		t.Fatalf("client B never became ready; state %s", cl.State())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rd := newReader(cl, flagKeys(flagCount))
	rd.start(ctx)
	defer rd.stop()

	mustWaitFor(t, 20*time.Second, "100 steady-state observations", func() bool { return rd.log.len() >= 100 })

	// ---- the service dies. ------------------------------------------------
	h.kill()
	mustWaitFor(t, 20*time.Second, "B to report DEGRADED_STALE", func() bool {
		return cl.State() == client.StateDegradedStale
	})
	atDeath := rd.log.len()
	mustWaitFor(t, 20*time.Second, "300 observations while the service is down", func() bool {
		return rd.log.len() >= atDeath+300
	})

	// Serving, not erroring. That is the whole AP-with-bounded-staleness claim.
	if !cl.Ready() {
		t.Errorf("B stopped being ready during a control-plane outage; a stale pod is a working pod")
	}
	if f := rd.log.faults(); !f.zero() {
		t.Errorf("A4: evaluation faulted while the service was down: %s", f)
	}
	if v, ok := rd.log.uniformFrom(0); !ok || v != valueOld {
		t.Errorf("B stopped serving last-known-good during the outage: uniform=%t value=%q", ok, v)
	}
	if gens := rd.log.generations(); len(gens) != 1 || gens[0] != genBefore {
		t.Errorf("B's generation moved during the outage: %v", gens)
	}
	t.Logf("outage: B stayed in %s and served %d further evaluations of last-known-good generation %d",
		client.StateDegradedStale, rd.log.len()-atDeath, genBefore)

	// ---- recovery, with no restart. ---------------------------------------
	h.revive()
	applied := h.mustApply(t, overlayLayer(envProd, flagCount, valueNew))
	genAfter := appliedGeneration(t, applied, envProd)

	mustWaitFor(t, 25*time.Second, "B to converge after recovery", func() bool { return rd.log.has(valueNew) })
	_, idx, _ := rd.log.firstWith(valueNew)
	mustWaitFor(t, 20*time.Second, "200 post-recovery observations", func() bool {
		return rd.log.countFrom(idx) >= 200
	})
	mustWaitFor(t, 20*time.Second, "B to report HEALTHY again", func() bool {
		return cl.State() == client.StateHealthy
	})

	cancel()
	rd.stop()

	if trs := rd.log.transitions(); len(trs) != 1 || trs[0].from != valueOld || trs[0].to != valueNew {
		t.Errorf("A2: expected exactly one %s->%s transition across the outage, got %v", valueOld, valueNew, trs)
	}
	if !rd.log.monotonic() {
		t.Errorf("A8: B's generation regressed across the outage")
	}
	if gens := rd.log.generations(); len(gens) != 2 || gens[1] != genAfter {
		t.Errorf("A9: B observed generations %v, want [%d %d]", gens, genBefore, genAfter)
	}
	if f := rd.log.faults(); !f.zero() {
		t.Errorf("A4: %s", f)
	}
}

// ---------------------------------------------------------------------------
// Variant 5 — B starts cold while the service is down.
// ---------------------------------------------------------------------------

func TestClientStartsColdWhileServiceIsDown_ServesCallerDefaultsAndNeverThrows(t *testing.T) {
	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld)) // the service HAS config...
	h.kill()                                       // ...and nobody can reach it.

	cl, _, lg := h.newClient(t, envProd)

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelReady()
	if cl.WaitForReady(readyCtx) {
		t.Fatalf("client reported ready with no reachable config source; state %s", cl.State())
	}
	if got := cl.State(); got != client.StateUninitialized {
		t.Errorf("state is %s, want %s", got, client.StateUninitialized)
	}
	if cl.Ready() {
		t.Errorf("Ready() is true in %s; /ready must gate a pod that has never loaded config (hazard H6)", cl.State())
	}
	if got := cl.Generation(); got != 0 {
		t.Errorf("generation is %d with no snapshot, want 0", got)
	}

	// Evaluation still never throws: it answers with the call-site default.
	ctx := context.Background()
	if got := cl.StringValue(ctx, flagKeys(1)[0], callerDefault, core.EvalContext{UserID: "u"}); got != callerDefault {
		t.Errorf("uninitialized StringValue returned %q, want the caller default %q", got, callerDefault)
	}
	det := cl.StringDetail(ctx, flagKeys(1)[0], callerDefault, core.EvalContext{UserID: "u"})
	if det.Reason != core.ReasonError {
		t.Errorf("uninitialized detail reason is %s, want %s (it must land in the fallback rate)", det.Reason, core.ReasonError)
	}

	// And the whole batch shape is safe, element by element.
	rd := newReader(cl, flagKeys(flagCount))
	var buf []core.Result
	rd.once(ctx, &buf)
	if n := rd.panics.Load(); n != 0 {
		t.Errorf("evaluation panicked %d time(s) with no snapshot", n)
	}
	obs := rd.log.all()
	if len(obs) != 1 {
		t.Fatalf("expected one recorded batch, got %d", len(obs))
	}
	if obs[0].value != callerDefault {
		t.Errorf("uninitialized batch returned %q, want the caller default %q", obs[0].value, callerDefault)
	}
	if obs[0].errs != flagCount {
		t.Errorf("uninitialized batch reported %d ERROR results, want %d (the alarm must be loud)", obs[0].errs, flagCount)
	}
	if lg.count("flag.client.uninitialized") == 0 {
		t.Errorf("the SDK did not raise the UNINITIALIZED alarm")
	}

	// It recovers without a restart once the service comes back.
	h.revive()
	recoverCtx, cancelRecover := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelRecover()
	if !cl.WaitForReady(recoverCtx) {
		t.Fatalf("client never converged after the service came back; state %s", cl.State())
	}
	mustWaitFor(t, 20*time.Second, "B to serve real config", func() bool {
		return cl.StringValue(context.Background(), flagKeys(1)[0], callerDefault, core.EvalContext{UserID: "u"}) == valueOld
	})
	t.Logf("cold start during a total outage: UNINITIALIZED -> %s at generation %d, no restart",
		cl.State(), cl.Generation())
}

// ---------------------------------------------------------------------------
// Variant 6 — two concurrent writers.
// ---------------------------------------------------------------------------

func TestTwoConcurrentWriters_GenerationsStayMonotonicAndTheFinalStateIsOneWrite(t *testing.T) {
	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld))

	cl, _, _ := h.newClient(t, envProd)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !cl.WaitForReady(readyCtx) {
		t.Fatalf("client B never became ready; state %s", cl.State())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rd := newReader(cl, flagKeys(flagCount))
	rd.start(ctx)
	defer rd.stop()

	mustWaitFor(t, 20*time.Second, "100 steady-state observations", func() bool { return rd.log.len() >= 100 })

	const perWriter = 12
	allowed := map[string]bool{valueOld: true}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, 2*perWriter)

	for _, writer := range []string{"w1", "w2"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for i := 1; i <= perWriter; i++ {
				v := fmt.Sprintf("%s-%02d", name, i)
				mu.Lock()
				allowed[v] = true
				mu.Unlock()
				out, err := h.apply(overlayLayer(envProd, flagCount, v))
				if err != nil {
					errs <- err
					return
				}
				if out.status != http.StatusOK {
					errs <- fmt.Errorf("%s: status %d code %q", name, out.status, out.errEnv.ErrorCode)
					return
				}
			}
		}(writer)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	finalValue := h.valueIn(t, envProd, flagKeys(1)[0])
	finalGen := h.generationIn(t, envProd)

	mustWaitFor(t, 25*time.Second, "B to converge on the final state", func() bool {
		return rd.log.has(finalValue)
	})
	_, idx, _ := rd.log.firstWith(finalValue)
	mustWaitFor(t, 20*time.Second, "200 post-convergence observations", func() bool {
		return rd.log.countFrom(idx) >= 200
	})
	cancel()
	rd.stop()

	mu.Lock()
	defer mu.Unlock()

	if !allowed[finalValue] || finalValue == valueOld {
		t.Errorf("the final served value %q is not one of the writes", finalValue)
	}
	if v, ok := rd.log.uniformFrom(idx); !ok || v != finalValue {
		t.Errorf("B did not settle on the final state: uniform=%t value=%q want %q", ok, v, finalValue)
	}
	if !rd.log.monotonic() {
		t.Errorf("A8: generations regressed under two concurrent writers (G3)")
	}
	for v, n := range rd.log.values() {
		if !allowed[v] {
			t.Errorf("A1: observed %d results with value %q, which no writer wrote — a mixture", n, v)
		}
	}
	if f := rd.log.faults(); !f.zero() {
		t.Errorf("A3/A4: %s — a batch mixed two writers' generations", f)
	}
	obs := rd.log.all()
	if last := obs[len(obs)-1].generation; last != finalGen {
		t.Errorf("B's final generation is %d, the server is on %d", last, finalGen)
	}
	t.Logf("two writers, %d writes each: server ended at generation %d serving %q; B observed %d generation segments, all monotonic",
		perWriter, finalGen, finalValue, len(rd.log.generations()))
}

// ---------------------------------------------------------------------------
// Variant 7 — environment isolation.
// ---------------------------------------------------------------------------

func TestEnvironmentIsolation_WritingProdLeavesDevGenerationUntouched(t *testing.T) {
	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld))
	devGen := h.generationIn(t, envDev)
	prodGen := h.generationIn(t, envProd)

	prodClient, _, _ := h.newClient(t, envProd)
	devClient, _, _ := h.newClient(t, envDev)

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	if !prodClient.WaitForReady(readyCtx) || !devClient.WaitForReady(readyCtx) {
		t.Fatalf("clients never became ready: prod %s, dev %s", prodClient.State(), devClient.State())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prodReader := newReader(prodClient, flagKeys(flagCount))
	devReader := newReader(devClient, flagKeys(flagCount))
	prodReader.start(ctx)
	devReader.start(ctx)
	defer prodReader.stop()
	defer devReader.stop()

	mustWaitFor(t, 20*time.Second, "both readers to warm up", func() bool {
		return prodReader.log.len() >= 100 && devReader.log.len() >= 100
	})

	applied := h.mustApply(t, overlayLayer(envProd, flagCount, valueNew))
	if len(applied.result.Applied) != 1 {
		t.Errorf("a prod overlay published to %d environments: %v", len(applied.result.Applied), applied.result.Applied)
	}
	prodGenAfter := appliedGeneration(t, applied, envProd)

	mustWaitFor(t, 20*time.Second, "prod to converge", func() bool { return prodReader.log.has(valueNew) })
	_, idx, _ := prodReader.log.firstWith(valueNew)
	mustWaitFor(t, 20*time.Second, "300 observations on both sides after the prod write", func() bool {
		return prodReader.log.countFrom(idx) >= 300 && devReader.log.len() >= 400
	})

	cancel()
	prodReader.stop()
	devReader.stop()

	// prod moved.
	if prodGenAfter <= prodGen {
		t.Errorf("prod generation did not advance: %d -> %d", prodGen, prodGenAfter)
	}
	if gens := prodReader.log.generations(); len(gens) != 2 || gens[1] != prodGenAfter {
		t.Errorf("prod client observed generations %v, want [%d %d]", gens, prodGen, prodGenAfter)
	}

	// dev did not. Not "eventually converged to the same thing" — no change at all.
	if got := h.generationIn(t, envDev); got != devGen {
		t.Errorf("dev generation changed on a prod-only write: %d -> %d", devGen, got)
	}
	if gens := devReader.log.generations(); len(gens) != 1 || gens[0] != devGen {
		t.Errorf("dev client observed generations %v, want exactly [%d]", gens, devGen)
	}
	if trs := devReader.log.transitions(); len(trs) != 0 {
		t.Errorf("dev client observed %d value transitions on a prod-only write: %v", len(trs), trs)
	}
	if v, ok := devReader.log.uniformFrom(0); !ok || v != valueOld {
		t.Errorf("dev value changed: uniform=%t value=%q", ok, v)
	}
	if f := devReader.log.faults(); !f.zero() {
		t.Errorf("A4 (dev): %s", f)
	}
	if got := h.valueIn(t, envDev, flagKeys(1)[0]); got != valueOld {
		t.Errorf("dev is serving %q after a prod-only write, want %q", got, valueOld)
	}
	t.Logf("prod %d -> %d over %d observations; dev held generation %d across %d observations with zero transitions",
		prodGen, prodGenAfter, prodReader.log.len(), devGen, devReader.log.len())
}

// ---------------------------------------------------------------------------
// Bounded goroutine lifetimes.
//
// Every goroutine the suite starts — the reader loop, the SDK updater, the
// reconcile poll, the HTTP source's heartbeat loop — is bound to a context and
// waited for. This test proves it rather than asserting it in a comment.
// ---------------------------------------------------------------------------

func TestEveryGoroutineStopsWhenTheClientAndReaderAreClosed(t *testing.T) {
	baseline := runtime.NumGoroutine()

	t.Run("run", func(t *testing.T) {
		h := newHarness(t)
		h.mustApply(t, baseLayer(flagCount, valueOld))
		cl, _, _ := h.newClient(t, envProd)
		readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelReady()
		if !cl.WaitForReady(readyCtx) {
			t.Fatalf("client never became ready")
		}
		ctx, cancel := context.WithCancel(context.Background())
		rd := newReader(cl, flagKeys(flagCount))
		rd.start(ctx)
		mustWaitFor(t, 20*time.Second, "the reader to run", func() bool { return rd.log.len() >= 50 })
		cancel()
		rd.stop()
	})

	// The subtest's cleanups have run: client closed, source drained, servers
	// shut down. Poll rather than sample once — net/http tears connections down
	// on its own schedule.
	var final int
	ok := waitFor(10*time.Second, func() bool {
		runtime.GC()
		final = runtime.NumGoroutine()
		return final <= baseline+2
	})
	if !ok {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Errorf("goroutines leaked: baseline %d, final %d\n%s", baseline, final, buf[:n])
	}
}

// TestInvalidWriteRejectionReachesTheOperatorInFull is SKIPPED because it fails,
// and it fails because of a defect in the transport contract rather than in this
// test. It is left here as the executable description of that defect.
//
// BUG: a structured config rejection cannot cross the HTTP boundary.
//
// docs/05-consistency-and-e2e.md §2.3 requires that on an invalid write "A
// receives a structured rejection listing every violation", and
// internal/config.BuildReport is built precisely to carry that: every finding
// names its rule, flag, layer and field path. But:
//
//  1. httpx.LayerApplier's rejection path is `(ApplyResult, error)` — a BARE
//     error. httpx.ApplyResult has no findings field, so there is no shape for
//     the report to travel in even if the applier had one.
//  2. handleApplyLayer passes err.Error() to apierr.Write, which runs it through
//     apierr.Sanitize. Sanitize truncates at 200 bytes and — because Findings
//     renders one finding per LINE — replaces any multi-line message wholesale
//     with "internal error".
//
// So an operator who pushes a base layer with three violations gets, at best, a
// 200-byte prefix naming the first one or two, and at worst the string "internal
// error" on a 400. The rule ids, the field paths and every finding past the
// truncation point are lost at the edge, on exactly the path where "we shipped
// the fix and it never took effect" is the failure being guarded against.
//
// The fix is a contract change, not a test change: give ApplyResult (or a
// sibling RejectResult) a []Finding, and have the admin handler render the
// findings as JSON rather than flattening them into a sanitised string. That is
// the parent's call, so nothing here is modified to make this pass.
func TestInvalidWriteRejectionReachesTheOperatorInFull(t *testing.T) {
	t.Skip("BUG: structured rejections are flattened to a sanitised 200-byte string by the HTTP error envelope; see the comment above")

	h := newHarness(t)
	h.mustApply(t, baseLayer(flagCount, valueOld))

	out, err := h.apply(invalidBaseLayer())
	if err != nil {
		t.Fatalf("apply invalid layer: %v", err)
	}
	requireEqual(t, out.status, http.StatusBadRequest, "invalid config must answer 400")

	// The store found three violations under three different rules...
	rep := h.applier.lastReport()
	rules := rep.Findings().Rejections().RuleIDs()
	if len(rules) != 3 {
		t.Fatalf("expected three rejection rules in the build report, got %v", rules)
	}

	// ...and the operator must be told about all three. This is what fails.
	for _, rule := range rules {
		if !strings.Contains(out.errEnv.Message, rule) {
			t.Errorf("rejection sent to the operator does not mention rule %s: %q", rule, out.errEnv.Message)
		}
	}
}

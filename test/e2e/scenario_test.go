package e2e

import (
	"context"
	"testing"
	"time"
)

// TestWriterUpdatesWhileReaderEvaluates_ReaderNeverSeesTornState is the scenario
// of docs/05-consistency-and-e2e.md §2.1, and it carries assertions A1–A9.
//
// Two clients, no coordination between them:
//
//	A  operator     POST /v1/config/layers, admin listener, HTTP only
//	B  application  pkg/client.Client over an HTTP Source, evaluating a
//	                100-flag batch in a tight loop
//
// The write flips all 100 flags from OLD to NEW in one overlay, so a batch that
// was torn across the swap shows up as two tokens in one result set rather than
// as a statistic nobody can reproduce.
func TestWriterUpdatesWhileReaderEvaluates_ReaderNeverSeesTornState(t *testing.T) {
	h := newHarness(t)

	// ---- t0: the world is OLD, and B is caught up on it. -------------------
	h.mustApply(t, baseLayer(flagCount, valueOld))
	genBefore := h.generationIn(t, envProd)

	cl, src, lg := h.newClient(t, envProd)
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

	// ---- steady state: enough OLD observations for A6 to mean something. ---
	mustWaitFor(t, 20*time.Second, "300 steady-state observations", func() bool {
		return rd.log.len() >= 300
	})

	// ---- the write. -------------------------------------------------------
	setAt := time.Now()
	applied := h.mustApply(t, overlayLayer(envProd, flagCount, valueNew))
	setReturned := time.Now()

	genAfter := appliedGeneration(t, applied, envProd)
	if genAfter <= genBefore {
		t.Fatalf("server generation did not advance: before %d, after %d", genBefore, genAfter)
	}

	// ---- convergence, then enough NEW observations to prove no flap back. --
	mustWaitFor(t, 20*time.Second, "B to observe NEW", func() bool { return rd.log.has(valueNew) })
	firstNew, firstNewIdx, _ := rd.log.firstWith(valueNew)
	mustWaitFor(t, 20*time.Second, "300 post-convergence observations", func() bool {
		return rd.log.countFrom(firstNewIdx) >= 300
	})

	cancel()
	rd.stop()

	obs := rd.log.all()
	if len(obs) < 600 {
		t.Fatalf("observation log too short to prove anything: %d entries", len(obs))
	}

	// ---- A5. Reported first, because it is the number a regression moves. --
	convergence := firstNew.at.Sub(setReturned)
	t.Logf("A5 convergence delta: %s (Set returned at +0; B first observed NEW at +%s; %d unary fetches)",
		convergence, convergence, src.fetchCount())
	t.Logf("    observations: %d total, %d before the transition, %d after",
		len(obs), firstNewIdx, len(obs)-firstNewIdx)
	if convergence > 5*time.Second {
		t.Errorf("A5: convergence took %s, over the 5s budget (G4)", convergence)
	}
	if convergence < -time.Second {
		t.Errorf("A5: B observed NEW %s BEFORE Set returned, which means the clock or the log is wrong", -convergence)
	}

	// ---- A1. Every observed value is OLD or NEW. --------------------------
	for value, n := range rd.log.values() {
		if value != valueOld && value != valueNew {
			t.Errorf("A1: observed %d results with value %q; only %q and %q are reachable (G5)",
				n, value, valueOld, valueNew)
		}
	}

	// ---- A2. Exactly one OLD -> NEW transition, and no flap back. ----------
	trs := rd.log.transitions()
	if len(trs) != 1 {
		t.Errorf("A2: expected exactly one transition, got %d: %v (G3)", len(trs), trs)
	} else if trs[0].from != valueOld || trs[0].to != valueNew {
		t.Errorf("A2: transition was %s, want %s->%s", trs[0], valueOld, valueNew)
	}
	if v, ok := rd.log.uniformFrom(firstNewIdx); !ok || v != valueNew {
		t.Errorf("A2: the post-convergence segment is not uniformly %q (got %q, uniform=%t): B flapped back", valueNew, v, ok)
	}

	// ---- A3 and A4. -------------------------------------------------------
	f := rd.log.faults()
	if !f.zero() {
		t.Errorf("A3/A4: %s; every batch must share one generation and one value, with no fallback and no error", f)
	}
	if n := rd.panics.Load(); n != 0 {
		t.Errorf("A4: %d panic(s) escaped the SDK into the reader loop", n)
	}
	if n := lg.count("flag.evaluation.panic"); n != 0 {
		t.Errorf("A4: SDK reported %d recovered evaluation panic(s)", n)
	}

	// ---- A6. The pre-transition segment is uniformly OLD. ------------------
	if v, ok := rd.log.uniformBefore(firstNewIdx); !ok || v != valueOld {
		t.Errorf("A6: the pre-transition segment is not uniformly %q (got %q, uniform=%t): B saw OLD intermittently (G2, G3)",
			valueOld, v, ok)
	}

	// ---- A7. The swap must not be a latency event. ------------------------
	steadyP99, steadyN := rd.log.p99Between(obs[0].at, setAt)
	duringP99, duringN := rd.log.p99Between(setAt, firstNew.at.Add(100*time.Millisecond))
	steadyP50 := rd.log.p50Between(obs[0].at, setAt)
	duringP50 := rd.log.p50Between(setAt, firstNew.at.Add(100*time.Millisecond))
	t.Logf("A7 evaluation latency for a %d-flag batch: steady p50=%s p99=%s (n=%d) | during-swap p50=%s p99=%s (n=%d)",
		flagCount, steadyP50, steadyP99, steadyN, duringP50, duringP99, duringN)

	// The budget scales with steady state, so a loaded machine moves both
	// numbers together, and has an absolute floor so a sub-100µs steady p99
	// does not turn scheduler noise into a failure. An atomic pointer swap
	// cannot cost milliseconds: anything outside this is a reader being blocked
	// by a writer, which is precisely the failure A7 exists to catch.
	budget := 4 * steadyP99
	if budget < 2*time.Millisecond {
		budget = 2 * time.Millisecond
	}
	if duringN < 10 {
		t.Errorf("A7: only %d observations inside the swap window; the measurement is meaningless", duringN)
	} else if duringP99 > budget {
		t.Errorf("A7: during-swap p99 %s exceeds the budget %s (steady p99 %s): the config swap is visible as a latency event",
			duringP99, budget, steadyP99)
	}

	// ---- A8. Generation never regresses. ----------------------------------
	if !rd.log.monotonic() {
		t.Errorf("A8: B's generation regressed at some point in the run (G3)")
	}

	// ---- A9. It advances exactly once, to the server's post-write value. ---
	gens := rd.log.generations()
	t.Logf("A9 generations observed by B, in order: %v (server: %d before the write, %d after)",
		gens, genBefore, genAfter)
	if len(gens) != 2 {
		t.Errorf("A9: B observed %d generation segments %v, want exactly 2 (G3, G4)", len(gens), gens)
	} else {
		if gens[0] != genBefore {
			t.Errorf("A9: B started on generation %d, server was on %d", gens[0], genBefore)
		}
		if gens[1] != genAfter {
			t.Errorf("A9: B converged on generation %d, server's post-write generation is %d", gens[1], genAfter)
		}
	}
	if got := h.generationIn(t, envProd); got != genAfter {
		t.Errorf("A9: server reports generation %d, the apply reported %d", got, genAfter)
	}
}

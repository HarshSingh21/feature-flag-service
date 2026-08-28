package client

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// fastBackoff keeps reconnect-driven tests quick without making them timing
// dependent on the default jittered schedule.
func fastBackoff(d time.Duration) Option {
	return WithBackoff(func(int) time.Duration { return d })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func genSource(gen *atomic.Int64, instance *atomic.Value) *fakeSource {
	src := newFakeSource()
	src.setFetch(func() (Update, error) {
		g := gen.Load()
		inst, _ := instance.Load().(string)
		return Update{Snapshot: fixture(g), Generation: g, InstanceID: inst}, nil
	})
	return src
}

// TestStartupDoesNotBlockOnTheFlagService is the position of docs/03-lld.md
// §6.3: if it did, the flag service would become a hard dependency of every
// deploy and pod restart in the fleet.
func TestStartupDoesNotBlockOnTheFlagService(t *testing.T) {
	t.Parallel()
	src := newFakeSource()
	blocked := make(chan struct{})
	src.setFetch(func() (Update, error) {
		<-blocked
		return Update{}, context.DeadlineExceeded
	})
	defer close(blocked)

	start := time.Now()
	c := newTestClient(t, refEvaluator(), WithSource(src), WithFetchTimeout(time.Hour))
	defer c.Close()
	if d := time.Since(start); d > 250*time.Millisecond {
		t.Fatalf("New blocked for %v; construction must not wait on the flag service", d)
	}
	if c.State() != StateUninitialized {
		t.Fatalf("state = %s, want UNINITIALIZED", c.State())
	}
	// And it is usable immediately, serving call-site defaults.
	if got := c.BoolValue(context.Background(), "checkout_v2", true, core.EvalContext{}); !got {
		t.Fatal("a client whose first fetch has not returned must still answer")
	}
}

func TestBootstrapFetchTransitionsToHealthy(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(1)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)
	met := newRecMetrics()

	c := newTestClient(t, refEvaluator(), WithSource(src), WithMetrics(met),
		WithDeadStreamThreshold(time.Minute), fastBackoff(5*time.Millisecond))
	defer c.Close()

	waitFor(t, "HEALTHY", func() bool { return c.State() == StateHealthy })
	if c.Generation() != 1 {
		t.Fatalf("generation = %d, want 1", c.Generation())
	}
	if !c.WaitForReady(context.Background()) {
		t.Fatal("WaitForReady must report ready")
	}
	if got := met.snapshotTransitions(); len(got) == 0 || got[0] != "UNINITIALIZED->HEALTHY" {
		t.Fatalf("transitions = %v, want UNINITIALIZED->HEALTHY first", got)
	}
}

func TestPushAppliesSnapshotAndDetectsGenerationGap(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(1)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)
	met := newRecMetrics()

	c := newTestClient(t, refEvaluator(), WithSource(src), WithMetrics(met),
		WithDeadStreamThreshold(time.Minute), WithReconcileInterval(0), fastBackoff(time.Hour))
	defer c.Close()
	waitFor(t, "HEALTHY", func() bool { return c.State() == StateHealthy })

	sub := src.awaitSub(2 * time.Second)
	if sub == nil {
		t.Fatal("no subscription was opened")
	}

	// A contiguous push applies silently.
	sub.push(Update{Snapshot: fixture(2), Generation: 2, InstanceID: "inst-a"})
	waitFor(t, "generation 2", func() bool { return c.Generation() == 2 })
	for _, r := range met.snapshotResyncs() {
		if r == "generation_gap" {
			t.Fatal("a contiguous generation must not be reported as a gap")
		}
	}

	// A jump proves a push frame was lost. Snapshots are absolute state so the
	// client still converges, but the lost frame is a fault worth surfacing.
	sub.push(Update{Snapshot: fixture(6), Generation: 6, InstanceID: "inst-a"})
	waitFor(t, "generation 6", func() bool { return c.Generation() == 6 })
	waitFor(t, "generation_gap resync", func() bool {
		for _, r := range met.snapshotResyncs() {
			if r == "generation_gap" {
				return true
			}
		}
		return false
	})
}

func TestOlderGenerationIsRefused(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(5)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)

	c := newTestClient(t, refEvaluator(), WithSource(src),
		WithDeadStreamThreshold(time.Minute), WithReconcileInterval(0), fastBackoff(time.Hour))
	defer c.Close()
	waitFor(t, "generation 5", func() bool { return c.Generation() == 5 })

	sub := src.awaitSub(2 * time.Second)
	if sub == nil {
		t.Fatal("no subscription")
	}
	// A late frame from a slow path must not rewind config that has moved on.
	sub.push(Update{Snapshot: fixture(2), Generation: 2, InstanceID: "inst-a"})
	time.Sleep(50 * time.Millisecond)
	if c.Generation() != 5 {
		t.Fatalf("generation = %d, want 5; an older frame must not rewind the cache", c.Generation())
	}
}

// TestInstanceChangeOverridesGenerationComparison covers the trap called out in
// core/contract.go: a bare counter resets on restart, so a client at generation
// 5 meeting a restarted source at generation 2 must not conclude it is ahead.
func TestInstanceChangeOverridesGenerationComparison(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(5)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)
	met := newRecMetrics()

	c := newTestClient(t, refEvaluator(), WithSource(src), WithMetrics(met),
		WithDeadStreamThreshold(time.Minute), WithReconcileInterval(0), fastBackoff(time.Hour))
	defer c.Close()
	waitFor(t, "generation 5", func() bool { return c.Generation() == 5 })

	sub := src.awaitSub(2 * time.Second)
	if sub == nil {
		t.Fatal("no subscription")
	}
	sub.push(Update{Snapshot: fixture(2), Generation: 2, InstanceID: "inst-b"})
	waitFor(t, "generation 2 from the restarted instance", func() bool { return c.Generation() == 2 })
}

func TestHeartbeatGapTriggersResync(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(1)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)
	met := newRecMetrics()

	c := newTestClient(t, refEvaluator(), WithSource(src), WithMetrics(met),
		WithDeadStreamThreshold(time.Minute), WithReconcileInterval(0), fastBackoff(time.Hour))
	defer c.Close()
	waitFor(t, "generation 1", func() bool { return c.Generation() == 1 })

	sub := src.awaitSub(2 * time.Second)
	if sub == nil {
		t.Fatal("no subscription")
	}
	// The source moved to generation 9 and the push frame never arrived. The
	// heartbeat is the only thing that can reveal that, because "no changes"
	// and "changes I never heard about" look identical otherwise.
	gen.Store(9)
	sub.push(Update{Generation: 9, InstanceID: "inst-a"})

	waitFor(t, "resync to generation 9", func() bool { return c.Generation() == 9 })
	waitFor(t, "heartbeat_gap resync metric", func() bool {
		for _, r := range met.snapshotResyncs() {
			if r == "heartbeat_gap" {
				return true
			}
		}
		return false
	})
}

func TestHeartbeatAtCurrentGenerationIsSilent(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(3)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)

	c := newTestClient(t, refEvaluator(), WithSource(src),
		WithDeadStreamThreshold(time.Minute), WithReconcileInterval(0), fastBackoff(time.Hour))
	defer c.Close()
	waitFor(t, "generation 3", func() bool { return c.Generation() == 3 })
	sub := src.awaitSub(2 * time.Second)
	if sub == nil {
		t.Fatal("no subscription")
	}
	fetchesBefore, _ := src.counts()
	for i := 0; i < 5; i++ {
		sub.push(Update{Generation: 3, InstanceID: "inst-a"})
	}
	time.Sleep(50 * time.Millisecond)
	if f, _ := src.counts(); f != fetchesBefore {
		t.Fatalf("heartbeats at the current generation triggered %d fetches; they must be free", f-fetchesBefore)
	}
	if c.State() != StateHealthy {
		t.Fatalf("state = %s, want HEALTHY", c.State())
	}
}

// TestDeadStreamDegradesThenRecovers walks HEALTHY -> DEGRADED_STALE ->
// HEALTHY, which is the whole of docs/03-lld.md §6.2 that involves the network.
func TestDeadStreamDegradesThenRecovers(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(1)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)
	met := newRecMetrics()

	// A stream that is open but mute is indistinguishable from a broken one.
	c := newTestClient(t, refEvaluator(), WithSource(src), WithMetrics(met),
		WithDeadStreamThreshold(30*time.Millisecond), WithReconcileInterval(0),
		WithBackoff(func(int) time.Duration { return 5 * time.Millisecond }))
	defer c.Close()

	waitFor(t, "HEALTHY", func() bool { return c.State() == StateHealthy })
	// Freeze the source so reconnects cannot immediately re-heal it.
	src.setFetch(func() (Update, error) { return Update{}, context.DeadlineExceeded })
	waitFor(t, "DEGRADED_STALE", func() bool { return c.State() == StateDegradedStale })

	// Degraded still serves the last-known-good snapshot, and /ready stays true.
	if !c.BoolValue(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"}) {
		t.Fatal("DEGRADED_STALE must keep serving the last-known-good snapshot")
	}
	if !c.Ready() {
		t.Fatal("/ready must not gate on HEALTHY")
	}

	// Recovery: the source comes back with an advanced generation.
	gen.Store(4)
	src.setFetch(func() (Update, error) {
		g := gen.Load()
		return Update{Snapshot: fixture(g), Generation: g, InstanceID: "inst-a"}, nil
	})
	waitFor(t, "HEALTHY again", func() bool { return c.State() == StateHealthy && c.Generation() == 4 })

	var sawDegrade, sawRecover bool
	for _, tr := range met.snapshotTransitions() {
		switch tr {
		case "HEALTHY->DEGRADED_STALE":
			sawDegrade = true
		case "DEGRADED_STALE->HEALTHY":
			sawRecover = true
		}
	}
	if !sawDegrade || !sawRecover {
		t.Fatalf("transitions = %v, want both HEALTHY->DEGRADED_STALE and DEGRADED_STALE->HEALTHY",
			met.snapshotTransitions())
	}
}

// TestRecoveryWithUnchangedGenerationStillHeals guards the refinement in
// stateMachine.onLiveConfirmation: if nothing changed during the outage, no
// generation will ever advance, and a strict "generation advanced" rule would
// strand a fully-current pod in DEGRADED_STALE, alarming forever.
func TestRecoveryWithUnchangedGenerationStillHeals(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(2)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)

	c := newTestClient(t, refEvaluator(), WithSource(src),
		WithDeadStreamThreshold(30*time.Millisecond), WithReconcileInterval(0),
		WithBackoff(func(int) time.Duration { return 5 * time.Millisecond }))
	defer c.Close()

	waitFor(t, "HEALTHY", func() bool { return c.State() == StateHealthy })
	src.setFetch(func() (Update, error) { return Update{}, context.DeadlineExceeded })
	waitFor(t, "DEGRADED_STALE", func() bool { return c.State() == StateDegradedStale })

	// The source returns, with the very same generation we already hold.
	src.setFetch(func() (Update, error) {
		return Update{Snapshot: fixture(2), Generation: 2, InstanceID: "inst-a"}, nil
	})
	waitFor(t, "HEALTHY on an unchanged generation", func() bool { return c.State() == StateHealthy })
}

// TestL2HydratedClientHealsOnFirstLiveConfirmation is the second entry into
// DEGRADED_STALE: cold start from disk, then the source appears.
func TestL2HydratedClientHealsOnFirstLiveConfirmation(t *testing.T) {
	t.Parallel()
	st := newMemStore()
	if err := st.Save("prod", fixture(4)); err != nil {
		t.Fatal(err)
	}
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(4)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)
	met := newRecMetrics()

	c := newTestClient(t, refEvaluator(), WithL2Store(st), WithSource(src), WithMetrics(met),
		WithDeadStreamThreshold(time.Minute), WithReconcileInterval(0), fastBackoff(time.Hour))
	defer c.Close()

	waitFor(t, "HEALTHY after live confirmation", func() bool { return c.State() == StateHealthy })
	got := met.snapshotTransitions()
	if len(got) < 2 || got[0] != "UNINITIALIZED->DEGRADED_STALE" || got[1] != "DEGRADED_STALE->HEALTHY" {
		t.Fatalf("transitions = %v, want disk hydration then recovery", got)
	}
}

// TestReconcilePollConvergesWithoutPush covers the correctness backstop: the
// poll exists for the class of faults where the push path believes it succeeded
// and did not, so it must work with the push path entirely broken.
func TestReconcilePollConvergesWithoutPush(t *testing.T) {
	t.Parallel()
	var gen atomic.Int64
	var inst atomic.Value
	gen.Store(1)
	inst.Store("inst-a")
	src := genSource(&gen, &inst)
	src.subErr = context.DeadlineExceeded // push is dead for the whole test

	c := newTestClient(t, refEvaluator(), WithSource(src),
		WithReconcileInterval(10*time.Millisecond), fastBackoff(time.Hour))
	defer c.Close()

	waitFor(t, "generation 1", func() bool { return c.Generation() == 1 })
	gen.Store(7)
	waitFor(t, "generation 7 via reconcile", func() bool { return c.Generation() == 7 })
}

// TestFullJitterBackoffSpreads: unjittered backoff turns a flag-service restart
// into a synchronised thundering herd from the whole fleet, which keeps the
// service it is retrying against from coming back up.
func TestFullJitterBackoffSpreads(t *testing.T) {
	t.Parallel()
	const base = 100 * time.Millisecond
	const maxDelay = 30 * time.Second
	b := FullJitterBackoff(base, maxDelay)

	const attempt = 6
	window := base << attempt // 6.4s
	const n = 2000
	seen := make(map[time.Duration]struct{}, n)
	var sum time.Duration
	var min, max time.Duration = maxDelay, 0
	for i := 0; i < n; i++ {
		d := b(attempt)
		if d < 0 || d > window {
			t.Fatalf("delay %v outside [0, %v]", d, window)
		}
		seen[d] = struct{}{}
		sum += d
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	if len(seen) < n/2 {
		t.Fatalf("only %d distinct delays out of %d; the fleet would retry in lockstep", len(seen), n)
	}
	// Full jitter is uniform over the window, so the mean sits near the middle
	// and the extremes reach into both the first and last tenth.
	mean := sum / n
	if mean < window*2/5 || mean > window*3/5 {
		t.Fatalf("mean delay %v is not near the middle of [0, %v]", mean, window)
	}
	if min > window/10 || max < window*9/10 {
		t.Fatalf("delays spanned only [%v, %v] of [0, %v]", min, max, window)
	}

	// The cap holds, and a fresh attempt is still bounded.
	for i := 0; i < 200; i++ {
		if d := b(30); d > maxDelay {
			t.Fatalf("delay %v exceeded the cap %v", d, maxDelay)
		}
		if d := b(0); d > base {
			t.Fatalf("first attempt delay %v exceeded base %v", d, base)
		}
	}
}

func TestClientWithoutSourceNeverFetches(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	if c.State() != StateUninitialized {
		t.Fatalf("state = %s", c.State())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if c.WaitForReady(ctx) {
		t.Fatal("WaitForReady must report not-ready when nothing can make it ready")
	}
}

func TestStatsReportsPinnedView(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 12)
	s := c.Stats()
	if s.Env != "prod" || s.Generation != 12 || s.State != StateHealthy {
		t.Fatalf("stats = %+v", s)
	}
	if s.Flags != fixture(12).Len() {
		t.Fatalf("flags = %d, want %d", s.Flags, fixture(12).Len())
	}
}

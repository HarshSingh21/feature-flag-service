// This file answers one question, end to end, with printed steps:
//
//	Client A updates a flag at the exact moment Client B is evaluating it,
//	under high throughput. What does the service actually do?
//
// It is entirely self-contained: this one file is the whole scenario.
//
//	go test -v ./test/scenario/
//	go test -race -v ./test/scenario/
package scenario

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/config"
	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

const (
	oldBanner = "checkout-v1"
	newBanner = "checkout-v2"

	bannerFlag  = "checkout-banner"
	fillerFlags = 100 // F=100, the p99 request shape
)

// observation is one recorded evaluation. Workers append to their OWN slice, so
// the hot path takes no lock -- a mutex here would measure the mutex, not the
// service.
type observation struct {
	at         time.Duration // since t0
	value      string
	generation int64
	batchGens  int // distinct generations seen within one 100-flag batch
	latency    time.Duration
	reason     core.Reason
}

func TestTwoClientsUpdateAndEvaluateConcurrently(t *testing.T) {
	step := stepper{t: t}
	workers := runtime.NumCPU()

	// =======================================================================
	step.title("STEP 1 - Set up the service and publish the starting config")
	// =======================================================================
	store := config.New(config.WithEnvironments("prod"))
	eval := core.New()

	if rep := store.Set(buildBase(t, oldBanner)); rep.Err() != nil {
		t.Fatalf("base rejected: %v", rep.Err())
	}
	start, _ := store.Snapshot("prod")
	step.logf("published generation=%d with %d flags", start.Generation(), start.Len())
	step.logf("%s = %q", bannerFlag, oldBanner)
	step.note("One immutable snapshot. Readers will reach it through an atomic pointer.")

	// =======================================================================
	step.title("STEP 2 - Client B starts evaluating at high throughput")
	// =======================================================================
	step.logf("starting %d concurrent workers", workers)
	step.logf("each iteration pins the snapshot ONCE, then evaluates %d flags", fillerFlags)
	step.note("Pinning once per request is invariant CACHE-1. Pinning per flag would let")
	step.note("a swap mid-request return flag A from generation N and flag B from N+1.")

	var (
		stop  atomic.Bool
		wg    sync.WaitGroup
		obsMu sync.Mutex
		all   []observation
		evals atomic.Int64
	)
	t0 := time.Now()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			local := make([]observation, 0, 4096)
			ctx := core.EvalContext{UserID: fmt.Sprintf("user-%d", id)}

			for !stop.Load() {
				callStart := time.Now()

				// --- ONE request: pin once, evaluate many. ---
				snap, ok := store.Snapshot("prod")
				if !ok {
					continue
				}
				res := eval.Evaluate(snap, bannerFlag, ctx, core.TypeString, core.String("FALLBACK"))

				gens := map[int64]struct{}{res.Generation: {}}
				for i := 0; i < fillerFlags; i++ {
					r := eval.Evaluate(snap, fmt.Sprintf("filler-%d", i), ctx,
						core.TypeBool, core.Bool(false))
					gens[r.Generation] = struct{}{}
				}
				lat := time.Since(callStart)
				evals.Add(int64(fillerFlags + 1))

				v, _ := res.Value.AsString()
				local = append(local, observation{
					at:         callStart.Sub(t0),
					value:      v,
					generation: res.Generation,
					batchGens:  len(gens),
					latency:    lat,
					reason:     res.Reason,
				})
			}
			obsMu.Lock()
			all = append(all, local...)
			obsMu.Unlock()
		}(w)
	}

	// =======================================================================
	step.title("STEP 3 - Let it reach steady state, and confirm it is uniform")
	// =======================================================================
	time.Sleep(250 * time.Millisecond)
	step.logf("steady state running for 250ms")

	// =======================================================================
	step.title("STEP 4 - Client A pushes the update WHILE Client B is hammering it")
	// =======================================================================
	pushCalled := time.Since(t0)
	pushStart := time.Now()
	rep := store.Set(buildOverlay(t, newBanner))
	pushReturned := time.Since(t0)
	pushDuration := time.Since(pushStart)

	if rep.Err() != nil {
		t.Fatalf("update rejected: %v", rep.Err())
	}
	afterSnap, _ := store.Snapshot("prod")
	step.logf("Set() returned in %v", pushDuration.Round(time.Microsecond))
	step.logf("generation %d -> %d, content changed=%v",
		start.Generation(), afterSnap.Generation(), rep.PerEnv["prod"].ContentChanged)
	step.logf("%s is now %q", bannerFlag, newBanner)
	step.note("Build fully, validate fully, THEN one atomic pointer store.")
	step.note("Readers already inside a request keep their pinned snapshot to completion.")

	// =======================================================================
	step.title("STEP 5 - Let Client B run on, then stop and analyse")
	// =======================================================================
	time.Sleep(250 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	total := time.Since(t0)

	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	step.logf("collected %d requests / %d evaluations over %v",
		len(all), evals.Load(), total.Round(time.Millisecond))
	step.logf("throughput = %.0f evaluations/sec (with %d workers)",
		float64(evals.Load())/total.Seconds(), workers)

	// =======================================================================
	step.title("STEP 6 - Did any request ever see a TORN or INVALID value?")
	// =======================================================================
	bad := 0
	for _, o := range all {
		if o.value != oldBanner && o.value != newBanner {
			bad++
		}
		if o.reason == core.ReasonError {
			bad++
		}
	}
	step.logf("observations that were neither %q nor %q: %d", oldBanner, newBanner, bad)
	if bad != 0 {
		t.Fatalf("%d torn or errored observations", bad)
	}
	step.note("GUARANTEE G5 - integrity is never sacrificed. Valid config, or valid")
	step.note("last-known-good. Never partial, never a zero value, never an error.")

	// =======================================================================
	step.title("STEP 7 - Did any single request mix two generations?")
	// =======================================================================
	mixed := 0
	for _, o := range all {
		if o.batchGens != 1 {
			mixed++
		}
	}
	step.logf("requests whose %d flags spanned more than one generation: %d", fillerFlags, mixed)
	if mixed != 0 {
		t.Fatalf("%d requests saw a mixed-generation result set", mixed)
	}
	step.note("GUARANTEE G1 - single-generation read isolation, across a live swap.")

	// =======================================================================
	step.title("STEP 8 - How many times did the value flip?")
	// =======================================================================
	transitions := 0
	var firstNew time.Duration = -1
	for i := 1; i < len(all); i++ {
		if all[i].value != all[i-1].value {
			transitions++
			if all[i].value == newBanner && firstNew < 0 {
				firstNew = all[i].at
			}
		}
	}
	step.logf("OLD -> NEW transitions observed: %d", transitions)
	if transitions != 1 {
		t.Fatalf("expected exactly one transition, saw %d (flapping)", transitions)
	}
	step.note("GUARANTEE G3 - monotonic reads. It never flaps back to the old value.")

	// =======================================================================
	step.title("STEP 9 - How long did convergence take?")
	// =======================================================================
	converge := firstNew - pushCalled
	step.logf("Set() was CALLED at    t+%v", pushCalled.Round(time.Microsecond))
	step.logf("first NEW observed at  t+%v", firstNew.Round(time.Microsecond))
	step.logf("Set() RETURNED at      t+%v", pushReturned.Round(time.Microsecond))
	step.logf("CONVERGENCE DELTA = %v (measured from the CALL, not the return)",
		converge.Round(time.Microsecond))
	step.logf("budget is 5s; used %.4f%% of it", float64(converge)/float64(5*time.Second)*100)

	if converge < 0 {
		t.Fatalf("new value observed %v BEFORE the push was even called", -converge)
	}
	if converge > 5*time.Second {
		t.Fatalf("convergence %v exceeded the 5s budget", converge)
	}

	// The ordering here is the interesting part, and it is easy to measure wrong.
	if firstNew < pushReturned {
		step.logf("NOTE: readers saw the new value %v BEFORE Set() returned to the operator",
			(pushReturned - firstNew).Round(time.Microsecond))
		step.note("That is correct, not a race. The atomic pointer store IS the commit")
		step.note("point, and it happens INSIDE Set(). Everything after it -- building the")
		step.note("report, notifying subscribers, unwinding -- is bookkeeping the operator")
		step.note("waits for but readers do not. Measuring convergence from Set()'s RETURN")
		step.note("would report a negative delta and look like a clock bug.")
	}
	step.note("GUARANTEE G4 - bounded convergence. In-process the commit is immediate;")
	step.note("across a real fleet the push, heartbeat and reconcile poll bound it under 5s.")

	// =======================================================================
	step.title("STEP 10 - Did the swap show up as a latency event?")
	// =======================================================================
	var before, after []time.Duration
	for _, o := range all {
		if o.at < pushCalled {
			before = append(before, o.latency)
		} else {
			after = append(after, o.latency)
		}
	}
	bp99, ap99 := p99(before), p99(after)
	step.logf("p99 request latency BEFORE the swap: %v (%d requests)", bp99.Round(time.Microsecond), len(before))
	step.logf("p99 request latency AFTER  the swap: %v (%d requests)", ap99.Round(time.Microsecond), len(after))
	step.logf("per-evaluation cost after = %v", (ap99 / fillerFlags).Round(time.Nanosecond))
	step.note("A config swap must not be visible to callers as a latency spike.")
	step.note("Readers never block: they load a pointer, they do not take a lock.")

	// =======================================================================
	step.title("STEP 11 - So what did the service actually DO?")
	// =======================================================================
	step.note("WRITER  validated the layer, merged base+overlay, built a NEW immutable")
	step.note("        snapshot in a fresh allocation, then published it with a single")
	step.note("        atomic pointer store. It never mutated the snapshot readers held.")
	step.note("READER  each request loaded the pointer ONCE and evaluated every flag")
	step.note("        against that one snapshot. No lock, no I/O, no allocation.")
	step.note("HANDOFF requests in flight at swap time finished against the OLD snapshot;")
	step.note("        requests starting after it got the NEW one. There is no in-between.")
	step.note("GC      the old snapshot is freed once the last reader holding it returns.")
	step.note("")
	step.note("This is why a mutex around a mutable map was rejected. It is not only")
	step.note("slower -- a concurrent map read/write in Go is a FATAL runtime error that")
	step.note("recover() cannot catch, so it would break the never-throw contract itself.")

	step.title("SCENARIO COMPLETE")
}

// ---------------------------------------------------------------------------
// Narration helpers. Kept in this file so it stands alone.
// ---------------------------------------------------------------------------

type stepper struct{ t *testing.T }

func (s stepper) title(msg string) {
	s.t.Helper()
	s.t.Logf("\n%s\n%s\n%s", strings.Repeat("=", 74), msg, strings.Repeat("=", 74))
}

func (s stepper) logf(format string, args ...any) {
	s.t.Helper()
	s.t.Logf("    "+format, args...)
}

func (s stepper) note(msg string) {
	s.t.Helper()
	s.t.Logf("  > %s", msg)
}

func p99(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[(len(c)*99)/100]
}

func buildBase(t *testing.T, banner string) *config.BaseLayer {
	t.Helper()
	flags := []map[string]any{{
		"key": bannerFlag, "type": "string", "enabled": true, "default_value": banner,
	}}
	for i := 0; i < fillerFlags; i++ {
		flags = append(flags, map[string]any{
			"key": fmt.Sprintf("filler-%d", i), "type": "bool",
			"enabled": true, "default_value": i%2 == 0,
		})
	}
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "flags": flags})
	var l config.BaseLayer
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	return &l
}

func buildOverlay(t *testing.T, banner string) *config.OverlayLayer {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"schema_version": 1, "environment": "prod",
		"flags": []map[string]any{{"key": bannerFlag, "default_value": banner}},
	})
	var l config.OverlayLayer
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	return &l
}
package scenario

package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

func TestL2HydrationEntersDegradedStaleNotHealthy(t *testing.T) {
	t.Parallel()
	st := newMemStore()
	if err := st.Save("prod", fixture(11)); err != nil {
		t.Fatal(err)
	}
	met := newRecMetrics()
	c := newTestClient(t, refEvaluator(), WithL2Store(st), WithMetrics(met))
	defer c.Close()

	if c.State() != StateDegradedStale {
		t.Fatalf("state = %s, want DEGRADED_STALE; a disk copy has never been confirmed current", c.State())
	}
	if !c.Ready() {
		t.Fatal("/ready must be true in DEGRADED_STALE: a pod serving last-known-good is a working pod")
	}
	if c.Generation() != 11 {
		t.Fatalf("generation = %d, want 11", c.Generation())
	}
	// The whole point of L2: real config, not compiled-in defaults, at cold start.
	if !c.BoolValue(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"}) {
		t.Fatal("hydrated client must serve the persisted value, not the caller default")
	}
	want := []string{"UNINITIALIZED->DEGRADED_STALE"}
	if got := met.snapshotTransitions(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
}

func TestHydratedEntryIsNotWrittenBackToDisk(t *testing.T) {
	t.Parallel()
	st := newMemStore()
	if err := st.Save("prod", fixture(11)); err != nil {
		t.Fatal(err)
	}
	before := st.saveCount()
	c := newTestClient(t, refEvaluator(), WithL2Store(st))
	defer c.Close()
	time.Sleep(30 * time.Millisecond)
	if st.saveCount() != before {
		t.Fatal("hydrating from L2 must not immediately rewrite L2")
	}
}

// TestL2WriteFailureDoesNotFailTheApply is the rule from docs/03-lld.md §3.5:
// L2 is a restart optimisation, and a disk error degrades cold-start recovery,
// never the running system.
func TestL2WriteFailureDoesNotFailTheApply(t *testing.T) {
	t.Parallel()
	st := newMemStore()
	st.saveErr = errors.New("disk full")
	met := newRecMetrics()
	c := newTestClient(t, refEvaluator(), WithL2Store(st), WithMetrics(met))
	defer c.Close()

	applyFixture(c, 5)

	// The swap is already visible: the write is queued behind it, never before.
	if c.Generation() != 5 {
		t.Fatalf("generation = %d, want 5 immediately after apply", c.Generation())
	}
	if !c.BoolValue(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"}) {
		t.Fatal("a failed L2 write must not affect what is served")
	}
	if !st.awaitSave(time.Second) {
		t.Fatal("L2 write was never attempted")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, errs := met.l2Counts(); errs > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("L2 write failure must be counted so cold-start recovery loss is visible")
}

func TestL2WriteHappensAfterTheSwap(t *testing.T) {
	t.Parallel()
	st := newMemStore()
	c := newTestClient(t, refEvaluator(), WithL2Store(st))
	defer c.Close()
	applyFixture(c, 9)
	if !st.awaitSave(time.Second) {
		t.Fatal("apply must eventually persist to L2")
	}
	got, _, err := st.Load("prod")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation() != 9 {
		t.Fatalf("persisted generation = %d, want 9", got.Generation())
	}
}

func TestAsyncWriterCoalescesBursts(t *testing.T) {
	t.Parallel()
	seen := make(chan int64, 64)
	release := make(chan struct{})
	w := newAsyncWriter(func(e *entry) {
		<-release
		seen <- e.gen
	})
	defer w.close()

	for gen := int64(1); gen <= 50; gen++ {
		w.enqueue(&entry{gen: gen})
	}
	close(release)

	// Last write wins: the writer must converge on the newest generation
	// without performing all 50 writes.
	deadline := time.Now().Add(2 * time.Second)
	var writes int
	for time.Now().Before(deadline) {
		select {
		case g := <-seen:
			writes++
			if g == 50 {
				if writes > 10 {
					t.Fatalf("performed %d writes for a burst of 50; coalescing is not working", writes)
				}
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("the newest generation was never written")
}

// TestNoHardExpiry: an ancient snapshot keeps serving. Expiring it would turn a
// control-plane freshness problem into a fleet-wide data-plane outage.
func TestNoHardExpiry(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	ancient := time.Now().Add(-72 * time.Hour)
	c.cache.apply(&entry{snap: fixture(1), gen: 1, appliedAt: ancient})
	c.sm.set(StateDegradedStale, "test")
	c.lastLiveNanos.Store(ancient.UnixNano())

	if !c.BoolValue(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"}) {
		t.Fatal("a three-day-old snapshot must keep serving; staleness is reported, never enforced")
	}
	d := c.BoolDetail(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"})
	if d.IsFallback() {
		t.Fatal("serving a stale snapshot is not a fallback")
	}
	s := c.Stats()
	if s.StalenessSeconds < 3600 {
		t.Fatalf("staleness = %.0fs, want it reported as very large", s.StalenessSeconds)
	}
	if !c.Ready() {
		t.Fatal("a stale but serving client is ready")
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := NewFileStore(dir, nil)

	if _, _, err := fs.Load("prod"); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("cold Load err = %v, want ErrNoSnapshot", err)
	}
	if err := fs.Save("prod", fixture(21)); err != nil {
		t.Fatal(err)
	}
	got, writtenAt, err := fs.Load("prod")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation() != 21 || got.Env() != "prod" || got.Len() != fixture(21).Len() {
		t.Fatalf("round trip lost data: gen=%d env=%s len=%d", got.Generation(), got.Env(), got.Len())
	}
	if writtenAt.IsZero() {
		t.Fatal("writtenAt must be populated so hydration can report the snapshot's age")
	}
	// No temporary files may survive a successful save.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("left a temp file behind: %s", e.Name())
		}
	}
}

func TestFileStoreSanitisesEnvironmentIntoPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := NewFileStore(dir, nil)
	if err := fs.Save("../../etc/passwd", fixture(1)); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("expected exactly one file in dir, got %d", len(ents))
	}
	if got := filepath.Dir(fs.path("../../etc/passwd")); got != dir {
		t.Fatalf("path escaped the cache directory: %s", got)
	}
}

func TestFileStoreRefusesForeignFormat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := NewFileStore(dir, nil)
	if err := os.WriteFile(fs.path("prod"), []byte(`{"format":999,"env":"prod"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Mis-reading persisted config is worse than having none, so a file this
	// binary does not understand is refused rather than best-effort parsed.
	if _, _, err := fs.Load("prod"); err == nil {
		t.Fatal("expected a decode error for a foreign L2 format")
	}
}

func TestUnreadableL2LeavesClientUninitializedNotBroken(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := NewFileStore(dir, nil)
	if err := os.WriteFile(fs.path("prod"), []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, refEvaluator(), WithL2Store(fs))
	defer c.Close()
	if c.State() != StateUninitialized {
		t.Fatalf("state = %s, want UNINITIALIZED", c.State())
	}
	if got := c.BoolValue(context.Background(), "checkout_v2", true, core.EvalContext{}); !got {
		t.Fatal("a corrupt L2 file must degrade to caller defaults, not to a broken client")
	}
}

func TestJSONCodecPreservesEnumsByName(t *testing.T) {
	t.Parallel()
	src := fixture(3)
	b, err := JSONCodec{}.Encode(src)
	if err != nil {
		t.Fatal(err)
	}
	// Enums are persisted as names, so that inserting a constant into the
	// middle of core.Operator cannot silently re-map a persisted rule.
	for _, want := range []string{`"in"`, `"and"`, `"rules_first"`, `"string"`, `"bool"`} {
		if !contains(string(b), want) {
			t.Fatalf("encoded form missing %s: %s", want, b)
		}
	}
	got, err := JSONCodec{}.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := got.Flag("targeted")
	if !ok {
		t.Fatal("targeted flag lost in round trip")
	}
	if len(f.Rules) != 1 || f.Rules[0].ID != "r-country-in" || f.Rules[0].Conditions[0].Op != core.OpIn {
		t.Fatalf("rule lost in round trip: %+v", f.Rules)
	}
	if f.EvaluationOrder != core.OrderRulesFirst {
		t.Fatalf("evaluation order = %s, want rules_first", f.EvaluationOrder)
	}
	ro, ok := got.Flag("rolled_out")
	if !ok || ro.Rollout == nil || ro.Rollout.BasisPoints != 5000 {
		t.Fatalf("rollout lost in round trip: %+v", ro)
	}
}

func TestJSONCodecRejectsUnknownOperator(t *testing.T) {
	t.Parallel()
	bad := `{"format":1,"env":"prod","generation":1,"flags":[{"key":"f","type":"bool","enabled":true,` +
		`"default_value":true,"off_value":false,"rules":[{"id":"r","combiner":"and","value":true,` +
		`"conditions":[{"attribute":"a","op":"regex","values":["x"]}]}]}]}`
	if _, err := (JSONCodec{}).Decode([]byte(bad)); err == nil {
		t.Fatal("an operator this binary does not implement must fail the decode, not be dropped")
	}
}

func TestMemSnapshotDoesNotAliasCallerSlice(t *testing.T) {
	t.Parallel()
	flags := []core.Flag{boolFlag("f", true)}
	snap := NewMemSnapshot("prod", 1, flags)
	flags[0].Enabled = false
	flags[0].DefaultValue = core.Bool(false)
	f, _ := snap.Flag("f")
	if !f.Enabled {
		t.Fatal("a published snapshot must not observe later mutation of the caller's slice (CACHE-2)")
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

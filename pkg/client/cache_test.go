package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// mustPath fails the test rather than propagating the containment error, which
// path only returns for an input encodeEnvName cannot produce.
func mustPath(t *testing.T, fs *FileStore, env string) string {
	t.Helper()
	p, err := fs.path(env)
	if err != nil {
		t.Fatalf("path(%q): %v", env, err)
	}
	return p
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
	if got := filepath.Dir(mustPath(t, fs, "../../etc/passwd")); got != dir {
		t.Fatalf("path escaped the cache directory: %s", got)
	}
}

// TestFileStoreEnvironmentNamesDoNotCollide is the regression test for
// cross-environment config bleed.
//
// The encoding used to fold every unusual byte to '_', so "prod!" and "prod?"
// resolved to one file. The consequence was not a cosmetic filename clash: two
// clients would overwrite each other's last-known-good, and the next cold start
// during an outage would hydrate a pod with a *different* environment's flag
// config, carrying a plausible generation and looking healthy while doing it.
func TestFileStoreEnvironmentNamesDoNotCollide(t *testing.T) {
	t.Parallel()
	fs := NewFileStore(t.TempDir(), nil)
	// Every one of these folded onto "prod_" or "prod" under the old encoding,
	// and the empty name folded onto "default".
	names := []string{"prod", "prod!", "prod?", "prod/", "prod_", "prod.", "Prod", "PROD", "", "default", "~", "prod~21"}
	seen := make(map[string]string, len(names))
	for _, env := range names {
		p := mustPath(t, fs, env)
		if prev, dup := seen[p]; dup {
			t.Fatalf("environments %q and %q share a last-known-good file %s", prev, env, filepath.Base(p))
		}
		seen[p] = env
	}
	// Case folding is the same bug one layer down: on macOS and Windows a
	// filename that differs only by case is the same file, so distinct paths are
	// necessary but not sufficient. Uppercase is escaped, so no two of these
	// differ only by case.
	lowered := make(map[string]string, len(names))
	for p, env := range seen {
		low := strings.ToLower(p)
		if prev, dup := lowered[low]; dup {
			t.Fatalf("environments %q and %q collide on a case-insensitive filesystem: %s", prev, env, filepath.Base(p))
		}
		lowered[low] = env
	}
}

// TestFileStoreRoundTripsAnAwkwardEnvironmentName proves the escaped name is a
// usable filename on this platform, not merely a distinct string.
func TestFileStoreRoundTripsAnAwkwardEnvironmentName(t *testing.T) {
	t.Parallel()
	fs := NewFileStore(t.TempDir(), nil)
	for _, env := range []string{"prod!", "eu west/1", "", "Prod"} {
		if err := fs.Save(env, fixture(7)); err != nil {
			t.Fatalf("Save(%q): %v", env, err)
		}
		got, _, err := fs.Load(env)
		if err != nil {
			t.Fatalf("Load(%q): %v", env, err)
		}
		if got.Generation() != 7 {
			t.Fatalf("Load(%q) generation = %d, want 7", env, got.Generation())
		}
	}
}

// TestFileStoreSaveClosesTheTempFileExactlyOnce pins the double-close fix. The
// old Save closed in the defer as well as on the success path, so the deferred
// Close always failed with os.ErrClosed and threw the result away, hiding any
// genuine close error behind a self-inflicted one.
func TestFileStoreSaveClosesTheTempFileExactlyOnce(t *testing.T) {
	t.Parallel()
	// A directory Save has to create itself, so the 0750 mode is actually
	// exercised: MkdirAll leaves an existing directory's mode alone.
	dir := filepath.Join(t.TempDir(), "l2")
	fs := NewFileStore(dir, nil)
	if err := fs.Save("prod", fixture(3)); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the renamed file: the temp is gone, and the rename was not
	// followed by a remove of the final name.
	if len(ents) != 1 || ents[0].Name() != filepath.Base(mustPath(t, fs, "prod")) {
		t.Fatalf("unexpected cache directory contents: %v", ents)
	}
	// A second Save over the top must also leave exactly one file, which is the
	// path where a spurious deferred Close/Remove would show up.
	if err := fs.Save("prod", fixture(4)); err != nil {
		t.Fatal(err)
	}
	if ents, err = os.ReadDir(dir); err != nil || len(ents) != 1 {
		t.Fatalf("after resave: %v %v", ents, err)
	}
	got, _, err := fs.Load("prod")
	if err != nil || got.Generation() != 4 {
		t.Fatalf("Load after resave = %v, %v; want generation 4", got, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o027 != 0 {
		t.Fatalf("cache directory mode = %04o, want no group-write or world access", perm)
	}
}

func TestFileStoreRefusesForeignFormat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fs := NewFileStore(dir, nil)
	if err := os.WriteFile(mustPath(t, fs, "prod"), []byte(`{"format":999,"env":"prod"}`), 0o644); err != nil {
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
	if err := os.WriteFile(mustPath(t, fs, "prod"), []byte("{ not json"), 0o644); err != nil {
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

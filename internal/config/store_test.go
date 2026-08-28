package config

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(WithClock(fixedClock()))
}

// mustPublish sets a layer and fails if the named environments did not publish.
func mustPublish(t *testing.T, s *Store, l Layer, envs ...string) *BuildReport {
	t.Helper()
	r := s.Set(l)
	for _, e := range envs {
		if !r.PublishedIn(e) {
			t.Fatalf("env %s did not publish:\n%s\n%s", e, r.String(), r.Findings().Error())
		}
	}
	return r
}

// intBase builds a base document with n int flags all carrying the same value,
// which lets a concurrent reader check a snapshot for internal consistency.
func intBase(n int, value int) string {
	var b strings.Builder
	b.WriteString(`{"schema_version":1,"flags":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"key":"f.%d","type":"int","owner":"o","enabled":true,"default_value":%d,"off_value":%d}`, i, value, value)
	}
	b.WriteString(`]}`)
	return b.String()
}

// -----------------------------------------------------------------------------
// Publication basics
// -----------------------------------------------------------------------------

func TestStoreColdStartMissesEverything(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if _, ok := s.Get(flagKey, "prod"); ok {
		t.Fatal("a store with no accepted base must miss")
	}
	if _, ok := s.Snapshot("prod"); ok {
		t.Fatal("no snapshot exists before the first successful build")
	}
	if _, ok := s.Get(flagKey, "no-such-env"); ok {
		t.Fatal("an unknown environment must miss, not panic")
	}
}

func TestStorePublishesAllEnvironmentsFromBase(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	r := mustPublish(t, s, mustBase(t, canonicalBase), "dev", "staging", "prod")
	if r.Err() != nil {
		t.Fatalf("clean base reported an error: %v", r.Err())
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		snap, ok := s.Snapshot(env)
		if !ok || snap.Generation() != 1 || snap.Env() != env {
			t.Fatalf("%s: ok=%v snapshot=%+v", env, ok, snap)
		}
		f, ok := s.Get(flagKey, env)
		if !ok || f.Rollout.BasisPoints != 500 {
			t.Fatalf("%s: flag=%+v ok=%v", env, f, ok)
		}
	}
}

func TestStoreGenerationIsMonotonicPerEnvironment(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	for want := int64(2); want <= 4; want++ {
		r := mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[{"key":"`+flagKey+`","enabled":false}]}`), "prod")
		if got := r.PerEnv["prod"].Generation; got != want {
			t.Fatalf("generation: got %d want %d", got, want)
		}
	}
	// dev never received a write of its own, so it stays at generation 1.
	dev, _ := s.Snapshot("dev")
	if dev.Generation() != 1 {
		t.Fatalf("dev generation moved to %d without a dev write", dev.Generation())
	}
}

func TestStoreLayeredResolutionEndToEnd(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[{"key":"`+flagKey+`",
	  "enabled":true,"rollout":{"basis_points":2500},"rules_mode":"append",
	  "rules":[{"id":"prod-enterprise","conditions":[{"attribute":"tenant_tier","op":"eq","values":["enterprise"]}],"value":true}]}]}`), "prod")
	mustPublish(t, s, mustOps(t, `{"environment":"prod","overrides":[{"key":"`+flagKey+`","enabled":false,
	  "expires_at":"2026-01-01T01:00:00Z","reason":"INC-1","owner":"oncall"}]}`), "prod")

	f, ok := s.Get(flagKey, "prod")
	if !ok {
		t.Fatal("flag missing from prod")
	}
	if f.Enabled {
		t.Fatal("L3 kill switch must outrank the L2 overlay")
	}
	if f.Rollout.BasisPoints != 2500 || f.Rollout.BucketNamespace != "checkout-cohort-a" {
		t.Fatalf("rollout: %+v", f.Rollout)
	}
	if len(f.Rules) != 3 || f.Rules[2].ID != "prod-enterprise" {
		t.Fatalf("rules: %+v", f.Rules)
	}

	// dev saw neither the overlay nor the override.
	d, _ := s.Get(flagKey, "dev")
	if !d.Enabled || d.Rollout.BasisPoints != 500 || len(d.Rules) != 2 {
		t.Fatalf("dev leaked prod config: %+v", d)
	}

	snap, _ := s.Snapshot("prod")
	prov, ok := snap.Provenance(flagKey)
	if !ok {
		t.Fatal("provenance missing from the published snapshot")
	}
	for field, want := range map[string]LayerID{
		FieldEnabled:                LayerOps,
		FieldRolloutBasisPoints:     LayerOverlay,
		FieldRolloutBucketNamespace: LayerBase,
		FieldRules:                  LayerOverlay,
		FieldType:                   LayerBase,
	} {
		if got := prov.Layer(field); got != want {
			t.Fatalf("provenance[%s] = %s, want %s", field, got, want)
		}
	}
}

func TestStoreOpsOverrideSelfHealsOnExpiry(t *testing.T) {
	t.Parallel()
	now := testNow
	s := New(WithClock(func() time.Time { return now }))
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	mustPublish(t, s, mustOps(t, `{"environment":"prod","overrides":[{"key":"`+flagKey+`","enabled":false,
	  "expires_at":"2026-01-01T01:00:00Z","reason":"INC-1","owner":"oncall"}]}`), "prod")
	if f, _ := s.Get(flagKey, "prod"); f.Enabled {
		t.Fatal("kill switch did not take effect")
	}

	now = testNow.Add(2 * time.Hour) // the TTL has passed
	r := s.Rebuild()
	if !r.PublishedIn("prod") {
		t.Fatalf("rebuild did not publish:\n%s", r.Findings().Error())
	}
	if f, _ := s.Get(flagKey, "prod"); !f.Enabled {
		t.Fatal("an expired ops override must stop applying")
	}
	if !r.PerEnv["prod"].Warnings.Has("M11") {
		t.Fatal("the drop must be signalled, not silent")
	}
}

// -----------------------------------------------------------------------------
// Failure posture
// -----------------------------------------------------------------------------

func TestStoreRejectedBaseLeavesSnapshotByteIdentical(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "dev", "staging", "prod")

	before := map[string]struct {
		gen int64
		fp  string
	}{}
	for _, env := range s.Environments() {
		snap, _ := s.Snapshot(env)
		before[env] = struct {
			gen int64
			fp  string
		}{snap.Generation(), snap.Fingerprint()}
	}

	// A malformed base is the one global blast radius: nothing publishes anywhere.
	r := s.Set(mustBase(t, `{"flags":[{"key":"`+flagKey+`","type":"not-a-type","enabled":true,"default_value":false}]}`))
	if r.Err() == nil {
		t.Fatal("a malformed base must be rejected")
	}
	if len(r.Global) == 0 {
		t.Fatal("base rejections must be reported as global")
	}
	for _, env := range s.Environments() {
		if r.PublishedIn(env) {
			t.Fatalf("%s published despite a malformed base", env)
		}
		snap, _ := s.Snapshot(env)
		if snap.Generation() != before[env].gen {
			t.Fatalf("%s generation moved on a rejected build: %d -> %d", env, before[env].gen, snap.Generation())
		}
		if snap.Fingerprint() != before[env].fp {
			t.Fatalf("%s content changed on a rejected build", env)
		}
		// A rejection is a no-op on the cache, not a flush.
		if _, ok := s.Get(flagKey, env); !ok {
			t.Fatalf("%s: last-known-good stopped serving after a rejection", env)
		}
	}
	if _, ok := s.LastRejected("base"); !ok {
		t.Fatal("the rejected layer must be retained for forensics")
	}
	// The raw layers still hold the ACCEPTED base, not the rejected one.
	base, _, _ := s.RawLayers()
	if base.Flags[0].Type != "bool" {
		t.Fatalf("rejected config reached the store: %+v", base.Flags[0])
	}
}

// A prod typo must not block an urgent dev fix.
func TestStoreEnvironmentIsolation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, intBase(30, 1)), "dev", "staging", "prod")

	stagingBefore, _ := s.Snapshot("staging")
	devBefore, _ := s.Snapshot("dev")

	// 21 bad prod flags: past the budget of max(20, 5% of 30) = 20, so prod
	// escalates to a whole-environment rejection.
	var sb strings.Builder
	sb.WriteString(`{"environment":"prod","flags":[`)
	for i := 0; i < 21; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"key":"f.%d","default_value":"a-string"}`, i)
	}
	sb.WriteString(`]}`)

	r := s.Set(mustOverlay(t, sb.String()))
	if r.PublishedIn("prod") {
		t.Fatal("prod published despite exceeding the quarantine budget")
	}
	if !r.PerEnv["prod"].Rejected.Has("M15") {
		t.Fatalf("want the M15 safety valve, got %v", r.PerEnv["prod"].Rejected.RuleIDs())
	}
	if _, ok := r.PerEnv["dev"]; ok {
		t.Fatal("a prod overlay write must not even touch dev")
	}

	// dev and staging are untouched: same generation, same content.
	for env, before := range map[string]*ResolvedSnapshot{"dev": devBefore, "staging": stagingBefore} {
		now, _ := s.Snapshot(env)
		if now.Generation() != before.Generation() {
			t.Fatalf("%s generation moved: %d -> %d", env, before.Generation(), now.Generation())
		}
		if now.Fingerprint() != before.Fingerprint() {
			t.Fatalf("%s content changed because of a prod failure", env)
		}
	}
	// prod keeps its own last-known-good.
	prod, _ := s.Snapshot("prod")
	if prod.Generation() != 1 || prod.Len() != 30 {
		t.Fatalf("prod lost its last-known-good: gen=%d len=%d", prod.Generation(), prod.Len())
	}

	// ...and an urgent dev fix still ships.
	r = mustPublish(t, s, mustOverlay(t, `{"environment":"dev","flags":[{"key":"f.0","enabled":false}]}`), "dev")
	if r.PerEnv["dev"].Generation != 2 {
		t.Fatalf("dev generation: %d", r.PerEnv["dev"].Generation)
	}
	if f, _ := s.Get("f.0", "prod"); !f.Enabled {
		t.Fatal("the dev fix leaked into prod")
	}
}

func TestStoreQuarantinesOneFlagAndKeepsServingTheRest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, intBase(30, 1)), "prod")

	// A good overlay first, so f.7 has a last-known-good to carry.
	mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[{"key":"f.7","default_value":42}]}`), "prod")
	if f, _ := s.Get("f.7", "prod"); f.DefaultValue.String() != "42" {
		t.Fatalf("setup: f.7 = %v", f.DefaultValue)
	}

	// Now break exactly one flag. M01: the overlay carries no type, so only the
	// merge can see that "a-string" is not an int.
	r := mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[
	  {"key":"f.7","default_value":"a-string"},
	  {"key":"f.8","default_value":99}]}`), "prod")

	if !r.PerEnv["prod"].Quarantined.Has("M01") {
		t.Fatalf("want M01 quarantine, got %v", r.PerEnv["prod"].Quarantined.RuleIDs())
	}
	snap, _ := s.Snapshot("prod")
	if snap.Generation() != 3 {
		t.Fatalf("the environment must still publish: gen=%d", snap.Generation())
	}
	if snap.Len() != 30 {
		t.Fatalf("len=%d, want all 30 flags still served", snap.Len())
	}
	if !snap.IsQuarantined("f.7") {
		t.Fatal("f.7 must be marked quarantined")
	}
	// The quarantined flag keeps its LAST-KNOWN-GOOD value, which is itself a
	// valid servable state.
	f7, _ := s.Get("f.7", "prod")
	if f7.DefaultValue.String() != "42" {
		t.Fatalf("f.7 must carry its last-known-good, got %v", f7.DefaultValue)
	}
	// Everything else applied normally.
	f8, _ := s.Get("f.8", "prod")
	if f8.DefaultValue.String() != "99" {
		t.Fatalf("an unrelated flag was blocked by the quarantine: %v", f8.DefaultValue)
	}
	if snap.IsQuarantined("f.8") {
		t.Fatal("f.8 must not be quarantined")
	}
}

func TestStoreQuarantineWithNoLastKnownGoodOmitsTheFlag(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// M02: an overlay naming a flag with no base entry is unservable, not merely
	// wrong -- there is no type and no default, so no core.Flag can be built.
	mustPublish(t, s, mustBase(t, intBase(3, 1)), "prod")
	r := mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[{"key":"ghost.flag","enabled":true}]}`), "prod")
	if !r.PerEnv["prod"].Quarantined.Has("M02") {
		t.Fatalf("want M02, got %v", r.PerEnv["prod"].Quarantined.RuleIDs())
	}
	snap, _ := s.Snapshot("prod")
	if _, ok := s.Get("ghost.flag", "prod"); ok {
		t.Fatal("an orphan must be absent so the caller applies its L0 default")
	}
	if !snap.IsQuarantined("ghost.flag") {
		t.Fatal("the orphan must still count against the quarantine budget")
	}
	if snap.Len() != 3 {
		t.Fatalf("the other flags must keep serving: len=%d", snap.Len())
	}
}

func TestStoreQuarantineBudgetEscalatesToEnvReject(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, intBase(1000, 1)), "prod")
	budget := QuarantineBudget(1000) // 50

	bad := func(n int) Layer {
		var sb strings.Builder
		sb.WriteString(`{"environment":"prod","flags":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"key":"f.%d","default_value":"a-string"}`, i)
		}
		sb.WriteString(`]}`)
		return mustOverlay(t, sb.String())
	}

	// Exactly at the budget: quarantine, and the environment still publishes.
	r := s.Set(bad(budget))
	if !r.PublishedIn("prod") {
		t.Fatalf("at the budget the environment must still publish:\n%s", r.Findings().Error())
	}
	snap, _ := s.Snapshot("prod")
	if snap.QuarantinedCount() != budget {
		t.Fatalf("quarantined=%d want %d", snap.QuarantinedCount(), budget)
	}
	if snap.Len() != 1000 {
		t.Fatalf("len=%d: quarantined flags must carry their last-known-good", snap.Len())
	}
	if f, ok := snap.Flag("f.0"); !ok || f.DefaultValue.String() != "1" {
		t.Fatalf("f.0 must still serve its last-known-good, got %v (ok=%v)", f, ok)
	}

	// One past it: systematically broken input, so the whole environment is
	// rejected rather than half-applied.
	genBefore := snap.Generation()
	fpBefore := snap.Fingerprint()
	r = s.Set(bad(budget + 1))
	if r.PublishedIn("prod") {
		t.Fatal("past the budget the environment must be rejected")
	}
	if !r.PerEnv["prod"].Rejected.Has("M15") {
		t.Fatalf("want M15, got %v", r.PerEnv["prod"].Rejected.RuleIDs())
	}
	after, _ := s.Snapshot("prod")
	if after.Generation() != genBefore || after.Fingerprint() != fpBefore {
		t.Fatal("a rejected environment build must leave the snapshot byte-identical")
	}
}

func TestStoreRejectedOverlayNeverReachesTheCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	before, _ := s.Snapshot("prod")

	// A pre-merge overlay rejection (type restatement) quarantines the flag; with
	// only one flag in the base that is within budget, so the env still publishes
	// but the flag keeps its previous content.
	r := s.Set(mustOverlay(t, `{"environment":"prod","flags":[{"key":"`+flagKey+`","type":"bool","enabled":false}]}`))
	if !r.PerEnv["prod"].Quarantined.Has("O02") {
		t.Fatalf("want O02, got %v", r.PerEnv["prod"].Quarantined.RuleIDs())
	}
	f, _ := s.Get(flagKey, "prod")
	if !f.Enabled {
		t.Fatal("a rejected overlay must not apply its other fields either")
	}
	after, _ := s.Snapshot("prod")
	if after.Fingerprint() != before.Fingerprint() {
		t.Fatal("rejected config changed the served content")
	}
}

func TestStoreRawLayersAreDeepCopies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[{"key":"`+flagKey+`","enabled":false}]}`), "prod")

	base, overlays, _ := s.RawLayers()
	if base == nil || overlays["prod"] == nil {
		t.Fatal("raw layers must be retained for forensics")
	}
	base.Flags[0].Type = "mutated"
	overlays["prod"].Flags[0].Key = "mutated"
	again, againOv, _ := s.RawLayers()
	if again.Flags[0].Type != "bool" || againOv["prod"].Flags[0].Key != flagKey {
		t.Fatal("RawLayers handed out live state")
	}
}

// -----------------------------------------------------------------------------
// Concurrency
// -----------------------------------------------------------------------------

func TestStoreConcurrentReadsDuringWrites(t *testing.T) {
	const (
		flags   = 40
		rounds  = 60
		readers = 8
	)
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, intBase(flags, 0)), "prod")

	var stop atomic.Bool
	var wg sync.WaitGroup

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lastGen int64
			for !stop.Load() {
				snap, ok := s.Snapshot("prod")
				if !ok {
					t.Error("prod snapshot disappeared mid-flight")
					return
				}
				// A snapshot must never be observed half-built.
				if snap.Len() != flags {
					t.Errorf("partially built snapshot: gen=%d len=%d want %d", snap.Generation(), snap.Len(), flags)
					return
				}
				if snap.Generation() < lastGen {
					t.Errorf("generation went backwards: %d then %d", lastGen, snap.Generation())
					return
				}
				lastGen = snap.Generation()

				// Every flag in one snapshot must come from ONE generation: they
				// all carry the same marker value.
				var marker core.Value
				for i := 0; i < flags; i++ {
					f, ok := snap.Flag(fmt.Sprintf("f.%d", i))
					if !ok {
						t.Errorf("gen=%d missing f.%d", snap.Generation(), i)
						return
					}
					if i == 0 {
						marker = f.DefaultValue
						continue
					}
					if !f.DefaultValue.Equal(marker) {
						t.Errorf("torn snapshot gen=%d: f.0=%v but f.%d=%v", snap.Generation(), marker, i, f.DefaultValue)
						return
					}
				}
				// The lock-free single-flag path must agree with the pinned one.
				if _, ok := s.Get("f.0", "prod"); !ok {
					t.Error("Get missed a flag that is in the snapshot")
					return
				}
			}
		}()
	}

	for round := 1; round <= rounds; round++ {
		if r := s.Set(mustBase(t, intBase(flags, round))); r.Err() != nil {
			t.Fatalf("round %d: %v", round, r.Err())
		}
	}
	stop.Store(true)
	wg.Wait()

	snap, _ := s.Snapshot("prod")
	if snap.Generation() != int64(rounds+1) {
		t.Fatalf("final generation %d, want %d", snap.Generation(), rounds+1)
	}
}

func TestStoreConcurrentWritersAcrossEnvironments(t *testing.T) {
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, intBase(10, 0)), "dev", "staging", "prod")

	var wg sync.WaitGroup
	for _, env := range []string{"dev", "staging", "prod"} {
		wg.Add(1)
		go func(env string) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				doc := fmt.Sprintf(`{"environment":%q,"flags":[{"key":"f.0","default_value":%d}]}`, env, i)
				if r := s.Set(mustOverlay(t, doc)); !r.PublishedIn(env) {
					t.Errorf("%s round %d did not publish", env, i)
					return
				}
			}
		}(env)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			for _, env := range []string{"dev", "staging", "prod"} {
				if _, ok := s.Get("f.0", env); !ok {
					t.Errorf("%s: f.0 missing", env)
					return
				}
			}
		}
	}()
	wg.Wait()

	for _, env := range []string{"dev", "staging", "prod"} {
		snap, _ := s.Snapshot(env)
		if snap.Generation() != 31 {
			t.Fatalf("%s generation %d, want 31", env, snap.Generation())
		}
	}
}

// -----------------------------------------------------------------------------
// Subscriptions
// -----------------------------------------------------------------------------

func TestSubscribeDeliversPublishedSnapshots(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ch, cancel := s.Subscribe("prod")
	defer cancel()

	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	select {
	case snap := <-ch:
		if snap == nil || snap.Generation() != 1 || snap.Env() != "prod" {
			t.Fatalf("bad delivery: %+v", snap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no snapshot delivered within the bound")
	}

	mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[{"key":"`+flagKey+`","enabled":false}]}`), "prod")
	select {
	case snap := <-ch:
		if snap.Generation() != 2 {
			t.Fatalf("want generation 2, got %d", snap.Generation())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no second snapshot delivered")
	}
}

func TestSubscribeReceivesCurrentSnapshotImmediately(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	ch, cancel := s.Subscribe("prod")
	defer cancel()
	select {
	case snap := <-ch:
		if snap.Generation() != 1 {
			t.Fatalf("generation %d", snap.Generation())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a late subscriber must get the current snapshot")
	}
}

func TestSubscribeSlowSubscriberDoesNotStallThePublisher(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, intBase(5, 0)), "prod")

	slow, cancelSlow := s.Subscribe("prod") // never drained until the end
	defer cancelSlow()
	fast, cancelFast := s.Subscribe("prod")
	defer cancelFast()

	fastSeen := make(chan int64, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for snap := range fast {
			fastSeen <- snap.Generation()
			if snap.Generation() == 21 {
				return
			}
		}
	}()

	start := time.Now()
	for i := 1; i <= 20; i++ {
		doc := fmt.Sprintf(`{"environment":"prod","flags":[{"key":"f.0","default_value":%d}]}`, i)
		if r := s.Set(mustOverlay(t, doc)); !r.PublishedIn("prod") {
			t.Fatalf("publish %d failed", i)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("publisher was stalled by a slow subscriber: %s", elapsed)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fast subscriber never reached the final generation")
	}

	// The slow subscriber missed intermediate generations -- that is the drop
	// policy -- but converges on the latest, because a snapshot is absolute state
	// and not a delta.
	select {
	case snap := <-slow:
		if snap.Generation() != 21 {
			t.Fatalf("slow subscriber holds generation %d, want the latest 21", snap.Generation())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow subscriber received nothing at all")
	}
}

func TestSubscribeCancelIsIdempotentAndClosesTheChannel(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ch, cancel := s.Subscribe("prod")
	cancel()
	cancel() // must not panic on a double cancel

	if _, ok := <-ch; ok {
		t.Fatal("cancel must close the channel")
	}
	// Publishing to an environment whose only subscriber cancelled must not panic
	// on a send to a closed channel.
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
}

func TestSubscribeRejectedBuildPublishesNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	ch, cancel := s.Subscribe("prod")
	defer cancel()
	<-ch // drain the initial delivery

	if r := s.Set(mustBase(t, `{"flags":[{"key":"a.b","type":"nope","enabled":true,"default_value":1}]}`)); r.Err() == nil {
		t.Fatal("expected a rejection")
	}
	select {
	case snap := <-ch:
		t.Fatalf("a rejected build notified subscribers with generation %d", snap.Generation())
	case <-time.After(200 * time.Millisecond):
	}
}

func TestStoreProvenanceIsExposedForDebugging(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustPublish(t, s, mustBase(t, canonicalBase), "prod")
	mustPublish(t, s, mustOverlay(t, `{"environment":"prod","flags":[{"key":"`+flagKey+`","rollout":{"basis_points":2500}}]}`), "prod")

	p, ok := s.Provenance(flagKey, "prod")
	if !ok {
		t.Fatal("provenance must be reachable from the store")
	}
	if p.Key != flagKey {
		t.Fatalf("provenance key %q", p.Key)
	}
	if p.Layer(FieldRolloutBasisPoints) != LayerOverlay {
		t.Fatalf("basis_points came from %s", p.Layer(FieldRolloutBasisPoints))
	}
	if p.Layer(FieldRolloutBucketNamespace) != LayerBase {
		t.Fatalf("bucket_namespace came from %s", p.Layer(FieldRolloutBucketNamespace))
	}
	if _, ok := s.Provenance(flagKey, "no-such-env"); ok {
		t.Fatal("provenance for an unknown environment must miss")
	}
}

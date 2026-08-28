package config

import (
	"testing"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

func buildTestSnapshot(t *testing.T, env string, gen int64, docs ...string) *ResolvedSnapshot {
	t.Helper()
	doc := canonicalBase
	if len(docs) > 0 {
		doc = docs[0]
	}
	base := mustBase(t, doc)
	b := newSnapshotBuilder(env, len(base.Flags))
	for i := range base.Flags {
		f, prov := mergeFlag(&base.Flags[i], nil, nil)
		b.add(f, prov)
	}
	return b.build(gen, testNow)
}

func TestSnapshotSatisfiesCoreContract(t *testing.T) {
	t.Parallel()
	var s core.Snapshot = buildTestSnapshot(t, "prod", 7)
	if s.Env() != "prod" || s.Generation() != 7 || s.Len() != 1 {
		t.Fatalf("env=%q gen=%d len=%d", s.Env(), s.Generation(), s.Len())
	}
	f, ok := s.Flag(flagKey)
	if !ok || f.Key != flagKey {
		t.Fatalf("Flag: %+v %v", f, ok)
	}
	if _, ok := s.Flag("does.not.exist"); ok {
		t.Fatal("a miss must report ok=false, not a zero flag")
	}
}

func TestSnapshotLookupIsMapBacked(t *testing.T) {
	t.Parallel()
	doc := `{"flags":[
      {"key":"a.one","type":"bool","owner":"o","enabled":true,"default_value":false},
      {"key":"a.two","type":"int","owner":"o","enabled":true,"default_value":1},
      {"key":"a.three","type":"string","owner":"o","enabled":false,"default_value":"x"}]}`
	s := buildTestSnapshot(t, "dev", 1, doc)
	if s.Len() != 3 {
		t.Fatalf("len=%d", s.Len())
	}
	for _, k := range []string{"a.one", "a.two", "a.three"} {
		if _, ok := s.Flag(k); !ok {
			t.Fatalf("missing %s", k)
		}
	}
	want := []string{"a.one", "a.three", "a.two"} // sorted
	got := s.Keys()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys() = %v, want sorted %v", got, want)
		}
	}
}

func TestSnapshotKeysAndProvenanceAreCopies(t *testing.T) {
	t.Parallel()
	s := buildTestSnapshot(t, "prod", 1)
	k := s.Keys()
	k[0] = "mutated"
	if s.Keys()[0] != flagKey {
		t.Fatal("Keys() handed out its backing array")
	}
	p, ok := s.Provenance(flagKey)
	if !ok {
		t.Fatal("provenance missing")
	}
	p.Fields[FieldEnabled] = LayerOps
	again, _ := s.Provenance(flagKey)
	if again.Layer(FieldEnabled) != LayerBase {
		t.Fatal("Provenance() handed out its internal map")
	}
}

func TestSnapshotFingerprintTracksContentNotGeneration(t *testing.T) {
	t.Parallel()
	a := buildTestSnapshot(t, "prod", 1)
	b := buildTestSnapshot(t, "prod", 99)
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("identical content must fingerprint identically regardless of generation")
	}
	c := buildTestSnapshot(t, "prod", 1, `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":true}]}`)
	if a.Fingerprint() == c.Fingerprint() {
		t.Fatal("different content must fingerprint differently")
	}
}

func TestSnapshotBuilderProducesFinishedObject(t *testing.T) {
	t.Parallel()
	// The builder hands its maps over and is not reused: the pointer readers load
	// is only ever a completed object (invariant CACHE-2).
	b := newSnapshotBuilder("prod", 2)
	base := mustBase(t, canonicalBase)
	f, prov := mergeFlag(&base.Flags[0], nil, nil)
	b.add(f, prov)
	b.markQuarantined("ghost.flag")
	if b.len() != 1 || b.quarantineCount() != 1 {
		t.Fatalf("builder counters: len=%d quarantined=%d", b.len(), b.quarantineCount())
	}
	s := b.build(3, testNow)
	if s.Len() != 1 || s.QuarantinedCount() != 1 {
		t.Fatalf("snapshot counters: len=%d quarantined=%d", s.Len(), s.QuarantinedCount())
	}
	if !s.IsQuarantined("ghost.flag") {
		t.Fatal("a quarantined flag with no last-known-good must still be counted")
	}
	if _, ok := s.Flag("ghost.flag"); ok {
		t.Fatal("a quarantined flag with no last-known-good must be ABSENT, so the caller applies its L0 default")
	}
	if !s.BuiltAt().Equal(testNow) {
		t.Fatalf("built_at: %s", s.BuiltAt())
	}
}

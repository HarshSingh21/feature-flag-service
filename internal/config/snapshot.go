package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// ResolvedSnapshot is one environment's fully merged, fully validated, immutable
// view of every flag.
//
// It is built to completion in a fresh allocation and published with a single
// atomic pointer store, so no reader can ever observe a partially built snapshot
// (invariant CACHE-2). Nothing mutates it after publication; the evaluator holds
// its pointers without copying or locking.
type ResolvedSnapshot struct {
	env         string
	generation  int64
	builtAt     time.Time
	flags       map[string]*core.Flag
	provenance  map[string]FlagProvenance
	quarantined map[string]struct{}
	warnings    Findings
	keys        []string // sorted, for deterministic iteration and fingerprinting
}

// ResolvedSnapshot satisfies the evaluator's frozen contract.
var _ core.Snapshot = (*ResolvedSnapshot)(nil)

// Generation is a monotonically increasing counter, per environment, per process.
func (s *ResolvedSnapshot) Generation() int64 { return s.generation }

// Env names the environment this snapshot resolves.
func (s *ResolvedSnapshot) Env() string { return s.env }

// Flag returns the resolved flag. The returned pointer is read-only by contract:
// it is shared by every concurrent reader and mutating it is a contract violation,
// not a race the runtime will report.
func (s *ResolvedSnapshot) Flag(key string) (*core.Flag, bool) {
	f, ok := s.flags[key]
	return f, ok
}

// Len reports the number of flags.
func (s *ResolvedSnapshot) Len() int { return len(s.flags) }

// BuiltAt reports when the snapshot was frozen.
func (s *ResolvedSnapshot) BuiltAt() time.Time { return s.builtAt }

// Keys returns the flag keys in sorted order. The returned slice is a copy.
func (s *ResolvedSnapshot) Keys() []string { return append([]string(nil), s.keys...) }

// Provenance returns the per-field winning layer for one flag. This is what backs
// the debug endpoint: during an incident the first question is "what did the base
// say versus the prod overlay", and a merged object without provenance cannot
// answer it. The returned value is a deep copy.
func (s *ResolvedSnapshot) Provenance(key string) (FlagProvenance, bool) {
	p, ok := s.provenance[key]
	if !ok {
		return FlagProvenance{}, false
	}
	return p.Clone(), true
}

// IsQuarantined reports whether this flag is serving a value carried forward from
// an earlier generation because its current config failed validation.
func (s *ResolvedSnapshot) IsQuarantined(key string) bool {
	_, ok := s.quarantined[key]
	return ok
}

// QuarantinedKeys lists the quarantined flags, sorted.
func (s *ResolvedSnapshot) QuarantinedKeys() []string {
	out := make([]string, 0, len(s.quarantined))
	for k := range s.quarantined {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// QuarantinedCount reports how many flags are carrying a previous generation.
func (s *ResolvedSnapshot) QuarantinedCount() int { return len(s.quarantined) }

// Warnings returns the non-blocking findings recorded when this snapshot was
// built. The returned slice is a copy.
func (s *ResolvedSnapshot) Warnings() Findings { return append(Findings(nil), s.warnings...) }

// Fingerprint is a stable content hash over every resolved flag, independent of
// generation and build time. It answers "did the served content actually change?"
// -- which is exactly the question after a rejected config push, where the
// generation alone would not distinguish "unchanged" from "rebuilt identically".
func (s *ResolvedSnapshot) Fingerprint() string {
	h := sha256.New()
	for _, k := range s.keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		b, err := json.Marshal(s.flags[k])
		if err != nil {
			h.Write([]byte("!marshal-error"))
			continue
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// -----------------------------------------------------------------------------
// Builder
// -----------------------------------------------------------------------------

// snapshotBuilder accumulates a snapshot under construction. It is never shared
// with a reader: the pointer that readers load is only stored once build has
// returned a finished, immutable object.
type snapshotBuilder struct {
	env         string
	flags       map[string]*core.Flag
	provenance  map[string]FlagProvenance
	quarantined map[string]struct{}
	warnings    Findings
}

func newSnapshotBuilder(env string, sizeHint int) *snapshotBuilder {
	return &snapshotBuilder{
		env:         env,
		flags:       make(map[string]*core.Flag, sizeHint),
		provenance:  make(map[string]FlagProvenance, sizeHint),
		quarantined: make(map[string]struct{}),
	}
}

func (b *snapshotBuilder) add(f *core.Flag, prov FlagProvenance) {
	b.flags[f.Key] = f
	b.provenance[f.Key] = prov
}

func (b *snapshotBuilder) addQuarantined(f *core.Flag, prov FlagProvenance) {
	b.add(f, prov)
	b.quarantined[f.Key] = struct{}{}
}

// markQuarantined records a flag as quarantined even when there is no
// last-known-good value to carry, so the safety valve counts it.
func (b *snapshotBuilder) markQuarantined(key string) { b.quarantined[key] = struct{}{} }

func (b *snapshotBuilder) warn(fs Findings) { b.warnings = append(b.warnings, fs...) }

func (b *snapshotBuilder) len() int { return len(b.flags) }

func (b *snapshotBuilder) quarantineCount() int { return len(b.quarantined) }

// build freezes the accumulated state into an immutable snapshot. The builder
// must not be used afterwards; it hands its maps over rather than copying them.
func (b *snapshotBuilder) build(generation int64, at time.Time) *ResolvedSnapshot {
	keys := make([]string, 0, len(b.flags))
	for k := range b.flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return &ResolvedSnapshot{
		env:         b.env,
		generation:  generation,
		builtAt:     at,
		flags:       b.flags,
		provenance:  b.provenance,
		quarantined: b.quarantined,
		warnings:    b.warnings,
		keys:        keys,
	}
}

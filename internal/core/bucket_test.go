package core

import (
	"math"
	"strconv"
	"testing"
)

// ============================================================================
//
//	         !!!  GOLDEN VECTORS  --  READ THIS BEFORE "FIXING" A FAILURE  !!!
//
// If TestBucketGoldenVectors fails, DO NOT update the table to match the new
// output. The table is the specification; the code is the thing that changed.
//
// A failure here means the bucketing has changed, and that means EVERY USER IN
// EVERY ACTIVE ROLLOUT WILL BE RE-BUCKETED the moment this ships:
//
//   - Users currently inside a 10% experiment silently leave it. Roughly 18% of
//     the population flips at a 10% rollout, while every dashboard stays green.
//   - Any metric computed over the exposure window is invalidated, because the
//     exposed cohort is not the cohort that was measured.
//   - Users lose a feature mid-session. For a UI feature that is a bug report;
//     for a data-migration flag it is data written in the new format on one
//     request and the old format on the next.
//   - A canary that looked healthy at 1% is now a DIFFERENT 1%, so the canary
//     proved nothing.
//
// The four things that can break this table:
//  1. the hash function or the xxhash dependency version,
//  2. the hash-to-bucket reduction (Lemire multiply-shift on the high 32 bits),
//  3. BucketSpace,
//  4. key construction -- the length prefix, the separators, or the ordering.
//
// Changing any of them is a versioned migration with a bucket_algo discriminator
// on the rollout config, not a patch release. This test is the build gate that
// makes that a deliberate act.
//
// ============================================================================

// goldenVectors are frozen (key -> bucket) pairs. Generated once from this
// implementation and never regenerated.
//
// The set deliberately includes the shapes that would break a naive key format:
// keys containing ':' in the namespace and in the subject, an empty namespace, an
// empty subject, case variants, whitespace, control characters, and multi-byte
// UTF-8. Note the pair {"3:a:b:c" -> 4697} and {"1:a:b:c" -> 1019}: those are
// namespace "a:b"/subject "c" and namespace "a"/subject "b:c", which the length
// prefix keeps distinct and which a plain `namespace + ":" + subject` would have
// collapsed into one shared bucket space.
var goldenVectors = []struct {
	key  string
	want int32
}{
	{key: "11:checkout-v2:u-1", want: 8203},
	{key: "11:checkout-v2:u-2", want: 4932},
	{key: "11:checkout-v2:u-3", want: 3275},
	{key: "11:checkout-v2:", want: 3257},
	{key: "0::u-1", want: 6738},
	{key: "0::", want: 1657},
	{key: "11:new-pricing:u-1", want: 9020},
	{key: "11:new-pricing:u-2", want: 778},
	{key: "11:shared-ramp:u-1", want: 172},
	{key: "11:shared-ramp:tenant-9001", want: 2278},
	{key: "1:a:b", want: 6476},
	{key: "3:a:b:c", want: 4697},
	{key: "1:a:b:c", want: 1019},
	{key: "14:flag.with.dots:user@example.com", want: 8737},
	{key: "5:UPPER:User123", want: 3917},
	{key: "5:UPPER:user123", want: 774},
	{key: "12:unicode-éè:子供", want: 3199},
	{key: "59:long-namespace-that-goes-on-and-on-for-quite-a-while-indeed:u-42", want: 2449},
	{key: "1:n:0", want: 3646},
	{key: "1:n:1", want: 9444},
	{key: "1:n:-1", want: 6283},
	{key: "1:n:9223372036854775807", want: 5473},
	{key: "1:b:true", want: 6767},
	{key: "1:b:false", want: 6097},
	{key: "12:tenant-space:t-000001", want: 1269},
	{key: "12:tenant-space:t-999999", want: 8810},
	{key: "10:emoji-😀:s-🚀", want: 3454},
	{key: "6:sp ace:sub ject", want: 2},
	{key: "6:tab\tns:tab\tsubj", want: 925},
	{key: "8:flag-key:u-1", want: 8968},
}

func TestBucketGoldenVectors(t *testing.T) {
	t.Parallel()
	var h XXHasher
	for _, tc := range goldenVectors {
		if got := h.Bucket(tc.key); got != tc.want {
			t.Errorf("BUCKETING HAS CHANGED. Bucket(%q) = %d, frozen value is %d.\n"+
				"Do not update this table. See the comment at the top of bucket_test.go: "+
				"shipping this re-buckets every user in every active rollout.",
				tc.key, got, tc.want)
		}
	}
}

// TestStrategyComposesGoldenKeys pins the key CONSTRUCTION, not just the hash.
// Golden vectors over hashes alone would not catch a change to the length prefix
// or the separators, which is the other half of the wire format.
func TestStrategyComposesGoldenKeys(t *testing.T) {
	t.Parallel()
	var s NamespaceStrategy
	tests := []struct {
		name    string
		flag    *Flag
		ctx     EvalContext
		wantKey string
		wantOK  bool
	}{
		{
			name:    "default namespace is the flag key",
			flag:    &Flag{Key: "flag-key", Rollout: &Rollout{}},
			ctx:     EvalContext{UserID: "u-1"},
			wantKey: "8:flag-key:u-1", wantOK: true,
		},
		{
			name:    "explicit namespace overrides the flag key",
			flag:    &Flag{Key: "flag-key", Rollout: &Rollout{BucketNamespace: "shared-ramp"}},
			ctx:     EvalContext{UserID: "u-1"},
			wantKey: "11:shared-ramp:u-1", wantOK: true,
		},
		{
			name:    "bucket_by selects a different subject",
			flag:    &Flag{Key: "flag-key", Rollout: &Rollout{BucketNamespace: "shared-ramp", BucketBy: "tenant_id"}},
			ctx:     EvalContext{UserID: "u-1", TenantID: "tenant-9001"},
			wantKey: "11:shared-ramp:tenant-9001", wantOK: true,
		},
		{
			name:    "int subject renders decimal",
			flag:    &Flag{Key: "n", Rollout: &Rollout{BucketBy: "seat"}},
			ctx:     EvalContext{Attributes: map[string]Value{"seat": Int(-1)}},
			wantKey: "1:n:-1", wantOK: true,
		},
		{
			name:    "bool subject renders true/false",
			flag:    &Flag{Key: "b", Rollout: &Rollout{BucketBy: "flagged"}},
			ctx:     EvalContext{Attributes: map[string]Value{"flagged": Bool(true)}},
			wantKey: "1:b:true", wantOK: true,
		},
		{
			name: "the length prefix keeps a colon-bearing namespace distinct",
			flag: &Flag{Key: "a:b", Rollout: &Rollout{BucketBy: "s"}},
			ctx:  EvalContext{Attributes: map[string]Value{"s": String("c")}},
			// namespace "a:b" + subject "c"
			wantKey: "3:a:b:c", wantOK: true,
		},
		{
			name: "the length prefix keeps a colon-bearing subject distinct",
			flag: &Flag{Key: "a", Rollout: &Rollout{BucketBy: "s"}},
			ctx:  EvalContext{Attributes: map[string]Value{"s": String("b:c")}},
			// namespace "a" + subject "b:c" -- the naive format would make this
			// identical to the previous case
			wantKey: "1:a:b:c", wantOK: true,
		},
		{
			name:   "absent subject",
			flag:   &Flag{Key: "flag-key", Rollout: &Rollout{}},
			ctx:    EvalContext{},
			wantOK: false,
		},
		{
			name:   "present-but-empty subject is not a subject",
			flag:   &Flag{Key: "flag-key", Rollout: &Rollout{BucketBy: "sid"}},
			ctx:    EvalContext{Attributes: map[string]Value{"sid": String("")}},
			wantOK: false,
		},
		{
			name:   "present-but-unset value is not a subject",
			flag:   &Flag{Key: "flag-key", Rollout: &Rollout{BucketBy: "sid"}},
			ctx:    EvalContext{Attributes: map[string]Value{"sid": {}}},
			wantOK: false,
		},
		{
			name:   "nil flag is safe",
			flag:   nil,
			ctx:    EvalContext{UserID: "u-1"},
			wantOK: false,
		},
		{
			name:    "nil rollout still composes from the flag key and user_id",
			flag:    &Flag{Key: "flag-key"},
			ctx:     EvalContext{UserID: "u-1"},
			wantKey: "8:flag-key:u-1", wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key, ok := s.Key(tc.flag, tc.ctx)
			if ok != tc.wantOK {
				t.Fatalf("Key ok = %v, want %v (key %q)", ok, tc.wantOK, key)
			}
			if ok && key != tc.wantKey {
				t.Fatalf("KEY CONSTRUCTION HAS CHANGED. Key = %q, frozen value is %q", key, tc.wantKey)
			}
		})
	}
}

// TestBucketKeyInjectivity is the property the length prefix exists to provide,
// asserted directly rather than only through the golden table.
func TestBucketKeyInjectivity(t *testing.T) {
	t.Parallel()
	var s NamespaceStrategy
	// Pairs that a plain `namespace + ":" + subject` would collapse into one key,
	// silently making two different flags share a bucket space.
	pairs := [][2][2]string{
		{{"a:b", "c"}, {"a", "b:c"}},
		{{"x", "y:z"}, {"x:y", "z"}},
		{{"billing:invoice", "u-1"}, {"billing", "invoice:u-1"}},
		{{"", "a:b"}, {"", "a:b"}}, // control: identical inputs must collide
	}
	for i, p := range pairs {
		k0, ok0 := s.Key(&Flag{Key: p[0][0], Rollout: &Rollout{BucketBy: "s"}},
			EvalContext{Attributes: map[string]Value{"s": String(p[0][1])}})
		k1, ok1 := s.Key(&Flag{Key: p[1][0], Rollout: &Rollout{BucketBy: "s"}},
			EvalContext{Attributes: map[string]Value{"s": String(p[1][1])}})
		if !ok0 || !ok1 {
			t.Fatalf("pair %d: key composition failed", i)
		}
		same := p[0] == p[1]
		if (k0 == k1) != same {
			t.Fatalf("pair %d: %q vs %q -- collision=%v, want %v", i, k0, k1, k0 == k1, same)
		}
	}
}

func TestBucketIsAlwaysInRange(t *testing.T) {
	t.Parallel()
	var h XXHasher
	for i := 0; i < 200000; i++ {
		b := h.Bucket("range-probe-" + strconv.Itoa(i))
		if b < 0 || b >= BucketSpace {
			t.Fatalf("Bucket produced %d, outside [0,%d)", b, BucketSpace)
		}
	}
	// The edge inputs too.
	for _, k := range []string{"", "0", "\x00", "\xff\xff\xff\xff\xff\xff\xff\xff"} {
		if b := h.Bucket(k); b < 0 || b >= BucketSpace {
			t.Fatalf("Bucket(%q) = %d, outside range", k, b)
		}
	}
}

// TestBucketDistribution asserts that the hash spreads a realistic population
// evenly across the bucket space.
//
// Tolerance, derived rather than guessed:
//
//	n      = 100,000 subjects
//	p      = 0.1     (one decile of the bucket space)
//	mean   = n*p                   = 10,000 per decile
//	sigma  = sqrt(n*p*(1-p))       = sqrt(9,000) = 94.87
//
// Under a uniform hash each decile count is Binomial(n, 0.1), which at this n is
// indistinguishable from Normal(10000, 94.87). A 4-sigma band is +/- 379 counts.
// For a single decile the two-sided probability of exceeding 4 sigma is ~6.3e-5;
// across 10 deciles, ~6.3e-4.
//
// That flake rate would matter for a randomised test. This one is DETERMINISTIC --
// fixed subject ids, fixed hash -- so it either always passes or always fails, and
// the 4-sigma band is chosen as the point where a failure is far more likely to
// mean "the hash changed" than "we got unlucky". The observed maximum deviation
// for this corpus is 1.86 sigma (176 counts), so there is ~2 sigma of headroom.
//
// A chi-square check follows, because a per-decile band catches a shifted
// distribution but not a lumpy one that happens to keep every decile inside the
// band.
func TestBucketDistribution(t *testing.T) {
	t.Parallel()
	const (
		n       = 100000
		deciles = 10
	)
	sigma := math.Sqrt(float64(n) * 0.1 * 0.9)
	tolerance := 4 * sigma
	mean := float64(n) / deciles

	var s NamespaceStrategy
	var h XXHasher
	flag := &Flag{Key: "dist", Rollout: &Rollout{BucketBy: "sid"}}

	var counts [deciles]int
	for i := 0; i < n; i++ {
		key, ok := s.Key(flag, EvalContext{Attributes: map[string]Value{"sid": String("subject-" + strconv.Itoa(i))}})
		if !ok {
			t.Fatalf("key composition failed at i=%d", i)
		}
		counts[h.Bucket(key)/(int32(BucketSpace)/deciles)]++
	}

	for i, c := range counts {
		dev := math.Abs(float64(c) - mean)
		if dev > tolerance {
			t.Errorf("decile %d holds %d subjects, want %.0f +/- %.0f (deviation %.2f sigma)",
				i, c, mean, tolerance, dev/sigma)
		}
	}

	// Chi-square over 100 bins, 99 degrees of freedom. The 99.9th percentile of
	// chi-square with 99 df is ~148.2; exceeding that means the spread is lumpy in a
	// way the per-decile band would miss.
	const bins = 100
	var binCounts [bins]int
	for i := 0; i < n; i++ {
		key, _ := s.Key(flag, EvalContext{Attributes: map[string]Value{"sid": String("subject-" + strconv.Itoa(i))}})
		binCounts[h.Bucket(key)/(int32(BucketSpace)/bins)]++
	}
	expected := float64(n) / bins
	chi2 := 0.0
	for _, c := range binCounts {
		d := float64(c) - expected
		chi2 += d * d / expected
	}
	if chi2 > 148.2 {
		t.Errorf("chi-square over %d bins = %.2f, exceeds the 99.9th percentile (148.2) for 99 df; the hash is not spreading uniformly", bins, chi2)
	}
}

// TestBucketIsSticky: the same key must produce the same bucket, every time, in
// every process. Stickiness is the whole point -- without it a rollout is a
// coin flip re-tossed on every request.
func TestBucketIsSticky(t *testing.T) {
	t.Parallel()
	var h XXHasher
	const key = "11:checkout-v2:u-1"
	first := h.Bucket(key)
	for i := 0; i < 1000; i++ {
		if got := h.Bucket(key); got != first {
			t.Fatalf("iteration %d: Bucket = %d, first call gave %d", i, got, first)
		}
	}
}

// TestStrategyKeyIsPure: the composed key must not depend on anything that varies
// between calls. A strategy that reached for a clock, a request id, or a random
// source would destroy stickiness, and the failure would be invisible in a
// single-call test.
func TestStrategyKeyIsPure(t *testing.T) {
	t.Parallel()
	var s NamespaceStrategy
	flag := &Flag{Key: "purity", Rollout: &Rollout{BasisPoints: 1234}}
	ctx := EvalContext{UserID: "u-7", Attributes: map[string]Value{"country": String("IN")}}
	first, ok := s.Key(flag, ctx)
	if !ok {
		t.Fatal("key composition failed")
	}
	for i := 0; i < 1000; i++ {
		got, ok := s.Key(flag, ctx)
		if !ok || got != first {
			t.Fatalf("iteration %d: Key = %q/%v, first call gave %q", i, got, ok, first)
		}
	}
}

// TestBucketKeyIgnoresBasisPoints is the guard against the single most common
// bucketing bug: folding the current percentage into the hash, e.g.
// hash(key + ":" + pct). That looks harmless and destroys monotonicity -- every
// ramp step reshuffles the entire population, so raising a rollout from 10% to 20%
// EVICTS roughly half the users who already had the feature.
func TestBucketKeyIgnoresBasisPoints(t *testing.T) {
	t.Parallel()
	var s NamespaceStrategy
	ctx := EvalContext{UserID: "u-7"}
	base, _ := s.Key(&Flag{Key: "ramp", Rollout: &Rollout{BasisPoints: 0}}, ctx)
	for _, bp := range []int32{1, 100, 2500, 5000, 9999, 10000} {
		got, _ := s.Key(&Flag{Key: "ramp", Rollout: &Rollout{BasisPoints: bp}}, ctx)
		if got != base {
			t.Fatalf("BasisPoints=%d changed the bucket key: %q vs %q", bp, got, base)
		}
	}
}

// TestMonotoneRamp: raising a percentage must only ever ADD users.
//
// For a fixed subject the bucket is fixed, and inclusion is `bucket < basisPoints`,
// so enrolment at N implies enrolment at every N+k. Asserted over a population and
// over every basis-point step, because the property that matters operationally is
// the set-inclusion one: nobody loses the feature during a ramp-up.
func TestMonotoneRamp(t *testing.T) {
	t.Parallel()
	var s NamespaceStrategy
	var h XXHasher
	flag := &Flag{Key: "ramp", Rollout: &Rollout{BucketBy: "sid"}}

	const population = 5000
	buckets := make([]int32, population)
	for i := range buckets {
		key, ok := s.Key(flag, EvalContext{Attributes: map[string]Value{"sid": String("u-" + strconv.Itoa(i))}})
		if !ok {
			t.Fatalf("key composition failed at i=%d", i)
		}
		buckets[i] = h.Bucket(key)
	}

	enrolled := func(bp int32) map[int]bool {
		set := make(map[int]bool)
		for i, b := range buckets {
			if b < bp {
				set[i] = true
			}
		}
		return set
	}

	prev := enrolled(0)
	if len(prev) != 0 {
		t.Fatalf("0 basis points enrolled %d subjects, want 0", len(prev))
	}
	for bp := int32(1); bp <= BucketSpace; bp += 37 {
		cur := enrolled(bp)
		for i := range prev {
			if !cur[i] {
				t.Fatalf("subject %d (bucket %d) was enrolled below %d basis points and is NOT enrolled at %d; a ramp-up evicted a user",
					i, buckets[i], bp, bp)
			}
		}
		if len(cur) < len(prev) {
			t.Fatalf("enrolment shrank from %d to %d when raising to %d basis points", len(prev), len(cur), bp)
		}
		prev = cur
	}
	if len(enrolled(BucketSpace)) != population {
		t.Fatalf("10000 basis points enrolled %d of %d subjects, want all", len(enrolled(BucketSpace)), population)
	}
}

func BenchmarkBucketKeyAndHash(b *testing.B) {
	var s NamespaceStrategy
	var h XXHasher
	flag := &Flag{Key: "checkout-v2", Rollout: &Rollout{BasisPoints: 1000}}
	ctx := EvalContext{UserID: "user-000000042"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key, _ := s.Key(flag, ctx)
		_ = h.Bucket(key)
	}
}

package core

import (
	"strconv"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Test doubles. Defined here rather than taken from internal/config: core is
// ring 0 and its tests must not reach up the dependency graph, or the "imports
// nothing that performs I/O" property becomes untestable.
// ---------------------------------------------------------------------------

type fakeSnapshot struct {
	gen   int64
	env   string
	flags map[string]*Flag

	// Fault injection. Each one models a real way a snapshot implementation can
	// betray the evaluator at 3am.
	panicOnFlag       bool
	panicOnGeneration bool
}

func (s *fakeSnapshot) Generation() int64 {
	if s.panicOnGeneration {
		panic("fakeSnapshot: Generation exploded")
	}
	return s.gen
}

func (s *fakeSnapshot) Env() string { return s.env }

func (s *fakeSnapshot) Flag(key string) (*Flag, bool) {
	if s.panicOnFlag {
		panic("fakeSnapshot: Flag exploded")
	}
	f, ok := s.flags[key]
	return f, ok
}

func (s *fakeSnapshot) Len() int { return len(s.flags) }

func snapshotOf(gen int64, flags ...*Flag) *fakeSnapshot {
	s := &fakeSnapshot{gen: gen, env: "test", flags: map[string]*Flag{}}
	for _, f := range flags {
		s.flags[f.Key] = f
	}
	return s
}

// countingStrategy wraps a strategy and counts calls, so a test can assert that a
// stage was never reached rather than only that its output was not used.
type countingStrategy struct {
	inner BucketKeyStrategy
	calls int
}

func (c *countingStrategy) Key(flag *Flag, ctx EvalContext) (string, bool) {
	c.calls++
	return c.inner.Key(flag, ctx)
}

// panicStrategy models a third-party BucketKeyStrategy that faults.
type panicStrategy struct{}

func (panicStrategy) Key(*Flag, EvalContext) (string, bool) { panic("panicStrategy: boom") }

// panicHasher models a third-party Hasher that faults.
type panicHasher struct{}

func (panicHasher) Bucket(string) int32 { panic("panicHasher: boom") }

// fixedHasher returns a chosen bucket, so rollout arithmetic can be tested without
// depending on the golden vectors.
type fixedHasher int32

func (f fixedHasher) Bucket(string) int32 { return int32(f) }

// panicObserver models an observability hook that faults inside the hot path.
type panicObserver struct{}

func (panicObserver) ObserveUndecidable(string, string, string, Tri) { panic("observer: boom") }

func assertResult(t *testing.T, got Result, wantValue Value, wantReason Reason) {
	t.Helper()
	if !got.Value.Equal(wantValue) {
		t.Errorf("Value = %v (%s), want %v (%s)", got.Value, got.Value.Type(), wantValue, wantValue.Type())
	}
	if got.Reason != wantReason {
		t.Errorf("Reason = %s, want %s", got.Reason, wantReason)
	}
	if got.Reason == ReasonUnknown {
		t.Error("Reason is UNKNOWN; a completed evaluation must never return it")
	}
}

// ---------------------------------------------------------------------------
// The pipeline, end to end, for all three flag types.
// ---------------------------------------------------------------------------

func TestEvaluateAllPathsAllTypes(t *testing.T) {
	t.Parallel()

	type typeCase struct {
		name          string
		vt            ValueType
		onValue       Value
		offValue      Value
		defaultValue  Value
		ruleValue     Value
		callerDefault Value
	}
	typeCases := []typeCase{
		{"bool", TypeBool, Bool(true), Bool(false), Bool(false), Bool(true), Bool(false)},
		{"string", TypeString, String("on"), String("off"), String("fallthrough"), String("rule"), String("caller")},
		{"int", TypeInt, Int(1), Int(0), Int(7), Int(42), Int(-1)},
	}

	for _, tc := range typeCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := New()

			t.Run("fallthrough", func(t *testing.T) {
				t.Parallel()
				f := &Flag{Key: "f", Type: tc.vt, Enabled: true, DefaultValue: tc.defaultValue, OffValue: tc.offValue}
				got := e.Evaluate(snapshotOf(3, f), "f", EvalContext{UserID: "u-1"}, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.defaultValue, ReasonFallthrough)
				if got.Bucket != NoBucket {
					t.Errorf("Bucket = %d, want NoBucket on a path with no rollout", got.Bucket)
				}
				if got.Generation != 3 {
					t.Errorf("Generation = %d, want 3", got.Generation)
				}
			})

			t.Run("disabled returns the off value, never the caller default", func(t *testing.T) {
				t.Parallel()
				f := &Flag{Key: "f", Type: tc.vt, Enabled: false, DefaultValue: tc.defaultValue, OffValue: tc.offValue}
				got := e.Evaluate(snapshotOf(3, f), "f", EvalContext{UserID: "u-1"}, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.offValue, ReasonDisabled)
				if got.IsFallback() {
					t.Error("DISABLED is a configured state, not a fallback")
				}
			})

			t.Run("rule match", func(t *testing.T) {
				t.Parallel()
				f := &Flag{
					Key: "f", Type: tc.vt, Enabled: true,
					DefaultValue: tc.defaultValue, OffValue: tc.offValue,
					Rules: []Rule{{
						ID:         "r-beta",
						Conditions: []Condition{{Attribute: "plan", Op: OpEquals, Values: []Value{String("beta")}}},
						Value:      tc.ruleValue,
					}},
				}
				ctx := EvalContext{UserID: "u-1", Attributes: map[string]Value{"plan": String("beta")}}
				got := e.Evaluate(snapshotOf(3, f), "f", ctx, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.ruleValue, ReasonRuleMatch)
				if got.RuleID != "r-beta" {
					t.Errorf("RuleID = %q, want %q", got.RuleID, "r-beta")
				}
			})

			t.Run("rule no match falls through", func(t *testing.T) {
				t.Parallel()
				f := &Flag{
					Key: "f", Type: tc.vt, Enabled: true,
					DefaultValue: tc.defaultValue, OffValue: tc.offValue,
					Rules: []Rule{{
						ID:         "r-beta",
						Conditions: []Condition{{Attribute: "plan", Op: OpEquals, Values: []Value{String("beta")}}},
						Value:      tc.ruleValue,
					}},
				}
				ctx := EvalContext{UserID: "u-1", Attributes: map[string]Value{"plan": String("free")}}
				got := e.Evaluate(snapshotOf(3, f), "f", ctx, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.defaultValue, ReasonFallthrough)
			})

			t.Run("rollout in", func(t *testing.T) {
				t.Parallel()
				f := &Flag{
					Key: "f", Type: tc.vt, Enabled: true,
					DefaultValue: tc.defaultValue, OffValue: tc.offValue,
					Rollout: &Rollout{BasisPoints: 5000, OnValue: tc.onValue, OffValue: tc.offValue},
				}
				ev := New(WithHasher(fixedHasher(4999)))
				got := ev.Evaluate(snapshotOf(3, f), "f", EvalContext{UserID: "u-1"}, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.onValue, ReasonRolloutIn)
				if got.Bucket != 4999 {
					t.Errorf("Bucket = %d, want 4999", got.Bucket)
				}
			})

			t.Run("rollout out at the boundary", func(t *testing.T) {
				t.Parallel()
				f := &Flag{
					Key: "f", Type: tc.vt, Enabled: true,
					DefaultValue: tc.defaultValue, OffValue: tc.offValue,
					Rollout: &Rollout{BasisPoints: 5000, OnValue: tc.onValue, OffValue: tc.offValue},
				}
				// bucket == basisPoints is OUT: inclusion is strictly less-than, which
				// is what makes the ramp monotone.
				ev := New(WithHasher(fixedHasher(5000)))
				got := ev.Evaluate(snapshotOf(3, f), "f", EvalContext{UserID: "u-1"}, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.offValue, ReasonRolloutOut)
				if got.Bucket != 5000 {
					t.Errorf("Bucket = %d, want 5000", got.Bucket)
				}
			})

			t.Run("flag not found", func(t *testing.T) {
				t.Parallel()
				got := e.Evaluate(snapshotOf(3), "nope", EvalContext{UserID: "u-1"}, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.callerDefault, ReasonFlagNotFound)
				if !got.IsFallback() {
					t.Error("FLAG_NOT_FOUND must classify as a fallback")
				}
			})

			t.Run("missing subject", func(t *testing.T) {
				t.Parallel()
				f := &Flag{
					Key: "f", Type: tc.vt, Enabled: true,
					DefaultValue: tc.defaultValue, OffValue: tc.offValue,
					Rollout: &Rollout{BasisPoints: 5000, OnValue: tc.onValue, OffValue: tc.offValue},
				}
				got := e.Evaluate(snapshotOf(3, f), "f", EvalContext{}, tc.vt, tc.callerDefault)
				assertResult(t, got, tc.callerDefault, ReasonMissingSubject)
				if got.Bucket != NoBucket {
					t.Errorf("Bucket = %d, want NoBucket when no bucket was computed", got.Bucket)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Decision O2: rules first, first match wins, rollout only on fallthrough.
// ---------------------------------------------------------------------------

func TestRulesEvaluateInSliceOrderAndFirstMatchWins(t *testing.T) {
	t.Parallel()
	f := &Flag{
		Key: "f", Type: TypeString, Enabled: true,
		DefaultValue: String("default"), OffValue: String("off"),
		Rules: []Rule{
			{ID: "r-1", Conditions: []Condition{{Attribute: "country", Op: OpExists}}, Value: String("first")},
			{ID: "r-2", Conditions: []Condition{{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}}}, Value: String("second")},
		},
	}
	ctx := EvalContext{UserID: "u-1", Attributes: map[string]Value{"country": String("IN")}}
	got := New().Evaluate(snapshotOf(1, f), "f", ctx, TypeString, String("caller"))
	assertResult(t, got, String("first"), ReasonRuleMatch)
	if got.RuleID != "r-1" {
		t.Fatalf("RuleID = %q, want r-1; THE ORDER OF THE RULE SLICE IS THE SEMANTICS", got.RuleID)
	}

	// Reverse the slice and the answer must change. If it does not, the engine is
	// not honouring rule order.
	f.Rules[0], f.Rules[1] = f.Rules[1], f.Rules[0]
	got = New().Evaluate(snapshotOf(1, f), "f", ctx, TypeString, String("caller"))
	if got.RuleID != "r-2" {
		t.Fatalf("after reordering, RuleID = %q, want r-2", got.RuleID)
	}
}

func TestMatchingRulePreventsRolloutFromRunningAtAll(t *testing.T) {
	t.Parallel()
	// Decision O2: the rollout runs ONLY for subjects that fell through every rule.
	// Asserted by counting strategy invocations, not merely by inspecting the value:
	// an implementation that computed the bucket and then discarded it would pass a
	// value-only assertion while still burning the hash and, worse, while making
	// ROLLOUT_IN/RULE_MATCH a compound state.
	strat := &countingStrategy{inner: NamespaceStrategy{}}
	e := New(WithBucketKeyStrategy(strat))

	f := &Flag{
		Key: "f", Type: TypeBool, Enabled: true,
		DefaultValue: Bool(false), OffValue: Bool(false),
		Rules: []Rule{{
			ID:         "r-allow",
			Conditions: []Condition{{Attribute: "plan", Op: OpEquals, Values: []Value{String("internal")}}},
			Value:      Bool(true),
		}},
		Rollout: &Rollout{BasisPoints: 1, OnValue: Bool(true), OffValue: Bool(false)},
	}

	matched := EvalContext{UserID: "u-1", Attributes: map[string]Value{"plan": String("internal")}}
	got := e.Evaluate(snapshotOf(1, f), "f", matched, TypeBool, Bool(false))
	assertResult(t, got, Bool(true), ReasonRuleMatch)
	if strat.calls != 0 {
		t.Fatalf("bucket strategy was invoked %d times for a rule-matched subject; the rollout must not run", strat.calls)
	}
	if got.Bucket != NoBucket {
		t.Fatalf("Bucket = %d on a RULE_MATCH, want NoBucket", got.Bucket)
	}

	// And it DOES run for a subject that falls through.
	fellThrough := EvalContext{UserID: "u-1", Attributes: map[string]Value{"plan": String("free")}}
	got = e.Evaluate(snapshotOf(1, f), "f", fellThrough, TypeBool, Bool(false))
	if strat.calls != 1 {
		t.Fatalf("bucket strategy called %d times for a fall-through subject, want 1", strat.calls)
	}
	if got.Reason != ReasonRolloutIn && got.Reason != ReasonRolloutOut {
		t.Fatalf("Reason = %s, want a rollout reason", got.Reason)
	}
}

func TestRuleWithAbsentAttributeDoesNotMatch(t *testing.T) {
	t.Parallel()
	// The leak this whole design exists to prevent: a negated condition on an
	// attribute that a failed upstream lookup left absent must NOT match. If it did,
	// one geo-IP degradation would target every user on the planet.
	f := &Flag{
		Key: "block-non-india", Type: TypeBool, Enabled: true,
		DefaultValue: Bool(false), OffValue: Bool(false),
		Rules: []Rule{{
			ID:         "r-not-in",
			Conditions: []Condition{{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}, Negate: true}},
			Value:      Bool(true),
		}},
	}
	snap := snapshotOf(1, f)
	e := New()

	// Geo lookup failed: no country attribute at all.
	got := e.Evaluate(snap, "block-non-india", EvalContext{UserID: "u-1"}, TypeBool, Bool(false))
	assertResult(t, got, Bool(false), ReasonFallthrough)

	// Geo lookup succeeded and the user is outside India: the rule DOES apply.
	ctx := EvalContext{UserID: "u-1", Attributes: map[string]Value{"country": String("US")}}
	got = e.Evaluate(snap, "block-non-india", ctx, TypeBool, Bool(false))
	assertResult(t, got, Bool(true), ReasonRuleMatch)
}

func TestRuleWithWrongTypeAttributeDoesNotMatchAndIsCounted(t *testing.T) {
	t.Parallel()
	obs := &recordingObserver{}
	e := New(WithConditionObserver(obs))
	f := &Flag{
		Key: "big-tenants", Type: TypeBool, Enabled: true,
		DefaultValue: Bool(false), OffValue: Bool(false),
		Rules: []Rule{{
			ID:         "r-seats",
			Conditions: []Condition{{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(100)}}},
			Value:      Bool(true),
		}},
	}
	// The producer sent seats as a string. No coercion: "500" is not 500.
	ctx := EvalContext{UserID: "u-1", Attributes: map[string]Value{"seats": String("500")}}
	got := e.Evaluate(snapshotOf(1, f), "big-tenants", ctx, TypeBool, Bool(false))
	assertResult(t, got, Bool(false), ReasonFallthrough)

	if len(obs.calls) != 1 {
		t.Fatalf("observed %d undecidable conditions, want 1 -- a silent false is the leak", len(obs.calls))
	}
	want := observedCall{FlagKey: "big-tenants", RuleID: "r-seats", Attribute: "seats", Outcome: TriBadType}
	if obs.calls[0] != want {
		t.Fatalf("observed %+v, want %+v", obs.calls[0], want)
	}
}

// ---------------------------------------------------------------------------
// Type safety.
// ---------------------------------------------------------------------------

func TestRequestedTypeMismatch(t *testing.T) {
	t.Parallel()
	f := &Flag{Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("v"), OffValue: String("off")}
	got := New().Evaluate(snapshotOf(1, f), "f", EvalContext{UserID: "u-1"}, TypeBool, Bool(false))
	assertResult(t, got, Bool(false), ReasonTypeMismatch)
}

func TestRequestedTypeMismatchIsCheckedBeforeTheEnabledCheck(t *testing.T) {
	t.Parallel()
	// A caller asking for the wrong type is a CODE bug and must surface identically
	// whether the flag is on or off. If the enabled check ran first, the bug would
	// hide behind a switched-off flag until someone flipped the kill switch -- which
	// is precisely the worst moment to discover it.
	f := &Flag{Key: "f", Type: TypeString, Enabled: false, DefaultValue: String("v"), OffValue: String("off")}
	got := New().Evaluate(snapshotOf(1, f), "f", EvalContext{UserID: "u-1"}, TypeBool, Bool(false))
	assertResult(t, got, Bool(false), ReasonTypeMismatch)
	if got.Reason == ReasonDisabled {
		t.Fatal("the type bug hid behind the disabled flag")
	}
}

func TestTypeUnknownAssertsNothing(t *testing.T) {
	t.Parallel()
	// The untyped introspection path (flagctl, an admin UI) passes TypeUnknown and
	// gets the configured value regardless of its type.
	f := &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(9), OffValue: Int(0)}
	got := New().Evaluate(snapshotOf(1, f), "f", EvalContext{UserID: "u-1"}, TypeUnknown, String("caller"))
	assertResult(t, got, Int(9), ReasonFallthrough)
}

func TestMalformedConfiguredValuesAreCaughtOnEveryPath(t *testing.T) {
	t.Parallel()
	// Config-time rejection is how you avoid the incident; eval-time checking is how
	// you survive the config-time check having a bug. Every path is checked,
	// including DISABLED -- a malformed OffValue must not detonate during an
	// incident, which is exactly when the kill switch gets flipped.
	callerDefault := String("caller")

	tests := []struct {
		name string
		flag *Flag
		ctx  EvalContext
		e    *Evaluator
	}{
		{
			name: "malformed OFF value on the DISABLED path",
			flag: &Flag{Key: "f", Type: TypeString, Enabled: false, OffValue: Int(0), DefaultValue: String("ok")},
			ctx:  EvalContext{UserID: "u-1"},
			e:    New(),
		},
		{
			name: "UNSET OFF value on the DISABLED path",
			flag: &Flag{Key: "f", Type: TypeString, Enabled: false, DefaultValue: String("ok")},
			ctx:  EvalContext{UserID: "u-1"},
			e:    New(),
		},
		{
			name: "malformed FALLTHROUGH value",
			flag: &Flag{Key: "f", Type: TypeString, Enabled: true, DefaultValue: Bool(true), OffValue: String("off")},
			ctx:  EvalContext{UserID: "u-1"},
			e:    New(),
		},
		{
			name: "malformed RULE value",
			flag: &Flag{
				Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("ok"), OffValue: String("off"),
				Rules: []Rule{{ID: "r-1", Conditions: []Condition{{Attribute: "user_id", Op: OpExists}}, Value: Int(1)}},
			},
			ctx: EvalContext{UserID: "u-1"},
			e:   New(),
		},
		{
			name: "malformed ROLLOUT ON value",
			flag: &Flag{
				Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("ok"), OffValue: String("off"),
				Rollout: &Rollout{BasisPoints: 10000, OnValue: Int(1), OffValue: String("out")},
			},
			ctx: EvalContext{UserID: "u-1"},
			e:   New(WithHasher(fixedHasher(0))),
		},
		{
			name: "malformed ROLLOUT OFF value",
			flag: &Flag{
				Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("ok"), OffValue: String("off"),
				Rollout: &Rollout{BasisPoints: 0, OnValue: String("in"), OffValue: Bool(false)},
			},
			ctx: EvalContext{UserID: "u-1"},
			e:   New(WithHasher(fixedHasher(0))),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.e.Evaluate(snapshotOf(1, tc.flag), "f", tc.ctx, TypeString, callerDefault)
			assertResult(t, got, callerDefault, ReasonTypeMismatch)
		})
	}
}

func TestTypeMismatchOnARolloutPathKeepsTheBucket(t *testing.T) {
	t.Parallel()
	// "This user was mistyped at bucket 7431 on a 2000 basis-point ramp" is a far
	// more useful log line than "this user was mistyped".
	f := &Flag{
		Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("ok"), OffValue: String("off"),
		Rollout: &Rollout{BasisPoints: 2000, OnValue: String("in"), OffValue: Int(0)},
	}
	got := New(WithHasher(fixedHasher(7431))).Evaluate(snapshotOf(1, f), "f", EvalContext{UserID: "u-1"}, TypeString, String("caller"))
	assertResult(t, got, String("caller"), ReasonTypeMismatch)
	if got.Bucket != 7431 {
		t.Fatalf("Bucket = %d, want 7431 preserved through the type-mismatch path", got.Bucket)
	}
}

// ---------------------------------------------------------------------------
// Rollout behaviour over a population.
// ---------------------------------------------------------------------------

func population(n int) []EvalContext {
	out := make([]EvalContext, n)
	for i := range out {
		out[i] = EvalContext{UserID: "user-" + strconv.Itoa(i)}
	}
	return out
}

func rolloutFlag(key, namespace string, bp int32) *Flag {
	return &Flag{
		Key: key, Type: TypeBool, Enabled: true,
		DefaultValue: Bool(false), OffValue: Bool(false),
		Rollout: &Rollout{BasisPoints: bp, BucketNamespace: namespace, OnValue: Bool(true), OffValue: Bool(false)},
	}
}

func enrolledSet(t *testing.T, e *Evaluator, snap Snapshot, key string, pop []EvalContext) map[string]bool {
	t.Helper()
	set := make(map[string]bool)
	for _, ctx := range pop {
		res := e.Evaluate(snap, key, ctx, TypeBool, Bool(false))
		if res.Reason != ReasonRolloutIn && res.Reason != ReasonRolloutOut {
			t.Fatalf("unexpected reason %s for %s", res.Reason, ctx.UserID)
		}
		if res.Reason == ReasonRolloutIn {
			set[ctx.UserID] = true
		}
	}
	return set
}

func TestRolloutIsSticky(t *testing.T) {
	t.Parallel()
	f := rolloutFlag("f", "", 3000)
	snap := snapshotOf(1, f)
	e := New()
	ctx := EvalContext{UserID: "user-12345"}

	first := e.Evaluate(snap, "f", ctx, TypeBool, Bool(false))
	for i := 0; i < 2000; i++ {
		got := e.Evaluate(snap, "f", ctx, TypeBool, Bool(false))
		if got.Reason != first.Reason || !got.Value.Equal(first.Value) || got.Bucket != first.Bucket {
			t.Fatalf("call %d: got %v/%s/bucket %d, first call gave %v/%s/bucket %d",
				i, got.Value, got.Reason, got.Bucket, first.Value, first.Reason, first.Bucket)
		}
	}
}

func TestZeroPercentEnrolsNobodyAndFullEnrolsEverybody(t *testing.T) {
	t.Parallel()
	pop := population(2000)
	e := New()

	zero := rolloutFlag("zero", "", 0)
	if n := len(enrolledSet(t, e, snapshotOf(1, zero), "zero", pop)); n != 0 {
		t.Fatalf("a 0 basis-point rollout enrolled %d of %d subjects, want 0", n, len(pop))
	}

	full := rolloutFlag("full", "", 10000)
	if n := len(enrolledSet(t, e, snapshotOf(1, full), "full", pop)); n != len(pop) {
		t.Fatalf("a 10000 basis-point rollout enrolled %d of %d subjects, want all", n, len(pop))
	}
}

func TestZeroPercentRolloutReturnsTheRolloutOffValueNotTheFlagDefault(t *testing.T) {
	t.Parallel()
	// The subtle one. A flag being ramped from zero very often has DefaultValue set
	// to the ON value (that is what the ramp is ramping toward). If a 0% rollout were
	// routed to FALLTHROUGH, "set it to 0%" would turn the feature ON for everyone --
	// the precise opposite of what the operator typed. So the rollout gate is
	// `Rollout != nil`, not `BasisPoints > 0`.
	f := &Flag{
		Key: "f", Type: TypeBool, Enabled: true,
		DefaultValue: Bool(true), // the eventual target
		OffValue:     Bool(false),
		Rollout:      &Rollout{BasisPoints: 0, OnValue: Bool(true), OffValue: Bool(false)},
	}
	got := New().Evaluate(snapshotOf(1, f), "f", EvalContext{UserID: "u-1"}, TypeBool, Bool(false))
	assertResult(t, got, Bool(false), ReasonRolloutOut)
}

func TestApproximatelyCorrectEnrolmentRate(t *testing.T) {
	t.Parallel()
	// n = 20,000 subjects at p = 0.25: sigma = sqrt(n*p*(1-p)) = sqrt(3750) = 61.2.
	// A 4-sigma band is +/- 245 subjects, i.e. +/- 1.2 percentage points. The test is
	// deterministic, so this band is about "the ramp dial does not lie", not flake
	// avoidance.
	const n = 20000
	pop := population(n)
	f := rolloutFlag("quarter", "", 2500)
	got := len(enrolledSet(t, New(), snapshotOf(1, f), "quarter", pop))
	if got < n/4-245 || got > n/4+245 {
		t.Fatalf("a 2500 basis-point rollout enrolled %d of %d (%.2f%%), want 25%% +/- 1.2pp",
			got, n, 100*float64(got)/float64(n))
	}
}

func TestFlagsBucketIndependentlyByDefault(t *testing.T) {
	t.Parallel()
	// Decision O1: an empty BucketNamespace means "use the flag key", so two flags at
	// the same percentage hit DIFFERENT cohorts. Without this, one unlucky cohort is
	// the guinea pig for every risky change in the company -- an operational flaw,
	// not just an experiment-design one.
	//
	// Asserted over a population, not on one user: a single user landing in different
	// buckets could be coincidence.
	pop := population(5000)
	e := New()
	a := enrolledSet(t, e, snapshotOf(1, rolloutFlag("checkout-v2", "", 3000)), "checkout-v2", pop)
	b := enrolledSet(t, e, snapshotOf(1, rolloutFlag("new-pricing", "", 3000)), "new-pricing", pop)

	if len(a) == 0 || len(b) == 0 {
		t.Fatal("expected both rollouts to enrol somebody")
	}

	overlap := 0
	for u := range a {
		if b[u] {
			overlap++
		}
	}
	// Independent 30% cohorts over 5000 subjects overlap at ~0.09 * 5000 = 450.
	// Identical cohorts would overlap at ~1500 (all of them). Anything above 900 --
	// double the independent expectation, still far below correlation -- means the
	// flag key is not reaching the hash.
	if overlap > 900 {
		t.Fatalf("cohorts overlap on %d subjects (|a|=%d, |b|=%d); the two flags are correlated, so the flag key is not salting the bucket",
			overlap, len(a), len(b))
	}
	// And the enrolled sets must genuinely differ.
	if len(a) == len(b) && overlap == len(a) {
		t.Fatal("the two flags enrolled identical cohorts")
	}
}

func TestExplicitSharedNamespaceBucketsIdentically(t *testing.T) {
	t.Parallel()
	// The brief's opt-in sharing requirement: setting the SAME literal namespace on
	// two flags makes them share a bucket space deliberately.
	pop := population(3000)
	e := New()
	a := enrolledSet(t, e, snapshotOf(1, rolloutFlag("checkout-v2", "q3-migration", 4000)), "checkout-v2", pop)
	b := enrolledSet(t, e, snapshotOf(1, rolloutFlag("new-pricing", "q3-migration", 4000)), "new-pricing", pop)

	if len(a) != len(b) {
		t.Fatalf("shared-namespace cohorts differ in size: %d vs %d", len(a), len(b))
	}
	for u := range a {
		if !b[u] {
			t.Fatalf("subject %s is enrolled in one flag but not the other despite a shared bucket namespace", u)
		}
	}
	if len(a) == 0 {
		t.Fatal("expected the shared rollout to enrol somebody")
	}

	// Per-subject, the computed bucket must be identical too, not merely the
	// in/out answer.
	for _, ctx := range pop[:50] {
		ra := e.Evaluate(snapshotOf(1, rolloutFlag("checkout-v2", "q3-migration", 4000)), "checkout-v2", ctx, TypeBool, Bool(false))
		rb := e.Evaluate(snapshotOf(1, rolloutFlag("new-pricing", "q3-migration", 4000)), "new-pricing", ctx, TypeBool, Bool(false))
		if ra.Bucket != rb.Bucket {
			t.Fatalf("%s bucketed at %d and %d under a shared namespace", ctx.UserID, ra.Bucket, rb.Bucket)
		}
	}
}

func TestRolloutHonoursBucketBy(t *testing.T) {
	t.Parallel()
	// A whole tenant flipping together is a different unit of rollout from a user,
	// and the config must make that explicit.
	f := rolloutFlag("f", "", 5000)
	f.Rollout.BucketBy = "tenant_id"
	snap := snapshotOf(1, f)
	e := New()

	base := e.Evaluate(snap, "f", EvalContext{UserID: "u-1", TenantID: "t-1"}, TypeBool, Bool(false))
	// Different user, same tenant: identical decision.
	other := e.Evaluate(snap, "f", EvalContext{UserID: "u-99999", TenantID: "t-1"}, TypeBool, Bool(false))
	if base.Bucket != other.Bucket || base.Reason != other.Reason {
		t.Fatalf("two users in tenant t-1 bucketed differently: %d/%s vs %d/%s",
			base.Bucket, base.Reason, other.Bucket, other.Reason)
	}
	// No tenant at all: missing subject, even though a user id is present.
	missing := e.Evaluate(snap, "f", EvalContext{UserID: "u-1"}, TypeBool, Bool(false))
	assertResult(t, missing, Bool(false), ReasonMissingSubject)
}

func TestMonotoneRampThroughTheEvaluator(t *testing.T) {
	t.Parallel()
	// End-to-end restatement of the guarantee: raising the percentage only ever ADDS
	// users. Nobody loses the feature during a ramp-up.
	pop := population(2000)
	e := New()
	var prev map[string]bool
	for _, bp := range []int32{0, 1, 50, 100, 500, 1000, 2500, 5000, 7500, 9999, 10000} {
		cur := enrolledSet(t, e, snapshotOf(1, rolloutFlag("ramp", "", bp)), "ramp", pop)
		for u := range prev {
			if !cur[u] {
				t.Fatalf("subject %s was enrolled below %d basis points and is not enrolled at %d; the ramp evicted a user", u, bp, bp)
			}
		}
		prev = cur
	}
}

// ---------------------------------------------------------------------------
// The never-throw contract.
// ---------------------------------------------------------------------------

func TestPanicInjection(t *testing.T) {
	t.Parallel()
	callerDefault := String("caller-default")

	tests := []struct {
		name string
		run  func() Result
	}{
		{
			name: "snapshot Flag() panics",
			run: func() Result {
				snap := &fakeSnapshot{gen: 7, panicOnFlag: true, flags: map[string]*Flag{}}
				return New().Evaluate(snap, "f", EvalContext{UserID: "u-1"}, TypeString, callerDefault)
			},
		},
		{
			name: "snapshot Generation() panics",
			run: func() Result {
				snap := &fakeSnapshot{panicOnGeneration: true, flags: map[string]*Flag{}}
				return New().Evaluate(snap, "f", EvalContext{UserID: "u-1"}, TypeString, callerDefault)
			},
		},
		{
			name: "bucket key strategy panics",
			run: func() Result {
				f := &Flag{Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("v"), OffValue: String("off"),
					Rollout: &Rollout{BasisPoints: 5000, OnValue: String("in"), OffValue: String("out")}}
				return New(WithBucketKeyStrategy(panicStrategy{})).
					Evaluate(snapshotOf(7, f), "f", EvalContext{UserID: "u-1"}, TypeString, callerDefault)
			},
		},
		{
			name: "hasher panics",
			run: func() Result {
				f := &Flag{Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("v"), OffValue: String("off"),
					Rollout: &Rollout{BasisPoints: 5000, OnValue: String("in"), OffValue: String("out")}}
				return New(WithHasher(panicHasher{})).
					Evaluate(snapshotOf(7, f), "f", EvalContext{UserID: "u-1"}, TypeString, callerDefault)
			},
		},
		{
			name: "observability hook panics inside the hot path",
			run: func() Result {
				f := &Flag{Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("v"), OffValue: String("off"),
					Rules: []Rule{{ID: "r", Conditions: []Condition{{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(1)}}}, Value: String("r")}}}
				ctx := EvalContext{UserID: "u-1", Attributes: map[string]Value{"seats": String("bad")}}
				return New(WithConditionObserver(panicObserver{})).
					Evaluate(snapshotOf(7, f), "f", ctx, TypeString, callerDefault)
			},
		},
		{
			name: "typed-nil snapshot inside a non-nil interface",
			run: func() Result {
				var snap *fakeSnapshot // nil pointer, non-nil interface
				return New().Evaluate(snap, "f", EvalContext{UserID: "u-1"}, TypeString, callerDefault)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.run()
			assertResult(t, got, callerDefault, ReasonError)
			if got.Bucket != NoBucket {
				t.Errorf("Bucket = %d after a recovered panic, want NoBucket", got.Bucket)
			}
		})
	}
}

func TestPanicAfterGenerationIsKnownStillReportsIt(t *testing.T) {
	t.Parallel()
	// "Which config decided this?" is the first question asked during an incident.
	// The recover path must not throw the answer away when it is already in hand.
	snap := &fakeSnapshot{gen: 4242, panicOnFlag: true, flags: map[string]*Flag{}}
	got := New().Evaluate(snap, "f", EvalContext{UserID: "u-1"}, TypeString, String("d"))
	assertResult(t, got, String("d"), ReasonError)
	if got.Generation != 4242 {
		t.Fatalf("Generation = %d, want 4242 preserved across the recover", got.Generation)
	}
}

func TestDegenerateInputs(t *testing.T) {
	t.Parallel()
	callerDefault := Int(-1)

	tests := []struct {
		name       string
		e          *Evaluator
		snap       Snapshot
		key        string
		ctx        EvalContext
		wantReason Reason
	}{
		{
			name: "nil snapshot", e: New(), snap: nil, key: "f",
			ctx: EvalContext{UserID: "u-1"}, wantReason: ReasonError,
		},
		{
			name: "empty snapshot", e: New(), snap: snapshotOf(1), key: "f",
			ctx: EvalContext{UserID: "u-1"}, wantReason: ReasonFlagNotFound,
		},
		{
			name: "nil *Flag stored under a present key", e: New(),
			snap: &fakeSnapshot{gen: 1, flags: map[string]*Flag{"f": nil}}, key: "f",
			ctx: EvalContext{UserID: "u-1"}, wantReason: ReasonError,
		},
		{
			name: "empty flag key", e: New(),
			snap: snapshotOf(1), key: "",
			ctx: EvalContext{UserID: "u-1"}, wantReason: ReasonFlagNotFound,
		},
		{
			// A Hasher is an injectable interface, so a third-party implementation can
			// hand back a bucket outside the space. Refuse it rather than deriving a
			// rollout decision from a broken bucket space.
			name: "hasher returns an out-of-range bucket",
			e:    New(WithHasher(fixedHasher(BucketSpace))),
			snap: snapshotOf(1, &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(1), OffValue: Int(0),
				Rollout: &Rollout{BasisPoints: 5000, OnValue: Int(1), OffValue: Int(0)}}),
			key: "f", ctx: EvalContext{UserID: "u-1"}, wantReason: ReasonError,
		},
		{
			name: "hasher returns a negative bucket",
			e:    New(WithHasher(fixedHasher(-5))),
			snap: snapshotOf(1, &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(1), OffValue: Int(0),
				Rollout: &Rollout{BasisPoints: 5000, OnValue: Int(1), OffValue: Int(0)}}),
			key: "f", ctx: EvalContext{UserID: "u-1"}, wantReason: ReasonError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.e.Evaluate(tc.snap, tc.key, tc.ctx, TypeInt, callerDefault)
			assertResult(t, got, callerDefault, tc.wantReason)
		})
	}
}

func TestNilAndEmptyConfigShapesAreSafe(t *testing.T) {
	t.Parallel()
	e := New()
	tests := []struct {
		name       string
		flag       *Flag
		ctx        EvalContext
		wantValue  Value
		wantReason Reason
	}{
		{
			name:      "nil rules slice",
			flag:      &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0)},
			ctx:       EvalContext{UserID: "u-1"},
			wantValue: Int(5), wantReason: ReasonFallthrough,
		},
		{
			name:      "empty rules slice",
			flag:      &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0), Rules: []Rule{}},
			ctx:       EvalContext{UserID: "u-1"},
			wantValue: Int(5), wantReason: ReasonFallthrough,
		},
		{
			name: "rule with a nil condition slice matches vacuously",
			// Defence in depth: the validator rejects empty rules, but if one reaches
			// the engine its behaviour is defined and tested rather than emergent.
			flag: &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0),
				Rules: []Rule{{ID: "r-empty", Combiner: LogicAnd, Value: Int(99)}}},
			ctx:       EvalContext{UserID: "u-1"},
			wantValue: Int(99), wantReason: ReasonRuleMatch,
		},
		{
			name: "OR rule with a nil condition slice matches nobody",
			flag: &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0),
				Rules: []Rule{{ID: "r-empty", Combiner: LogicOr, Value: Int(99)}}},
			ctx:       EvalContext{UserID: "u-1"},
			wantValue: Int(5), wantReason: ReasonFallthrough,
		},
		{
			name:      "nil rollout",
			flag:      &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0), Rollout: nil},
			ctx:       EvalContext{UserID: "u-1"},
			wantValue: Int(5), wantReason: ReasonFallthrough,
		},
		{
			name:      "nil attributes map",
			flag:      &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0), Rules: []Rule{{ID: "r", Conditions: []Condition{{Attribute: "x", Op: OpEquals, Values: []Value{String("y")}}}, Value: Int(1)}}},
			ctx:       EvalContext{UserID: "u-1", Attributes: nil},
			wantValue: Int(5), wantReason: ReasonFallthrough,
		},
		{
			name: "wholly empty EvalContext against a rules-only flag",
			flag: &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0),
				Rules: []Rule{{ID: "r", Conditions: []Condition{{Attribute: "user_id", Op: OpExists}}, Value: Int(1)}}},
			ctx:       EvalContext{},
			wantValue: Int(5), wantReason: ReasonFallthrough,
		},
		{
			name: "negative basis points enrol nobody",
			flag: &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0),
				Rollout: &Rollout{BasisPoints: -1, OnValue: Int(1), OffValue: Int(0)}},
			ctx:       EvalContext{UserID: "u-1"},
			wantValue: Int(0), wantReason: ReasonRolloutOut,
		},
		{
			name: "absurdly large basis points enrol everybody without overflowing",
			flag: &Flag{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(5), OffValue: Int(0),
				Rollout: &Rollout{BasisPoints: 1 << 30, OnValue: Int(1), OffValue: Int(0)}},
			ctx:       EvalContext{UserID: "u-1"},
			wantValue: Int(1), wantReason: ReasonRolloutIn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := e.Evaluate(snapshotOf(1, tc.flag), "f", tc.ctx, TypeInt, Int(-1))
			assertResult(t, got, tc.wantValue, tc.wantReason)
		})
	}
}

func TestReasonIsNeverUnknown(t *testing.T) {
	t.Parallel()
	// Sweep every reachable shape and assert the invariant holds on all of them.
	flags := []*Flag{
		{Key: "a", Type: TypeBool, Enabled: true, DefaultValue: Bool(true), OffValue: Bool(false)},
		{Key: "b", Type: TypeBool, Enabled: false, DefaultValue: Bool(true), OffValue: Bool(false)},
		{Key: "c", Type: TypeString, Enabled: true, DefaultValue: String("d"), OffValue: String("o")},
		{Key: "d", Type: TypeInt, Enabled: true, DefaultValue: Int(1), OffValue: Int(0),
			Rollout: &Rollout{BasisPoints: 5000, OnValue: Int(1), OffValue: Int(0)}},
		{Key: "e", Type: TypeInt, Enabled: true, DefaultValue: Bool(true)}, // malformed
		{Key: "f", Type: TypeInt, Enabled: true, DefaultValue: Int(1), OffValue: Int(0),
			Rules: []Rule{{ID: "r", Conditions: []Condition{{Attribute: "q", Op: OpUnknown}}, Value: Int(2)}}},
	}
	snap := snapshotOf(9, flags...)
	contexts := []EvalContext{
		{},
		{UserID: "u-1"},
		{UserID: "u-1", Attributes: map[string]Value{"q": String("x")}},
		{TenantID: "t-1"},
	}
	keys := []string{"a", "b", "c", "d", "e", "f", "absent"}
	types := []ValueType{TypeUnknown, TypeBool, TypeString, TypeInt}
	e := New()
	for _, k := range keys {
		for _, ctx := range contexts {
			for _, ty := range types {
				got := e.Evaluate(snap, k, ctx, ty, String("caller"))
				if got.Reason == ReasonUnknown {
					t.Fatalf("key=%q type=%s ctx=%+v produced ReasonUnknown", k, ty, ctx)
				}
				if got.Value.IsUnknown() {
					t.Fatalf("key=%q type=%s ctx=%+v produced an unset Value", k, ty, ctx)
				}
			}
		}
	}
}

func TestEvaluatorOptionsRejectNilPlugins(t *testing.T) {
	t.Parallel()
	// A nil strategy or hasher would nil-panic on every evaluation. Options refuse
	// them so a mis-wired construction degrades to the shipped defaults rather than
	// to a process-wide fault.
	e := New(WithBucketKeyStrategy(nil), WithHasher(nil), nil)
	f := rolloutFlag("f", "", 10000)
	got := e.Evaluate(snapshotOf(1, f), "f", EvalContext{UserID: "u-1"}, TypeBool, Bool(false))
	assertResult(t, got, Bool(true), ReasonRolloutIn)
}

func TestEvaluateIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	// The evaluator holds no mutable state after construction and the snapshot is
	// immutable; this is here so `go test -race` has something to prove it against.
	f := &Flag{
		Key: "f", Type: TypeString, Enabled: true, DefaultValue: String("d"), OffValue: String("o"),
		Rules:   []Rule{{ID: "r", Conditions: []Condition{{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}}}, Value: String("rule")}},
		Rollout: &Rollout{BasisPoints: 5000, OnValue: String("in"), OffValue: String("out")},
	}
	snap := snapshotOf(1, f)
	e := New()

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				ctx := EvalContext{UserID: "u-" + strconv.Itoa(g*500+i)}
				if res := e.Evaluate(snap, "f", ctx, TypeString, String("caller")); res.Reason == ReasonUnknown {
					t.Errorf("goroutine %d iteration %d produced ReasonUnknown", g, i)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Benchmarks. The design claims ~0.3 us typical and ~3.4 us worst realistic;
// these are the evidence.
// ---------------------------------------------------------------------------

// simpleFlag is the common case: enabled, no rules, no rollout.
func simpleFlag() *Flag {
	return &Flag{Key: "simple", Type: TypeBool, Enabled: true, DefaultValue: Bool(true), OffValue: Bool(false)}
}

// realisticFlag is 10 rules x 3 conditions, which is at the lint warning threshold
// for rule count -- i.e. the worst flag we intend to allow into the corpus.
func realisticFlag() *Flag {
	f := &Flag{
		Key: "realistic", Type: TypeBool, Enabled: true,
		DefaultValue: Bool(false), OffValue: Bool(false),
		Rollout: &Rollout{BasisPoints: 2500, OnValue: Bool(true), OffValue: Bool(false)},
	}
	for i := 0; i < 10; i++ {
		f.Rules = append(f.Rules, Rule{
			ID:       "r-" + strconv.Itoa(i),
			Combiner: LogicAnd,
			Conditions: []Condition{
				{Attribute: "country", Op: OpIn, Values: []Value{String("XX" + strconv.Itoa(i)), String("YY")}},
				{Attribute: "seats", Op: OpGreaterOrEqual, Values: []Value{Int(int64(1000 + i))}},
				{Attribute: "app_version", Op: OpSemverGreaterThan, Values: []Value{String("9.9." + strconv.Itoa(i))}},
			},
			Value: Bool(true),
		})
	}
	return f
}

func BenchmarkEvaluateSimple(b *testing.B) {
	e := New()
	snap := snapshotOf(1, simpleFlag())
	ctx := EvalContext{UserID: "user-000000042"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := e.Evaluate(snap, "simple", ctx, TypeBool, Bool(false)); r.Reason == ReasonUnknown {
			b.Fatal("unknown reason")
		}
	}
}

func BenchmarkEvaluateRollout(b *testing.B) {
	e := New()
	snap := snapshotOf(1, &Flag{Key: "ramp", Type: TypeBool, Enabled: true,
		DefaultValue: Bool(false), OffValue: Bool(false),
		Rollout: &Rollout{BasisPoints: 2500, OnValue: Bool(true), OffValue: Bool(false)}})
	ctx := EvalContext{UserID: "user-000000042"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := e.Evaluate(snap, "ramp", ctx, TypeBool, Bool(false)); r.Reason == ReasonUnknown {
			b.Fatal("unknown reason")
		}
	}
}

// BenchmarkEvaluateRealisticWorstCase walks all 10 rules (none match) and then
// buckets. This is the number the capacity plan is built on.
func BenchmarkEvaluateRealisticWorstCase(b *testing.B) {
	e := New()
	snap := snapshotOf(1, realisticFlag())
	ctx := EvalContext{
		UserID: "user-000000042",
		Attributes: map[string]Value{
			"country":     String("IN"),
			"seats":       Int(12),
			"app_version": String("1.2.3"),
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := e.Evaluate(snap, "realistic", ctx, TypeBool, Bool(false)); r.Reason == ReasonUnknown {
			b.Fatal("unknown reason")
		}
	}
}

// BenchmarkEvaluateRealisticFirstRuleMatches is the best case for the same flag:
// rule 0 matches, so nine rules and the rollout are never touched.
func BenchmarkEvaluateRealisticFirstRuleMatches(b *testing.B) {
	e := New()
	f := realisticFlag()
	snap := snapshotOf(1, f)
	ctx := EvalContext{
		UserID: "user-000000042",
		Attributes: map[string]Value{
			"country":     String("XX0"),
			"seats":       Int(5000),
			"app_version": String("10.0.0"),
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := e.Evaluate(snap, "realistic", ctx, TypeBool, Bool(false)); r.Reason != ReasonRuleMatch {
			b.Fatalf("reason = %s, want RULE_MATCH", r.Reason)
		}
	}
}

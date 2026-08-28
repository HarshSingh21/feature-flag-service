package core

import "testing"

// recordingObserver captures undecidable outcomes so tests can assert that a silent
// false was in fact counted.
type recordingObserver struct {
	calls []observedCall
}

type observedCall struct {
	FlagKey   string
	RuleID    string
	Attribute string
	Outcome   Tri
}

func (o *recordingObserver) ObserveUndecidable(flagKey, ruleID, attribute string, outcome Tri) {
	o.calls = append(o.calls, observedCall{flagKey, ruleID, attribute, outcome})
}

func ctxWith(kv map[string]Value) EvalContext { return EvalContext{Attributes: kv} }

func TestMatchConditionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		cond        Condition
		ctx         EvalContext
		wantMatched bool
		wantOutcome Tri
	}{
		// ---- eq -------------------------------------------------------------
		{
			name:        "eq string hit",
			cond:        Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "eq string miss",
			cond:        Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}},
			ctx:         ctxWith(map[string]Value{"country": String("US")}),
			wantMatched: false, wantOutcome: TriFalse,
		},
		{
			name:        "eq int hit",
			cond:        Condition{Attribute: "seats", Op: OpEquals, Values: []Value{Int(5)}},
			ctx:         ctxWith(map[string]Value{"seats": Int(5)}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "eq bool hit",
			cond:        Condition{Attribute: "beta", Op: OpEquals, Values: []Value{Bool(true)}},
			ctx:         ctxWith(map[string]Value{"beta": Bool(true)}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "eq present-but-empty string is decided, not absent",
			cond:        Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("")}},
			ctx:         ctxWith(map[string]Value{"country": String("")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "eq with empty Values is a config defect: false and counted",
			cond:        Condition{Attribute: "country", Op: OpEquals},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: false, wantOutcome: TriBadOp,
		},

		// ---- NO COERCION, EVER ----------------------------------------------
		{
			name:        `string "1" is not int 1`,
			cond:        Condition{Attribute: "seats", Op: OpEquals, Values: []Value{Int(1)}},
			ctx:         ctxWith(map[string]Value{"seats": String("1")}),
			wantMatched: false, wantOutcome: TriBadType,
		},
		{
			name:        `string "true" is not bool true`,
			cond:        Condition{Attribute: "beta", Op: OpEquals, Values: []Value{Bool(true)}},
			ctx:         ctxWith(map[string]Value{"beta": String("true")}),
			wantMatched: false, wantOutcome: TriBadType,
		},
		{
			name:        "int 1 is not bool true",
			cond:        Condition{Attribute: "beta", Op: OpEquals, Values: []Value{Bool(true)}},
			ctx:         ctxWith(map[string]Value{"beta": Int(1)}),
			wantMatched: false, wantOutcome: TriBadType,
		},

		// ---- ABSENCE: the rule that prevents the classic leak ----------------
		{
			name:        "absent attribute is FALSE",
			cond:        Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}},
			ctx:         ctxWith(nil),
			wantMatched: false, wantOutcome: TriAbsent,
		},
		{
			name: `country != "IN" with ABSENT country is FALSE, not true`,
			// This is the bug in production at more companies than you would like.
			// A failed upstream geo lookup must not silently target the planet.
			cond:        Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}, Negate: true},
			ctx:         ctxWith(nil),
			wantMatched: false, wantOutcome: TriAbsent,
		},
		{
			name:        "negated in with absent attribute is FALSE",
			cond:        Condition{Attribute: "plan", Op: OpIn, Values: []Value{String("free")}, Negate: true},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: false, wantOutcome: TriAbsent,
		},
		{
			name:        "negated gt with absent attribute is FALSE",
			cond:        Condition{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(10)}, Negate: true},
			ctx:         ctxWith(nil),
			wantMatched: false, wantOutcome: TriAbsent,
		},
		{
			name:        "negation of a WRONG-TYPE attribute is also FALSE",
			cond:        Condition{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(10)}, Negate: true},
			ctx:         ctxWith(map[string]Value{"seats": String("50")}),
			wantMatched: false, wantOutcome: TriBadType,
		},
		{
			name:        "negation applies normally to a DECIDED false",
			cond:        Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}, Negate: true},
			ctx:         ctxWith(map[string]Value{"country": String("US")}),
			wantMatched: true, wantOutcome: TriFalse,
		},

		// ---- exists: the one operator defined on absence ---------------------
		{
			name:        "exists on present attribute",
			cond:        Condition{Attribute: "country", Op: OpExists},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "exists on present-but-empty attribute is TRUE",
			cond:        Condition{Attribute: "country", Op: OpExists},
			ctx:         ctxWith(map[string]Value{"country": String("")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "exists on absent attribute",
			cond:        Condition{Attribute: "country", Op: OpExists},
			ctx:         ctxWith(nil),
			wantMatched: false, wantOutcome: TriFalse,
		},
		{
			name:        "NOT exists on absent attribute is TRUE - negation IS legal here",
			cond:        Condition{Attribute: "country", Op: OpExists, Negate: true},
			ctx:         ctxWith(nil),
			wantMatched: true, wantOutcome: TriFalse,
		},
		{
			name:        "NOT exists on present attribute is FALSE",
			cond:        Condition{Attribute: "country", Op: OpExists, Negate: true},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: false, wantOutcome: TriTrue,
		},

		// ---- in --------------------------------------------------------------
		{
			name:        "in hit",
			cond:        Condition{Attribute: "country", Op: OpIn, Values: []Value{String("IN"), String("US")}},
			ctx:         ctxWith(map[string]Value{"country": String("US")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "in miss",
			cond:        Condition{Attribute: "country", Op: OpIn, Values: []Value{String("IN"), String("US")}},
			ctx:         ctxWith(map[string]Value{"country": String("DE")}),
			wantMatched: false, wantOutcome: TriFalse,
		},
		{
			name:        "in over an empty set is a decided false, not a defect",
			cond:        Condition{Attribute: "country", Op: OpIn},
			ctx:         ctxWith(map[string]Value{"country": String("DE")}),
			wantMatched: false, wantOutcome: TriFalse,
		},
		{
			name:        "in where no member shares the attribute type is a type mismatch",
			cond:        Condition{Attribute: "seats", Op: OpIn, Values: []Value{String("1"), String("2")}},
			ctx:         ctxWith(map[string]Value{"seats": Int(1)}),
			wantMatched: false, wantOutcome: TriBadType,
		},
		{
			name:        "in with a mixed-type set still matches on the comparable member",
			cond:        Condition{Attribute: "seats", Op: OpIn, Values: []Value{String("x"), Int(7)}},
			ctx:         ctxWith(map[string]Value{"seats": Int(7)}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "negated in over a hit",
			cond:        Condition{Attribute: "country", Op: OpIn, Values: []Value{String("IN")}, Negate: true},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: false, wantOutcome: TriTrue,
		},

		// ---- contains --------------------------------------------------------
		{
			name:        "contains hit",
			cond:        Condition{Attribute: "email", Op: OpContains, Values: []Value{String("@corp.")}},
			ctx:         ctxWith(map[string]Value{"email": String("a@corp.example")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "contains miss",
			cond:        Condition{Attribute: "email", Op: OpContains, Values: []Value{String("@corp.")}},
			ctx:         ctxWith(map[string]Value{"email": String("a@other.example")}),
			wantMatched: false, wantOutcome: TriFalse,
		},
		{
			name:        "contains on an int attribute is a type mismatch, not a stringify",
			cond:        Condition{Attribute: "seats", Op: OpContains, Values: []Value{String("4")}},
			ctx:         ctxWith(map[string]Value{"seats": Int(42)}),
			wantMatched: false, wantOutcome: TriBadType,
		},
		{
			name:        "contains with an int operand is a type mismatch",
			cond:        Condition{Attribute: "email", Op: OpContains, Values: []Value{Int(4)}},
			ctx:         ctxWith(map[string]Value{"email": String("a4b")}),
			wantMatched: false, wantOutcome: TriBadType,
		},

		// ---- ordered comparisons: TypeInt only -------------------------------
		{
			name:        "gt hit",
			cond:        Condition{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(10)}},
			ctx:         ctxWith(map[string]Value{"seats": Int(11)}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "gt boundary is exclusive",
			cond:        Condition{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(10)}},
			ctx:         ctxWith(map[string]Value{"seats": Int(10)}),
			wantMatched: false, wantOutcome: TriFalse,
		},
		{
			name:        "gte boundary is inclusive",
			cond:        Condition{Attribute: "seats", Op: OpGreaterOrEqual, Values: []Value{Int(10)}},
			ctx:         ctxWith(map[string]Value{"seats": Int(10)}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "lt hit",
			cond:        Condition{Attribute: "seats", Op: OpLessThan, Values: []Value{Int(10)}},
			ctx:         ctxWith(map[string]Value{"seats": Int(9)}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "lte boundary is inclusive",
			cond:        Condition{Attribute: "seats", Op: OpLessOrEqual, Values: []Value{Int(10)}},
			ctx:         ctxWith(map[string]Value{"seats": Int(10)}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name: "gt on a string is a CONFIG ERROR, not a lexicographic compare",
			// Silently ordering "10" below "9" is worse than not answering.
			cond:        Condition{Attribute: "plan", Op: OpGreaterThan, Values: []Value{String("gold")}},
			ctx:         ctxWith(map[string]Value{"plan": String("silver")}),
			wantMatched: false, wantOutcome: TriBadType,
		},
		{
			name:        "gt on a bool is a config error",
			cond:        Condition{Attribute: "beta", Op: OpGreaterThan, Values: []Value{Bool(false)}},
			ctx:         ctxWith(map[string]Value{"beta": Bool(true)}),
			wantMatched: false, wantOutcome: TriBadType,
		},
		{
			name:        "gt with no operand is a config defect",
			cond:        Condition{Attribute: "seats", Op: OpGreaterThan},
			ctx:         ctxWith(map[string]Value{"seats": Int(5)}),
			wantMatched: false, wantOutcome: TriBadOp,
		},
		{
			name:        "negative ints order correctly",
			cond:        Condition{Attribute: "delta", Op: OpLessThan, Values: []Value{Int(0)}},
			ctx:         ctxWith(map[string]Value{"delta": Int(-1)}),
			wantMatched: true, wantOutcome: TriTrue,
		},

		// ---- semver ----------------------------------------------------------
		{
			name:        "semver_gt hit",
			cond:        Condition{Attribute: "app_version", Op: OpSemverGreaterThan, Values: []Value{String("2.3.0")}},
			ctx:         ctxWith(map[string]Value{"app_version": String("2.10.0")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "semver_gt: a prerelease does not outrank its release",
			cond:        Condition{Attribute: "app_version", Op: OpSemverGreaterThan, Values: []Value{String("2.3.0")}},
			ctx:         ctxWith(map[string]Value{"app_version": String("2.3.0-rc1")}),
			wantMatched: false, wantOutcome: TriFalse,
		},
		{
			name:        "semver_lt: a prerelease sorts below its release",
			cond:        Condition{Attribute: "app_version", Op: OpSemverLessThan, Values: []Value{String("2.3.0")}},
			ctx:         ctxWith(map[string]Value{"app_version": String("2.3.0-rc1")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "semver_eq ignores build metadata",
			cond:        Condition{Attribute: "app_version", Op: OpSemverEqual, Values: []Value{String("2.3.0")}},
			ctx:         ctxWith(map[string]Value{"app_version": String("2.3.0+deadbeef")}),
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        `unparseable version "2.14" does not compare - false and counted`,
			cond:        Condition{Attribute: "app_version", Op: OpSemverGreaterThan, Values: []Value{String("2.3.0")}},
			ctx:         ctxWith(map[string]Value{"app_version": String("2.14")}),
			wantMatched: false, wantOutcome: TriBadValue,
		},
		{
			name:        "negated semver on an unparseable version stays FALSE",
			cond:        Condition{Attribute: "app_version", Op: OpSemverLessThan, Values: []Value{String("2.3.0")}, Negate: true},
			ctx:         ctxWith(map[string]Value{"app_version": String("garbage")}),
			wantMatched: false, wantOutcome: TriBadValue,
		},
		{
			name:        "semver on an int attribute is a type mismatch",
			cond:        Condition{Attribute: "app_version", Op: OpSemverEqual, Values: []Value{String("2.3.0")}},
			ctx:         ctxWith(map[string]Value{"app_version": Int(2)}),
			wantMatched: false, wantOutcome: TriBadType,
		},

		// ---- shorthand attributes -------------------------------------------
		{
			name:        "user_id resolves from the shorthand field",
			cond:        Condition{Attribute: "user_id", Op: OpEquals, Values: []Value{String("u-1")}},
			ctx:         EvalContext{UserID: "u-1"},
			wantMatched: true, wantOutcome: TriTrue,
		},
		{
			name:        "tenant_id resolves from the shorthand field",
			cond:        Condition{Attribute: "tenant_id", Op: OpIn, Values: []Value{String("t-1"), String("t-2")}},
			ctx:         EvalContext{TenantID: "t-2"},
			wantMatched: true, wantOutcome: TriTrue,
		},

		// ---- unknown operator ------------------------------------------------
		{
			name:        "unknown operator is false and counted, never silently unmatchable",
			cond:        Condition{Attribute: "country", Op: OpUnknown, Values: []Value{String("IN")}},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: false, wantOutcome: TriBadOp,
		},
		{
			name:        "negated unknown operator is still false",
			cond:        Condition{Attribute: "country", Op: OpUnknown, Values: []Value{String("IN")}, Negate: true},
			ctx:         ctxWith(map[string]Value{"country": String("IN")}),
			wantMatched: false, wantOutcome: TriBadOp,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cond := tc.cond
			matched, outcome := MatchCondition(&cond, tc.ctx)
			if matched != tc.wantMatched || outcome != tc.wantOutcome {
				t.Fatalf("MatchCondition = (%v, %s), want (%v, %s)", matched, outcome, tc.wantMatched, tc.wantOutcome)
			}
		})
	}
}

func TestMatchConditionNilIsSafe(t *testing.T) {
	t.Parallel()
	matched, outcome := MatchCondition(nil, EvalContext{})
	if matched || outcome != TriBadOp {
		t.Fatalf("nil condition = (%v, %s), want (false, bad_op)", matched, outcome)
	}
}

func TestTriClassification(t *testing.T) {
	t.Parallel()
	// TriAbsent must NOT be countable: an absent attribute is routine, high-volume,
	// and counting it would bury the three outcomes that indicate a real bug.
	if TriAbsent.Countable() {
		t.Fatal("TriAbsent must not be countable")
	}
	for _, tr := range []Tri{TriBadType, TriBadValue, TriBadOp} {
		if !tr.Countable() {
			t.Fatalf("%s must be countable", tr)
		}
		if tr.Decided() {
			t.Fatalf("%s must not be decided", tr)
		}
	}
	if !TriTrue.Decided() || !TriFalse.Decided() {
		t.Fatal("true/false must be decided")
	}
}

func TestMatchRuleCombiners(t *testing.T) {
	t.Parallel()
	ctx := ctxWith(map[string]Value{"country": String("IN"), "plan": String("pro")})
	inIN := Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}}
	inUS := Condition{Attribute: "country", Op: OpEquals, Values: []Value{String("US")}}
	proPlan := Condition{Attribute: "plan", Op: OpEquals, Values: []Value{String("pro")}}

	tests := []struct {
		name string
		rule Rule
		want bool
	}{
		{"and all true", Rule{Conditions: []Condition{inIN, proPlan}, Combiner: LogicAnd}, true},
		{"and one false", Rule{Conditions: []Condition{inIN, inUS}, Combiner: LogicAnd}, false},
		{"or one true", Rule{Conditions: []Condition{inUS, proPlan}, Combiner: LogicOr}, true},
		{"or all false", Rule{Conditions: []Condition{inUS, {Attribute: "plan", Op: OpEquals, Values: []Value{String("free")}}}, Combiner: LogicOr}, false},

		// Vacuous truth. The config validator rejects empty condition lists, so this
		// is defence in depth: the behaviour is defined, documented and tested rather
		// than emergent. An AND rule with no conditions is a catch-all that matches
		// every subject, which is exactly why the validator refuses to ship one.
		{"AND over zero conditions is vacuously TRUE", Rule{Combiner: LogicAnd}, true},
		{"OR over zero conditions is FALSE", Rule{Combiner: LogicOr}, false},

		// The exists-guarded negation pattern the linter nudges authors toward:
		// "everyone except IN users, and only where we actually know the country".
		{"exists AND negated-eq is the correct way to write NOT IN",
			Rule{Conditions: []Condition{
				{Attribute: "country", Op: OpExists},
				{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}, Negate: true},
			}, Combiner: LogicAnd}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rule := tc.rule
			if got := MatchRule("f", &rule, ctx, nil); got != tc.want {
				t.Fatalf("MatchRule = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchRuleExistsGuardedNegationOnAbsentAttribute(t *testing.T) {
	t.Parallel()
	// Same rule as above, but the attribute is absent. The EXISTS guard makes the
	// author's intent explicit: "I do not know the country, so this rule does not
	// apply." Without the guard, the negated condition would ALSO be false -- which
	// is the safe reading either way. The guard exists to make that reading visible
	// in the config rather than implicit in the engine.
	rule := Rule{Conditions: []Condition{
		{Attribute: "country", Op: OpExists},
		{Attribute: "country", Op: OpEquals, Values: []Value{String("IN")}, Negate: true},
	}, Combiner: LogicAnd}
	if MatchRule("f", &rule, ctxWith(nil), nil) {
		t.Fatal("rule must not match when the guarded attribute is absent")
	}
	// And it does match for a country we know is not IN.
	if !MatchRule("f", &rule, ctxWith(map[string]Value{"country": String("US")}), nil) {
		t.Fatal("rule must match a known non-IN country")
	}
}

func TestMatchRuleNilIsSafe(t *testing.T) {
	t.Parallel()
	if MatchRule("f", nil, EvalContext{}, nil) {
		t.Fatal("nil rule must not match")
	}
}

func TestUndecidableConditionsAreObserved(t *testing.T) {
	t.Parallel()
	// A silent false is the leak; a counted false is a signal. Assert the counter
	// actually fires for each countable outcome.
	obs := &recordingObserver{}
	rule := Rule{
		ID:       "r-1",
		Combiner: LogicOr, // OR so every condition is consulted
		Conditions: []Condition{
			{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(1)}},                // wrong type
			{Attribute: "app_version", Op: OpSemverEqual, Values: []Value{String("1.0.0")}}, // bad value
			{Attribute: "country", Op: OpUnknown, Values: []Value{String("IN")}},            // bad op
			{Attribute: "missing", Op: OpEquals, Values: []Value{String("x")}},              // absent: NOT counted
		},
	}
	ctx := ctxWith(map[string]Value{
		"seats":       String("50"),
		"app_version": String("not-a-version"),
		"country":     String("IN"),
	})
	if MatchRule("flag-a", &rule, ctx, obs) {
		t.Fatal("rule of undecidable conditions must not match")
	}
	want := []observedCall{
		{"flag-a", "r-1", "seats", TriBadType},
		{"flag-a", "r-1", "app_version", TriBadValue},
		{"flag-a", "r-1", "country", TriBadOp},
	}
	if len(obs.calls) != len(want) {
		t.Fatalf("observed %d calls (%v), want %d", len(obs.calls), obs.calls, len(want))
	}
	for i := range want {
		if obs.calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, obs.calls[i], want[i])
		}
	}
}

func TestMatchRuleShortCircuits(t *testing.T) {
	t.Parallel()
	// AND stops at the first false, so the second condition's type mismatch is never
	// consulted and therefore never counted. Documented behaviour: the counter
	// measures conditions that were actually evaluated, and the hot path does not do
	// extra work to improve a metric.
	obs := &recordingObserver{}
	rule := Rule{
		ID:       "r-1",
		Combiner: LogicAnd,
		Conditions: []Condition{
			{Attribute: "country", Op: OpEquals, Values: []Value{String("US")}}, // false
			{Attribute: "seats", Op: OpGreaterThan, Values: []Value{Int(1)}},    // wrong type, unreached
		},
	}
	ctx := ctxWith(map[string]Value{"country": String("IN"), "seats": String("50")})
	if MatchRule("flag-a", &rule, ctx, obs) {
		t.Fatal("rule must not match")
	}
	if len(obs.calls) != 0 {
		t.Fatalf("short-circuited condition was observed: %+v", obs.calls)
	}
}

func TestMatchRuleNilAttributesMapDoesNotPanic(t *testing.T) {
	t.Parallel()
	rule := Rule{Conditions: []Condition{{Attribute: "anything", Op: OpEquals, Values: []Value{String("x")}}}}
	var ctx EvalContext // nil Attributes
	if MatchRule("f", &rule, ctx, nil) {
		t.Fatal("must not match against an empty context")
	}
}

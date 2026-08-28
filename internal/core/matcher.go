package core

import "strings"

// Tri is the outcome of one raw condition comparison, BEFORE Negate is applied.
//
// Boolean logic is not enough here. A condition can be true, false, or undecidable,
// and the three reasons a condition is undecidable are operationally distinct:
// a missing attribute is routine (an anonymous request has no user_id), a
// wrong-typed attribute is a caller data-quality bug, and an unparseable value is a
// producer bug. Collapsing all of them to "false" at the point of comparison would
// be correct behaviourally and invisible operationally. We collapse to false only
// after recording which kind of undecidable it was.
//
// See HLD C.3: three-valued logic collapsed at the condition boundary, with
// UNKNOWN -> FALSE applied AFTER the operator and BEFORE negation.
type Tri uint8

const (
	// TriFalse: decided, and the answer is no. Negate flips this.
	TriFalse Tri = iota

	// TriTrue: decided, and the answer is yes. Negate flips this.
	TriTrue

	// TriAbsent: the attribute is not in the evaluation context.
	//
	// THIS IS THE ONE THAT MATTERS. It collapses to false and Negate does NOT flip
	// it. `country != "IN"` with an absent country is FALSE, not true. Getting this
	// wrong means a failed upstream geo lookup silently targets every user on the
	// planet -- a single upstream degradation flipping a targeting rule for the
	// entire population. OpExists is the sole operator defined on absence and is
	// handled before this point.
	TriAbsent

	// TriBadType: the attribute is present but its type cannot be compared with this
	// operator or against this operand. No coercion is attempted anywhere: "1" is
	// not 1 and "true" is not true. Collapses to false, and is COUNTED.
	TriBadType

	// TriBadValue: the attribute is present and correctly typed, but the value is
	// malformed for the operator -- in practice, a string that is not a valid semver.
	// Collapses to false, and is COUNTED.
	TriBadValue

	// TriBadOp: the condition names an operator this build does not implement, or
	// OpUnknown. The config validator rejects these, so reaching here means the
	// validator has a hole. Collapses to false, and is COUNTED so the hole is
	// visible rather than silently making a rule unmatchable.
	TriBadOp
)

func (t Tri) String() string {
	switch t {
	case TriFalse:
		return "false"
	case TriTrue:
		return "true"
	case TriAbsent:
		return "absent"
	case TriBadType:
		return "bad_type"
	case TriBadValue:
		return "bad_value"
	case TriBadOp:
		return "bad_op"
	default:
		return "invalid"
	}
}

// Decided reports whether the outcome carries an answer that Negate may flip.
func (t Tri) Decided() bool { return t == TriTrue || t == TriFalse }

// Countable reports whether the outcome represents a defect worth a counter.
//
// TriAbsent is deliberately excluded: an absent attribute is a routine, high-volume
// condition on any public endpoint, and counting it would bury the three outcomes
// that actually indicate a bug.
func (t Tri) Countable() bool { return t == TriBadType || t == TriBadValue || t == TriBadOp }

// ConditionObserver is the observability hook for undecidable conditions.
//
// It exists because "present but wrong type -> false" is a silent failure by
// construction: the rule simply does not match, the flag returns its default, and
// nothing anywhere says why. A silent false is the leak; a counted false is a
// signal. The evaluator calls this for every Countable outcome.
//
// Implementations MUST NOT block, allocate unboundedly, panic, or perform I/O on
// the calling goroutine -- they run inside the evaluation hot path. A panic here is
// caught by the evaluator's recover boundary and degrades the whole evaluation to
// the caller default, which is a far worse outcome than the mismatch it was
// reporting. Increment a counter and return.
type ConditionObserver interface {
	ObserveUndecidable(flagKey, ruleID, attribute string, outcome Tri)
}

// ConditionObserverFunc adapts a function to ConditionObserver.
type ConditionObserverFunc func(flagKey, ruleID, attribute string, outcome Tri)

func (f ConditionObserverFunc) ObserveUndecidable(flagKey, ruleID, attribute string, outcome Tri) {
	f(flagKey, ruleID, attribute, outcome)
}

// MatchCondition evaluates one condition against a context.
//
// It returns the post-negation match plus the pre-negation raw outcome, so a caller
// can tell "did not match" apart from "could not be decided" without re-running the
// comparison.
//
// The ordering here is the whole point of the function:
//
//	decide -> collapse UNKNOWN to false -> apply Negate only to a decided answer
//
// Negation is applied to the match, never to the undecidable.
func MatchCondition(c *Condition, ctx EvalContext) (matched bool, outcome Tri) {
	if c == nil {
		return false, TriBadOp
	}
	outcome = decideCondition(c, ctx)
	switch outcome {
	case TriTrue:
		return !c.Negate, outcome
	case TriFalse:
		return c.Negate, outcome
	default:
		// Undecidable. False regardless of Negate.
		return false, outcome
	}
}

// decideCondition performs the raw comparison. It never applies Negate.
func decideCondition(c *Condition, ctx EvalContext) Tri {
	v, present := ctx.Attribute(c.Attribute)

	// OpExists is the sole operator whose semantics are defined on absence, so it is
	// answered before the absent-attribute collapse. It returns a DECIDED outcome
	// either way, which is exactly why `NOT EXISTS` works and `NOT EQUALS` does not:
	// negation is legal on a decided answer.
	if c.Op == OpExists {
		if present {
			return TriTrue
		}
		return TriFalse
	}

	if !present {
		return TriAbsent
	}

	switch c.Op {
	case OpEquals:
		return matchEquals(v, c.Values)
	case OpIn:
		return matchIn(v, c.Values)
	case OpContains:
		return matchContains(v, c.Values)
	case OpGreaterThan, OpGreaterOrEqual, OpLessThan, OpLessOrEqual:
		return matchOrdered(c.Op, v, c.Values)
	case OpSemverEqual, OpSemverGreaterThan, OpSemverLessThan:
		return matchSemver(c.Op, v, c.Values)
	default:
		return TriBadOp
	}
}

// operand returns Values[0], the single-operand form used by eq, contains, the
// ordered comparisons and the semver comparisons.
//
// An EMPTY Values slice means the condition has no operand to compare against. That
// is a config defect the validator rejects; here it is TriBadOp -- decidedly not
// true, and counted, because a condition with nothing on the right-hand side that
// silently evaluates false is a rule that can never fire and never explains itself.
func operand(vals []Value) (Value, bool) {
	if len(vals) == 0 {
		return Value{}, false
	}
	return vals[0], true
}

func matchEquals(v Value, vals []Value) Tri {
	want, ok := operand(vals)
	if !ok {
		return TriBadOp
	}
	// The operand's type declares what the attribute is expected to be. A type
	// disagreement is not "not equal", it is "not comparable" -- and it must be
	// counted, because String("1") vs Int(1) is a config/producer disagreement that
	// will otherwise sit there making a rule permanently unmatchable.
	if v.Type() != want.Type() {
		return TriBadType
	}
	return triOf(v.Equal(want))
}

func matchIn(v Value, vals []Value) Tri {
	// An empty set genuinely contains nothing. Unlike eq, `in []` has a defensible
	// reading, so it is a decided false rather than a defect.
	if len(vals) == 0 {
		return TriFalse
	}
	sawComparable := false
	for i := range vals {
		if vals[i].Type() != v.Type() {
			continue
		}
		sawComparable = true
		if v.Equal(vals[i]) {
			return TriTrue
		}
	}
	if !sawComparable {
		// Not one member of the set is even the same type as the attribute. That is a
		// mismatch, not a miss.
		return TriBadType
	}
	return TriFalse
}

func matchContains(v Value, vals []Value) Tri {
	want, ok := operand(vals)
	if !ok {
		return TriBadOp
	}
	s, sok := v.AsString()
	sub, subok := want.AsString()
	if !sok || !subok {
		// `contains` on an int or a bool is meaningless. It is not false-by-value, it
		// is false-by-confusion, so it is counted.
		return TriBadType
	}
	return triOf(strings.Contains(s, sub))
}

// matchOrdered implements gt / gte / lt / lte.
//
// TypeInt ONLY. `gt` on a string is a config error, not a lexicographic comparison:
// silently ordering "10" below "9" is worse than not answering, and a lexicographic
// `gt` in a targeting rule is never what the author meant -- if it were, they would
// have written a version comparison, which is what the semver operators are for.
// Booleans have no useful order either. Both are TriBadType, so the config defect
// is counted rather than quietly producing plausible-looking nonsense.
func matchOrdered(op Operator, v Value, vals []Value) Tri {
	want, ok := operand(vals)
	if !ok {
		return TriBadOp
	}
	a, aok := v.AsInt()
	b, bok := want.AsInt()
	if !aok || !bok {
		return TriBadType
	}
	switch op {
	case OpGreaterThan:
		return triOf(a > b)
	case OpGreaterOrEqual:
		return triOf(a >= b)
	case OpLessThan:
		return triOf(a < b)
	case OpLessOrEqual:
		return triOf(a <= b)
	default:
		return TriBadOp
	}
}

// matchSemver implements semver_eq / semver_gt / semver_lt.
//
// Both sides must be strings (TriBadType otherwise). A string that does not parse as
// a semver is TriBadValue: false, counted, never an error. "2.14" is not rejected as
// a version at eval time -- it simply does not compare. Build metadata is ignored,
// so 1.0.0+a and 1.0.0+b are equal.
func matchSemver(op Operator, v Value, vals []Value) Tri {
	want, ok := operand(vals)
	if !ok {
		return TriBadOp
	}
	as, aok := v.AsString()
	bs, bok := want.AsString()
	if !aok || !bok {
		return TriBadType
	}
	av, aparsed := parseSemver(as)
	bv, bparsed := parseSemver(bs)
	if !aparsed || !bparsed {
		return TriBadValue
	}
	c := av.compare(bv)
	switch op {
	case OpSemverEqual:
		return triOf(c == 0)
	case OpSemverGreaterThan:
		return triOf(c > 0)
	case OpSemverLessThan:
		return triOf(c < 0)
	default:
		return TriBadOp
	}
}

func triOf(b bool) Tri {
	if b {
		return TriTrue
	}
	return TriFalse
}

// MatchRule reports whether a rule's conditions are satisfied.
//
// Combiner semantics on an EMPTY condition slice:
//
//   - LogicAnd over zero conditions is TRUE. That is vacuous truth, and it is the
//     mathematically correct reading of "all of nothing".
//   - LogicOr over zero conditions is FALSE, symmetrically: "any of nothing".
//
// A rule with no conditions and AND therefore matches EVERY subject, which is a
// catch-all. That is a real thing an author might want and a catastrophic thing to
// write by accident, so the config validator rejects empty rule condition lists
// outright. What is implemented here is defence in depth for the case where the
// validator has a hole: it is defined, documented and tested rather than emergent.
//
// Short-circuit note: AND stops at the first false and OR at the first true, so a
// later condition's undecidable outcome is not observed. That is deliberate -- the
// hot path is not going to evaluate conditions it does not need in order to improve
// a counter. The counter measures conditions that were actually consulted.
func MatchRule(flagKey string, r *Rule, ctx EvalContext, obs ConditionObserver) bool {
	if r == nil {
		return false
	}
	if r.Combiner == LogicOr {
		for i := range r.Conditions {
			matched, outcome := MatchCondition(&r.Conditions[i], ctx)
			observe(obs, flagKey, r.ID, r.Conditions[i].Attribute, outcome)
			if matched {
				return true
			}
		}
		return false
	}
	for i := range r.Conditions {
		matched, outcome := MatchCondition(&r.Conditions[i], ctx)
		observe(obs, flagKey, r.ID, r.Conditions[i].Attribute, outcome)
		if !matched {
			return false
		}
	}
	return true
}

func observe(obs ConditionObserver, flagKey, ruleID, attribute string, outcome Tri) {
	if obs == nil || !outcome.Countable() {
		return
	}
	obs.ObserveUndecidable(flagKey, ruleID, attribute, outcome)
}

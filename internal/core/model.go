package core

// Operator is a targeting condition's comparison.
//
// The set is deliberately small. Every operator here is one a reviewer can reason
// about in constant time and a lint rule can type-check against the attribute's
// declared type. REGEX is absent on purpose -- see ADR-0004. Go's RE2 removes the
// usual ReDoS objection, but the remaining ones stand: compile lifecycle poisons
// snapshot builds, match cost is linear in an attacker-influenced attribute length
// (~100 microseconds on a 64KB attribute, 30x the whole evaluation budget), and a
// regex in a targeting rule is unreviewable in a config diff.
type Operator uint8

const (
	OpUnknown Operator = iota
	OpEquals
	OpIn
	OpContains // substring; lint warns, see ADR-0004
	OpGreaterThan
	OpGreaterOrEqual
	OpLessThan
	OpLessOrEqual
	OpExists
	OpSemverEqual
	OpSemverGreaterThan
	OpSemverLessThan
)

func (o Operator) String() string {
	switch o {
	case OpEquals:
		return "eq"
	case OpIn:
		return "in"
	case OpContains:
		return "contains"
	case OpGreaterThan:
		return "gt"
	case OpGreaterOrEqual:
		return "gte"
	case OpLessThan:
		return "lt"
	case OpLessOrEqual:
		return "lte"
	case OpExists:
		return "exists"
	case OpSemverEqual:
		return "semver_eq"
	case OpSemverGreaterThan:
		return "semver_gt"
	case OpSemverLessThan:
		return "semver_lt"
	default:
		return "unknown"
	}
}

// ParseOperator maps the wire representation to an Operator.
func ParseOperator(s string) (Operator, bool) {
	switch s {
	case "eq":
		return OpEquals, true
	case "in":
		return OpIn, true
	case "contains":
		return OpContains, true
	case "gt":
		return OpGreaterThan, true
	case "gte":
		return OpGreaterOrEqual, true
	case "lt":
		return OpLessThan, true
	case "lte":
		return OpLessOrEqual, true
	case "exists":
		return OpExists, true
	case "semver_eq":
		return OpSemverEqual, true
	case "semver_gt":
		return OpSemverGreaterThan, true
	case "semver_lt":
		return OpSemverLessThan, true
	default:
		return OpUnknown, false
	}
}

// LogicOp combines the conditions within a single rule.
type LogicOp uint8

const (
	LogicAnd LogicOp = iota // zero value: AND is the safer default
	LogicOr
)

func (l LogicOp) String() string {
	if l == LogicOr {
		return "or"
	}
	return "and"
}

// Condition is one attribute comparison inside a rule.
//
// Negation is a flag on the condition rather than a family of NOT_* operators, so
// that the absent-attribute rule is written exactly once and cannot drift between
// an operator and its negated twin. See ADR-0003.
type Condition struct {
	Attribute string   `json:"attribute"`
	Op        Operator `json:"op"`
	Values    []Value  `json:"values"`
	Negate    bool     `json:"negate,omitempty"`
}

// Rule is an ordered targeting rule. Rules are evaluated in slice order and the
// first match wins, so the ORDER OF THIS SLICE IS THE SEMANTICS. That is why an
// overlay may only `replace` or `append` the rule list and may never deep-merge
// or key-merge it -- see ADR-0002.
type Rule struct {
	ID         string      `json:"id"`
	Conditions []Condition `json:"conditions"`
	Combiner   LogicOp     `json:"combiner"`
	Value      Value       `json:"value"`
}

// Rollout is a percentage rollout with sticky bucketing.
type Rollout struct {
	// BasisPoints is the rollout size in hundredths of a percent, 0..10000.
	//
	// Basis points rather than whole percent so that fractional rollouts such as
	// 0.5% are expressible without a later breaking change to the bucketing maths.
	// Changing the bucket space after launch re-buckets every user.
	BasisPoints int32 `json:"basis_points"`

	// BucketNamespace is the hash salt. This is decision O1.
	//
	// Empty means "use the flag key", which makes every flag bucket independently.
	// Setting the same literal on two flags makes them share a bucket space
	// deliberately, which is the brief's opt-in sharing requirement.
	//
	// This value is IMMUTABLE once a rollout has run in production. Changing it
	// silently re-buckets every user -- at a 10% rollout roughly 18% of users flip
	// while every dashboard stays green. Guarded by the bucketing_scheme_hash gauge
	// and the golden-vector build gate.
	BucketNamespace string `json:"bucket_namespace,omitempty"`

	// BucketBy names the context attribute used as the bucketing subject.
	// Empty means user_id.
	BucketBy string `json:"bucket_by,omitempty"`

	OnValue  Value `json:"on_value"`
	OffValue Value `json:"off_value"`
}

// EvaluationOrder is decision O2, written explicitly into the schema rather than
// left implicit.
//
// OrderRulesFirst and a rollout-gates-first ordering would accept byte-identical
// config and mean different things, so a flag carrying both rules and a rollout
// with no explicit order is REJECTED at config time rather than defaulted. Shipping
// a default here would decide O2 by accident and make any later change a silent
// behavioural migration across every flag.
type EvaluationOrder uint8

const (
	// OrderUnspecified is the zero value. Legal only when the flag does not have
	// both rules and a rollout.
	OrderUnspecified EvaluationOrder = iota

	// OrderRulesFirst: rules are evaluated in order and the first match returns
	// immediately; the rollout runs only for subjects that fell through every rule.
	// This is the chosen ordering.
	OrderRulesFirst
)

func (e EvaluationOrder) String() string {
	if e == OrderRulesFirst {
		return "rules_first"
	}
	return "unspecified"
}

// Flag is a fully resolved flag: the output of merging the base layer, the
// environment overlay and any ops override, then compiling the result.
//
// A Flag inside a Snapshot is immutable. Nothing may mutate it after the snapshot
// pointer is published (invariant CACHE-2).
type Flag struct {
	Key     string    `json:"key"`
	Type    ValueType `json:"type"`
	Enabled bool      `json:"enabled"`

	// DefaultValue is the flag's own fallthrough value: what it resolves to when
	// no rule matches and no rollout is configured. Distinct from the caller's
	// default, which is the terminal fallback used only when evaluation cannot
	// produce a configured value at all.
	DefaultValue Value `json:"default_value"`

	// OffValue is returned when Enabled is false.
	OffValue Value `json:"off_value"`

	Rules           []Rule          `json:"rules,omitempty"`
	Rollout         *Rollout        `json:"rollout,omitempty"`
	EvaluationOrder EvaluationOrder `json:"evaluation_order,omitempty"`
}

// HasRollout reports whether a percentage rollout is configured.
func (f *Flag) HasRollout() bool { return f.Rollout != nil && f.Rollout.BasisPoints > 0 }

package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// Severity is a finding's blast radius.
//
// The whole point of having four levels rather than "error" is that one bad
// overlay out of five thousand flags must not block an unrelated urgent change,
// while a systematically broken input must not be half-applied.
type Severity uint8

const (
	// SeverityWarn publishes. It emits a structured warning and a metric.
	SeverityWarn Severity = iota
	// SeverityRejectFlag quarantines one flag: it carries forward its previous
	// resolved version, or is absent if it has none. The environment publishes.
	SeverityRejectFlag
	// SeverityRejectEnv keeps that environment on its last-known-good snapshot.
	// Other environments publish normally.
	SeverityRejectEnv
	// SeverityRejectGlobal publishes nothing anywhere. Reserved for the base
	// layer, which is the only layer shared by every environment and is therefore
	// the only global blast radius in the system.
	SeverityRejectGlobal
)

func (s Severity) String() string {
	switch s {
	case SeverityRejectFlag:
		return "reject-flag"
	case SeverityRejectEnv:
		return "reject-env"
	case SeverityRejectGlobal:
		return "reject-global"
	default:
		return "warn"
	}
}

// IsRejection reports whether this severity stops something from publishing.
func (s Severity) IsRejection() bool { return s != SeverityWarn }

// Finding is one validation violation.
//
// Every finding names the rule, the flag, the layer that contributed the
// offending field, and the dotted field path -- enough for an operator to fix the
// config without reading the merge code.
type Finding struct {
	RuleID   string   `json:"rule_id"`
	Flag     string   `json:"flag,omitempty"`
	Env      string   `json:"env,omitempty"`
	Layer    LayerID  `json:"layer"`
	Field    string   `json:"field,omitempty"`
	Message  string   `json:"message"`
	Severity Severity `json:"severity"`
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(f.RuleID)
	b.WriteString(" [")
	b.WriteString(f.Severity.String())
	b.WriteString("] ")
	b.WriteString(f.Layer.String())
	if f.Env != "" {
		b.WriteString(" env=")
		b.WriteString(f.Env)
	}
	if f.Flag != "" {
		b.WriteString(" flag=")
		b.WriteString(f.Flag)
	}
	if f.Field != "" {
		b.WriteString(" field=")
		b.WriteString(f.Field)
	}
	b.WriteString(": ")
	b.WriteString(f.Message)
	return b.String()
}

// Findings is the aggregated, structured error type.
//
// Validation never stops at the first violation. An operator fixing a config file
// one error per round trip is a bad experience and, worse, encourages fixing the
// first error and re-pushing rather than reading the whole file.
type Findings []Finding

// Error implements error, so Findings can be returned where an error is expected.
func (fs Findings) Error() string {
	if len(fs) == 0 {
		return "config: no findings"
	}
	lines := make([]string, 0, len(fs)+1)
	lines = append(lines, fmt.Sprintf("config: %d validation finding(s)", len(fs)))
	for _, f := range fs {
		lines = append(lines, "  "+f.String())
	}
	return strings.Join(lines, "\n")
}

// Err returns fs as an error when it contains at least one rejection, and nil
// otherwise. Returning fs directly would put a non-nil interface holding an empty
// slice into an `err != nil` check.
func (fs Findings) Err() error {
	if !fs.HasRejections() {
		return nil
	}
	return fs
}

// HasRejections reports whether any finding stops something from publishing.
func (fs Findings) HasRejections() bool {
	for _, f := range fs {
		if f.Severity.IsRejection() {
			return true
		}
	}
	return false
}

// MaxSeverity returns the highest severity present.
func (fs Findings) MaxSeverity() Severity {
	var max Severity
	for _, f := range fs {
		if f.Severity > max {
			max = f.Severity
		}
	}
	return max
}

// Rejections returns only the findings that block publication.
func (fs Findings) Rejections() Findings {
	return fs.filter(func(f Finding) bool { return f.Severity.IsRejection() })
}

// Warns returns only the warnings.
func (fs Findings) Warns() Findings {
	return fs.filter(func(f Finding) bool { return f.Severity == SeverityWarn })
}

// ForFlag returns the findings attached to one flag key.
func (fs Findings) ForFlag(key string) Findings {
	return fs.filter(func(f Finding) bool { return f.Flag == key })
}

// Has reports whether a finding with the given rule id is present.
func (fs Findings) Has(ruleID string) bool {
	for _, f := range fs {
		if f.RuleID == ruleID {
			return true
		}
	}
	return false
}

// RuleIDs lists the rule ids present, sorted and deduplicated.
func (fs Findings) RuleIDs() []string {
	seen := make(map[string]struct{}, len(fs))
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		if _, ok := seen[f.RuleID]; ok {
			continue
		}
		seen[f.RuleID] = struct{}{}
		out = append(out, f.RuleID)
	}
	sort.Strings(out)
	return out
}

func (fs Findings) filter(keep func(Finding) bool) Findings {
	var out Findings
	for _, f := range fs {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Limits
// -----------------------------------------------------------------------------

// QuarantineFloor and QuarantineFraction define the safety valve on per-flag
// quarantine.
//
// Quarantining a bad flag is right for a typo: the flag keeps serving its
// last-known-good value and the other 4,999 flags publish. It is WRONG for a
// systematically broken input -- a schema change, a bad codegen run, a truncated
// file -- because partially applying a systematically broken config is how you get
// a half-configured production that nobody authored. Past
// max(QuarantineFloor, QuarantineFraction * flags) the build escalates to
// rejecting the whole environment, which keeps the last-known-good snapshot whole.
const (
	QuarantineFloor    = 20
	QuarantineFraction = 0.05
)

// MaxFlagsPerEnv is a memory guard: beyond this a build is rejected for the
// environment rather than allowed to blow the snapshot memory budget.
const MaxFlagsPerEnv = 20000

// QuarantineBudget returns the number of quarantined flags an environment may
// carry before the build escalates to a whole-environment rejection.
func QuarantineBudget(totalFlags int) int {
	budget := int(float64(totalFlags) * QuarantineFraction)
	if budget < QuarantineFloor {
		budget = QuarantineFloor
	}
	return budget
}

var flagKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func sortStrings(s []string) { sort.Strings(s) }

// -----------------------------------------------------------------------------
// PASS 1 -- pre-merge. Checkable against one layer, alone.
// -----------------------------------------------------------------------------

// ValidateBase checks the base layer against everything decidable without an
// overlay. Base findings are reject-global: the base is the one layer shared by
// every environment, so a malformed base publishes nothing anywhere and every
// environment keeps its last-known-good snapshot.
func ValidateBase(l *BaseLayer) Findings {
	var out Findings
	if l == nil {
		return out
	}
	const sev = SeverityRejectGlobal
	seen := make(map[string]struct{}, len(l.Flags))
	for i := range l.Flags {
		f := &l.Flags[i]
		add := func(id, field, msg string, s Severity) {
			out = append(out, Finding{RuleID: id, Flag: f.Key, Layer: LayerBase, Field: field, Message: msg, Severity: s})
		}
		for _, m := range f.missing {
			add("B00", m, "required base field is absent; the base layer is a total record", sev)
		}
		if !flagKeyRE.MatchString(f.Key) {
			add("B01", FieldKey, fmt.Sprintf("flag key %q does not match %s", f.Key, flagKeyRE.String()), sev)
		}
		if _, dup := seen[f.Key]; dup {
			add("B12", FieldKey, fmt.Sprintf("duplicate flag key %q in the base layer", f.Key), sev)
		}
		seen[f.Key] = struct{}{}

		typ, okType := core.ParseValueType(f.Type)
		if !okType {
			add("B02", FieldType, fmt.Sprintf("unknown value type %q (want bool, string or int)", f.Type), sev)
		}
		if okType {
			if f.DefaultValue.Type() != typ {
				add("B03", FieldDefaultValue, fmt.Sprintf("default_value is %s, flag is declared %s", f.DefaultValue.Type(), typ), sev)
			}
			if !f.OffValue.IsUnknown() && f.OffValue.Type() != typ {
				add("B04", FieldOffValue, fmt.Sprintf("off_value is %s, flag is declared %s", f.OffValue.Type(), typ), sev)
			}
		}
		if _, ok := parseEvaluationOrder(f.EvaluationOrder); !ok {
			add("B18", FieldEvaluationOrder, fmt.Sprintf("unknown evaluation_order %q", f.EvaluationOrder), sev)
		}
		if f.Owner == "" {
			add("B13", FieldOwner, "owner is not set; an unowned flag has nobody to page", SeverityWarn)
		}
		out = append(out, validateRuleList(l.LayerEnv(), LayerBase, f.Key, FieldRules, f.Rules, sev)...)
		if f.Rollout != nil {
			if f.Rollout.BasisPoints < 0 || f.Rollout.BasisPoints > core.BucketSpace {
				add("B08", FieldRolloutBasisPoints, fmt.Sprintf("basis_points %d outside 0..%d", f.Rollout.BasisPoints, core.BucketSpace), sev)
			}
		}
	}
	return out
}

// ValidateOverlay checks an environment overlay against everything decidable
// without the base. Findings are reject-flag: one bad flag is quarantined, the
// rest of the environment publishes.
func ValidateOverlay(l *OverlayLayer) Findings {
	var out Findings
	if l == nil {
		return out
	}
	env := l.Environment
	const sev = SeverityRejectFlag
	if env == "" {
		out = append(out, Finding{RuleID: "O00", Layer: LayerOverlay, Field: "environment",
			Message: "overlay layer does not name an environment", Severity: SeverityRejectEnv})
	}
	seen := make(map[string]struct{}, len(l.Flags))
	for i := range l.Flags {
		f := &l.Flags[i]
		add := func(id, field, msg string, s Severity) {
			out = append(out, Finding{RuleID: id, Flag: f.Key, Env: env, Layer: LayerOverlay, Field: field, Message: msg, Severity: s})
		}
		if !flagKeyRE.MatchString(f.Key) {
			add("O01", FieldKey, fmt.Sprintf("flag key %q does not match %s", f.Key, flagKeyRE.String()), sev)
		}
		if _, dup := seen[f.Key]; dup {
			add("O11", FieldKey, fmt.Sprintf("duplicate flag key %q in the overlay", f.Key), sev)
		}
		seen[f.Key] = struct{}{}

		if f.Type.Set {
			// Rejected even when it MATCHES the base. Allowing a matching
			// restatement is what invites a future non-matching one.
			add("O02", FieldType, "type is base-only and immutable; an overlay may never restate it", sev)
		}

		// O04 -- explicit null on a non-nullable scalar. For a scalar in a strict
		// precedence chain, null and absent are semantically identical, so a null
		// is always author confusion and is rejected rather than silently accepted.
		for _, s := range []struct {
			field string
			null  bool
		}{
			{FieldEnabled, f.Enabled.IsNull()},
			{FieldDefaultValue, f.DefaultValue.IsNull()},
			{FieldOffValue, f.OffValue.IsNull()},
			{FieldEvaluationOrder, f.EvaluationOrder.IsNull()},
			{FieldRulesMode, f.RulesMode.IsNull()},
		} {
			if s.null {
				add("O04", s.field, "explicit null on a non-nullable scalar; omit the key to inherit", sev)
			}
		}

		// O03 -- the rule-list operator must be declared, exactly once. Order is
		// first-match-wins semantics, so the operation on the list is not
		// something to infer.
		if f.Rules.Set {
			mode, hasMode := f.RulesMode.Get()
			switch {
			case !hasMode:
				add("O03", FieldRulesMode, "rules present without rules_mode; declare \"replace\" or \"append\"", sev)
			case mode != RuleModeReplace && mode != RuleModeAppend:
				add("O03", FieldRulesMode, fmt.Sprintf("unknown rules_mode %q; want \"replace\" or \"append\"", mode), sev)
			case f.Rules.Null && mode == RuleModeAppend:
				add("O03", FieldRules, "rules: null with rules_mode append is meaningless; use replace to clear the list", sev)
			}
			if f.Rules.IsValue() {
				out = append(out, validateRuleList(env, LayerOverlay, f.Key, FieldRules, f.Rules.Val, sev)...)
			}
		} else if f.RulesMode.Set {
			add("O03", FieldRulesMode, "rules_mode declared without rules", sev)
		}

		if v, ok := f.EvaluationOrder.Get(); ok {
			if _, ok := parseEvaluationOrder(v); !ok {
				add("O12", FieldEvaluationOrder, fmt.Sprintf("unknown evaluation_order %q", v), sev)
			}
		}

		if r, ok := f.Rollout.Get(); ok {
			for _, s := range []struct {
				field string
				null  bool
			}{
				{FieldRolloutBasisPoints, r.BasisPoints.IsNull()},
				{FieldRolloutBucketNamespace, r.BucketNamespace.IsNull()},
				{FieldRolloutBucketBy, r.BucketBy.IsNull()},
				{FieldRolloutOnValue, r.OnValue.IsNull()},
				{FieldRolloutOffValue, r.OffValue.IsNull()},
			} {
				if s.null {
					add("O04", s.field, "explicit null on a non-nullable scalar; omit the key to inherit", sev)
				}
			}
			if bp, ok := r.BasisPoints.Get(); ok && (bp < 0 || bp > core.BucketSpace) {
				add("O05", FieldRolloutBasisPoints, fmt.Sprintf("basis_points %d outside 0..%d", bp, core.BucketSpace), sev)
			}
		}
	}
	return out
}

// ValidateOps checks an ops override layer. now is the build clock; TTL rules are
// evaluated against it on every build, which is what makes an override self-heal.
func ValidateOps(l *OpsLayer, now time.Time) Findings {
	var out Findings
	if l == nil {
		return out
	}
	env := l.Environment
	const sev = SeverityRejectFlag
	if env == "" {
		out = append(out, Finding{RuleID: "O00", Layer: LayerOps, Field: "environment",
			Message: "ops layer does not name an environment", Severity: SeverityRejectEnv})
	}
	seen := make(map[string]struct{}, len(l.Overrides))
	for i := range l.Overrides {
		o := &l.Overrides[i]
		add := func(id, field, msg string, s Severity) {
			out = append(out, Finding{RuleID: id, Flag: o.Key, Env: env, Layer: LayerOps, Field: field, Message: msg, Severity: s})
		}
		if !flagKeyRE.MatchString(o.Key) {
			add("O01", FieldKey, fmt.Sprintf("flag key %q does not match %s", o.Key, flagKeyRE.String()), sev)
		}
		if _, dup := seen[o.Key]; dup {
			add("O14", FieldKey, fmt.Sprintf("duplicate flag key %q in the ops layer", o.Key), sev)
		}
		seen[o.Key] = struct{}{}

		// O07 -- the whitelist. An unbounded emergency layer is a second config
		// system with none of the review of the first one.
		for _, k := range o.disallowed {
			add("O07", k, fmt.Sprintf("field %q is outside the L3 whitelist %v", k, sortedKeys(opsAllowedFields)), sev)
		}

		// O08 -- a kill switch with no expiry becomes permanent config.
		exp, hasExp := o.ExpiresAt.Get()
		if !hasExp {
			add("O08", FieldExpiresAt, "ops override requires expires_at; a kill switch with no TTL becomes permanent config", sev)
		}
		if o.Reason == "" {
			add("O08", FieldReason, "ops override requires reason", sev)
		}
		if o.Owner == "" {
			add("O08", FieldOwner, "ops override requires owner", sev)
		}
		if hasExp {
			ttl := exp.Sub(now)
			switch {
			case ttl > MaxOpsOverrideTTL:
				add("O09", FieldExpiresAt, fmt.Sprintf("expires_at is %s out, over the %s cap", ttl.Round(time.Hour), MaxOpsOverrideTTL), sev)
			case ttl > OpsOverrideTTLWarn:
				add("O10", FieldExpiresAt, fmt.Sprintf("expires_at is %s out; over %s an override is config, not an incident tool", ttl.Round(time.Hour), OpsOverrideTTLWarn), SeverityWarn)
			case ttl <= 0:
				// M11 -- self-healing: an expired override is dropped, not an error.
				add("M11", FieldExpiresAt, "ops override has expired and is being dropped from the merge", SeverityWarn)
			}
		}
		if bp, ok := o.BasisPoints.Get(); ok && (bp < 0 || bp > core.BucketSpace) {
			add("O05", FieldRolloutBasisPoints, fmt.Sprintf("basis_points %d outside 0..%d", bp, core.BucketSpace), sev)
		}
		if !o.Enabled.IsValue() && !o.BasisPoints.IsValue() {
			add("O15", "", "ops override changes nothing; it sets neither enabled nor basis_points", SeverityWarn)
		}
	}
	return out
}

// opsExpired reports whether an override has passed its TTL at build time.
func opsExpired(o *OpsOverride, now time.Time) bool {
	exp, ok := o.ExpiresAt.Get()
	if !ok {
		return false // missing TTL is a rejection, not an expiry
	}
	return !exp.After(now)
}

// validateRuleList runs the checks that hold for any rule list, in any layer.
func validateRuleList(env string, layer LayerID, flagKey, field string, rules []WireRule, sev Severity) Findings {
	var out Findings
	seen := make(map[string]struct{}, len(rules))
	for i, r := range rules {
		path := fmt.Sprintf("%s[%d]", field, i)
		add := func(id, f, msg string) {
			out = append(out, Finding{RuleID: id, Flag: flagKey, Env: env, Layer: layer, Field: f, Message: msg, Severity: sev})
		}
		if r.ID == "" {
			add("B05", path+".id", "rule id is empty; the id is the observability key on every matched evaluation")
		} else if _, dup := seen[r.ID]; dup {
			add("B05", path+".id", fmt.Sprintf("duplicate rule id %q within the list", r.ID))
		}
		seen[r.ID] = struct{}{}

		if _, ok := parseLogicOp(r.Combiner); !ok {
			add("B17", path+".combiner", fmt.Sprintf("unknown combiner %q; want \"and\" or \"or\"", r.Combiner))
		}
		if len(r.Conditions) == 0 {
			// An empty rule matches everything, which under first-match-wins
			// silently shadows every rule after it.
			add("B07", path+".conditions", "rule has no conditions; an empty rule matches everything and shadows every rule after it")
		}
		for j, c := range r.Conditions {
			cpath := fmt.Sprintf("%s.conditions[%d]", path, j)
			if c.Attribute == "" {
				add("B07", cpath+".attribute", "condition names no attribute")
			}
			op, ok := core.ParseOperator(c.Op)
			if !ok {
				add("B06", cpath+".op", fmt.Sprintf("unknown operator %q", c.Op))
				continue
			}
			switch op {
			case core.OpExists:
				if len(c.Values) != 0 {
					add("B07", cpath+".values", "operator exists takes no values")
				}
			case core.OpIn:
				if len(c.Values) == 0 {
					add("B07", cpath+".values", "operator in requires at least one value")
				}
			default:
				if len(c.Values) != 1 {
					add("B07", cpath+".values", fmt.Sprintf("operator %s requires exactly one value, got %d", op, len(c.Values)))
				}
			}
			for k, v := range c.Values {
				if v.IsUnknown() {
					add("B07", fmt.Sprintf("%s.values[%d]", cpath, k), "condition value is null; a null can never compare equal to anything")
				}
			}
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// -----------------------------------------------------------------------------
// PASS 2 -- post-merge. Decidable only once the layers are combined.
// -----------------------------------------------------------------------------

// validateResolved checks a merged flag. These are the rules that make eager
// resolution mandatory: a base layer that is valid alone can merge into an
// invalid resolved flag, and discovering that on the hot path inside an
// evaluation is exactly what the never-throw contract forbids.
func validateResolved(env string, f *core.Flag, base *BaseFlag, ov *OverlayFlag, ops *OpsOverride, prov FlagProvenance) Findings {
	var out Findings
	const sev = SeverityRejectFlag
	add := func(id, field, msg string, s Severity) {
		out = append(out, Finding{RuleID: id, Flag: f.Key, Env: env, Layer: prov.Layer(field), Field: field, Message: msg, Severity: s})
	}

	if f.Type == core.TypeUnknown {
		add("M20", FieldType, "resolved flag has no declared type", sev)
		return out
	}

	// M01 -- the overlay does not carry a type; only the base knows it, so a
	// type mismatch between an overlay value and the flag is a post-merge fact.
	if f.DefaultValue.Type() != f.Type {
		add("M01", FieldDefaultValue, fmt.Sprintf("resolved default_value is %s, flag is %s", f.DefaultValue.Type(), f.Type), sev)
	}
	if f.OffValue.Type() != f.Type {
		add("M01", FieldOffValue, fmt.Sprintf("resolved off_value is %s, flag is %s", f.OffValue.Type(), f.Type), sev)
	}

	// M03 / M04 -- rule id uniqueness needs the resolved list, which only exists
	// after the base list and the appended list have been concatenated.
	baseIDs := make(map[string]struct{}, len(base.Rules))
	for _, r := range base.Rules {
		baseIDs[r.ID] = struct{}{}
	}
	appended := ov != nil && ov.Rules.IsValue() && ov.RulesMode.OrElse(RuleModeReplace) == RuleModeAppend
	if appended {
		for _, r := range ov.Rules.Val {
			if _, clash := baseIDs[r.ID]; clash {
				out = append(out, Finding{RuleID: "M03", Flag: f.Key, Env: env, Layer: LayerOverlay, Field: FieldRules,
					Message: fmt.Sprintf("appended rule id %q collides with a base rule id", r.ID), Severity: sev})
			}
		}
	}
	seen := make(map[string]struct{}, len(f.Rules))
	for i, r := range f.Rules {
		path := fmt.Sprintf("%s[%d]", FieldRules, i)
		if _, dup := seen[r.ID]; dup {
			out = append(out, Finding{RuleID: "M04", Flag: f.Key, Env: env, Layer: prov.Layer(FieldRules), Field: path + ".id",
				Message: fmt.Sprintf("duplicate rule id %q in the resolved list", r.ID), Severity: sev})
		}
		seen[r.ID] = struct{}{}
		// M06 -- an overlay rule does not know the base type.
		if r.Value.Type() != f.Type {
			out = append(out, Finding{RuleID: "M06", Flag: f.Key, Env: env, Layer: prov.Layer(FieldRules), Field: path + ".value",
				Message: fmt.Sprintf("rule %q value is %s, flag is %s", r.ID, r.Value.Type(), f.Type), Severity: sev})
		}
	}

	if f.Rollout != nil {
		// M05 -- each layer can be individually in range and still resolve out
		// of range if a layer is malformed or a future field widens.
		if f.Rollout.BasisPoints < 0 || f.Rollout.BasisPoints > core.BucketSpace {
			add("M05", FieldRolloutBasisPoints, fmt.Sprintf("resolved basis_points %d outside 0..%d", f.Rollout.BasisPoints, core.BucketSpace), sev)
		}
		// M17 -- a rollout with no on/off value cannot produce a value at all.
		if f.Rollout.OnValue.IsUnknown() {
			add("M17", FieldRolloutOnValue, "rollout is configured but has no on_value", sev)
		}
		if f.Rollout.OffValue.IsUnknown() {
			add("M17", FieldRolloutOffValue, "rollout is configured but has no off_value", sev)
		}
		// M18 -- the rollout's values must be the flag's type.
		if !f.Rollout.OnValue.IsUnknown() && f.Rollout.OnValue.Type() != f.Type {
			add("M18", FieldRolloutOnValue, fmt.Sprintf("rollout on_value is %s, flag is %s", f.Rollout.OnValue.Type(), f.Type), sev)
		}
		if !f.Rollout.OffValue.IsUnknown() && f.Rollout.OffValue.Type() != f.Type {
			add("M18", FieldRolloutOffValue, fmt.Sprintf("rollout off_value is %s, flag is %s", f.Rollout.OffValue.Type(), f.Type), sev)
		}
	} else if ops != nil && ops.BasisPoints.IsValue() {
		// M19 -- an ops override that cannot take effect is worse than one that
		// is rejected: on-call believes the rollout was throttled and it was not.
		out = append(out, Finding{RuleID: "M19", Flag: f.Key, Env: env, Layer: LayerOps, Field: FieldRolloutBasisPoints,
			Message: "ops override sets basis_points but the resolved flag has no rollout", Severity: sev})
	}

	// M09 -- decision O2. Rules-first and rollout-gates-first accept
	// byte-identical config and mean different things, so a flag carrying both is
	// REJECTED rather than defaulted. Defaulting would decide O2 by accident and
	// make any later change a silent behavioural migration across every flag.
	if len(f.Rules) > 0 && f.Rollout != nil && f.EvaluationOrder == core.OrderUnspecified {
		add("M09", FieldEvaluationOrder, "flag has both rules and a rollout but declares no evaluation_order; the ordering is not defaultable", sev)
	}

	return out
}

// orphanFinding is M02: an overlay or ops entry naming a flag with no base entry.
// It is unservable rather than merely wrong -- there is no type and no default, so
// no core.Flag can be constructed at all.
func orphanFinding(env, key string, layer LayerID) Finding {
	return Finding{RuleID: "M02", Flag: key, Env: env, Layer: layer, Field: FieldKey,
		Message: "no base entry for this flag; an overlay alone carries no type and no default", Severity: SeverityRejectFlag}
}

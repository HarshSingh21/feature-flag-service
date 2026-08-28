package config

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// LayerID names a configuration layer. Precedence is ascending: L1 < L2 < L3.
//
// L0 -- the caller-supplied compiled-in default -- is deliberately NOT a member.
// It lives in the caller's binary, at the call site, and the merge pipeline never
// sees it. It is the terminal fallback used when the flag, the snapshot or the
// service itself does not exist, which is precisely the situation in which a
// merge cannot help.
type LayerID uint8

const (
	// LayerNone is the zero value: no layer supplied this field.
	LayerNone LayerID = iota
	// LayerBase is L1: the total record. Environment-agnostic flag identity.
	LayerBase
	// LayerOverlay is L2: the per-environment sparse patch, written by CI.
	LayerOverlay
	// LayerOps is L3: the on-call kill switch. Whitelisted fields, TTL-bound.
	LayerOps
)

func (l LayerID) String() string {
	switch l {
	case LayerBase:
		return "L1:base"
	case LayerOverlay:
		return "L2:overlay"
	case LayerOps:
		return "L3:ops"
	default:
		return "none"
	}
}

// Layer is the closed set of things that can be handed to Store.Set.
type Layer interface {
	// LayerID reports which layer this is.
	LayerID() LayerID
	// LayerEnv reports the environment this layer targets. Empty for the base
	// layer, which is environment-agnostic and therefore shared by every env.
	LayerEnv() string
	// CloneLayer returns a deep copy, so the store can hand raw layers to a
	// forensics endpoint without exposing its own state to mutation.
	CloneLayer() Layer
}

// -----------------------------------------------------------------------------
// Shared wire types
// -----------------------------------------------------------------------------

// WireCondition is the JSON form of core.Condition, carrying the operator as the
// wire string so an unknown operator is a validation finding rather than a decode
// error that aborts the whole document.
type WireCondition struct {
	Attribute string       `json:"attribute"`
	Op        string       `json:"op"`
	Values    []core.Value `json:"values,omitempty"`
	Negate    bool         `json:"negate,omitempty"`
}

// WireRule is the JSON form of core.Rule.
//
// The list these live in is ORDERED and first-match-wins, so its position is its
// semantics. That is why an overlay may only replace or append the list, never
// deep-merge or key-merge it.
type WireRule struct {
	ID         string          `json:"id"`
	Conditions []WireCondition `json:"conditions"`
	Combiner   string          `json:"combiner,omitempty"` // "and" (default) | "or"
	Value      core.Value      `json:"value"`
}

// WireRollout is the TOTAL form of a rollout, used by the base layer.
type WireRollout struct {
	// BasisPoints is the rollout size in hundredths of a percent, 0..10000.
	BasisPoints int32 `json:"basis_points"`
	// BucketNamespace is the hash salt. Empty means "bucket by the flag key",
	// which is decision O1: flags bucket independently unless they are explicitly
	// given a shared literal. Empty and set are distinct states and the merge
	// preserves the distinction faithfully.
	BucketNamespace string `json:"bucket_namespace"`
	// BucketBy names the context attribute used as the bucketing subject.
	// Empty means user_id.
	BucketBy string     `json:"bucket_by"`
	OnValue  core.Value `json:"on_value"`
	OffValue core.Value `json:"off_value"`
}

func (r *WireRollout) clone() *WireRollout {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func (r *WireRollout) toCore() *core.Rollout {
	if r == nil {
		return nil
	}
	return &core.Rollout{
		BasisPoints:     r.BasisPoints,
		BucketNamespace: r.BucketNamespace,
		BucketBy:        r.BucketBy,
		OnValue:         r.OnValue,
		OffValue:        r.OffValue,
	}
}

// OverlayRollout is the SPARSE mirror of WireRollout.
//
// It exists so that `rollout: {basis_points: 2500}` deep-merges rather than
// replacing the whole block. A whole-block replace would blank BucketNamespace,
// which re-buckets every user: a routine 5% -> 25% bump would flip an arbitrary
// set of already-enrolled users OFF while enrolling strangers, with every
// dashboard staying green. That is a production incident produced by a merge rule.
type OverlayRollout struct {
	BasisPoints     Opt[int32]      `json:"basis_points,omitzero"`
	BucketNamespace Opt[string]     `json:"bucket_namespace,omitzero"`
	BucketBy        Opt[string]     `json:"bucket_by,omitzero"`
	OnValue         Opt[core.Value] `json:"on_value,omitzero"`
	OffValue        Opt[core.Value] `json:"off_value,omitzero"`
}

// RuleListMode is the overlay's declared rule-list operator.
//
// Exactly two operators, mutually exclusive, and no prepend or insert-at. A
// prepended overlay rule can shadow every base rule, which is a replace wearing a
// disguise; forcing the author to write the full replace makes them look at the
// whole ordering while they do it.
type RuleListMode string

const (
	// RuleModeReplace discards the base list and installs the overlay's list.
	RuleModeReplace RuleListMode = "replace"
	// RuleModeAppend keeps the base list, in order, then adds the overlay's.
	RuleModeAppend RuleListMode = "append"
)

// -----------------------------------------------------------------------------
// L1 -- base layer. TOTAL records: plain values, every field required.
// -----------------------------------------------------------------------------

// BaseLayer is the environment-agnostic flag catalogue.
type BaseLayer struct {
	SchemaVersion int        `json:"schema_version"`
	Flags         []BaseFlag `json:"flags"`
}

func (*BaseLayer) LayerID() LayerID    { return LayerBase }
func (*BaseLayer) LayerEnv() string    { return "" }
func (l *BaseLayer) CloneLayer() Layer { return l.Clone() }

// Clone returns a deep copy.
func (l *BaseLayer) Clone() *BaseLayer {
	if l == nil {
		return nil
	}
	c := &BaseLayer{SchemaVersion: l.SchemaVersion}
	if l.Flags != nil {
		c.Flags = make([]BaseFlag, len(l.Flags))
		for i := range l.Flags {
			c.Flags[i] = l.Flags[i].clone()
		}
	}
	return c
}

// requiredBaseFields are the keys a base record must carry. The base layer is a
// TOTAL record by design -- you cannot author a partial base, because a partial
// base has no type and no default and therefore cannot produce a servable flag.
var requiredBaseFields = []string{"key", "type", "enabled", "default_value"}

// BaseFlag is one total flag definition. Plain values throughout: no Opt, so the
// type system itself refuses a sparse base.
type BaseFlag struct {
	Key             string       `json:"key"`
	Type            string       `json:"type"` // wire string; core.ParseValueType maps it
	Owner           string       `json:"owner,omitempty"`
	Description     string       `json:"description,omitempty"`
	Enabled         bool         `json:"enabled"`
	DefaultValue    core.Value   `json:"default_value"`
	OffValue        core.Value   `json:"off_value,omitzero"` // omitted => defaults to DefaultValue
	Rules           []WireRule   `json:"rules,omitempty"`
	Rollout         *WireRollout `json:"rollout,omitempty"`
	EvaluationOrder string       `json:"evaluation_order,omitempty"`

	// missing records required keys absent from the source document. It is
	// captured at decode time and reported by the validator rather than returned
	// as a decode error, so that one incomplete record does not abort the parse
	// and hide every other problem in the file.
	missing []string
}

// MissingFields lists required keys that were absent in the decoded document.
func (f *BaseFlag) MissingFields() []string { return append([]string(nil), f.missing...) }

// UnmarshalJSON decodes the record and separately probes for required keys.
func (f *BaseFlag) UnmarshalJSON(b []byte) error {
	type alias BaseFlag // sheds the method set, so no recursion
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	*f = BaseFlag(a)
	f.missing = nil
	for _, k := range requiredBaseFields {
		if _, ok := probe[k]; !ok {
			f.missing = append(f.missing, k)
		}
	}
	return nil
}

func (f BaseFlag) clone() BaseFlag {
	c := f
	c.Rules = cloneWireRules(f.Rules)
	c.Rollout = f.Rollout.clone()
	c.missing = append([]string(nil), f.missing...)
	return c
}

// -----------------------------------------------------------------------------
// L2 -- environment overlay. SPARSE patches: every field tri-state.
// -----------------------------------------------------------------------------

// OverlayLayer is one environment's divergence from the base.
type OverlayLayer struct {
	SchemaVersion int           `json:"schema_version"`
	Environment   string        `json:"environment"`
	Flags         []OverlayFlag `json:"flags"`
}

func (*OverlayLayer) LayerID() LayerID    { return LayerOverlay }
func (l *OverlayLayer) LayerEnv() string  { return l.Environment }
func (l *OverlayLayer) CloneLayer() Layer { return l.Clone() }

// Clone returns a deep copy.
func (l *OverlayLayer) Clone() *OverlayLayer {
	if l == nil {
		return nil
	}
	c := &OverlayLayer{SchemaVersion: l.SchemaVersion, Environment: l.Environment}
	if l.Flags != nil {
		c.Flags = make([]OverlayFlag, len(l.Flags))
		for i := range l.Flags {
			c.Flags[i] = l.Flags[i].clone()
		}
	}
	return c
}

// OverlayFlag is a sparse patch over a base flag.
//
// Type is present only so that an overlay restating it can be REJECTED. Type is
// base-only and immutable; allowing a matching restatement invites a future
// non-matching one.
type OverlayFlag struct {
	Key             string              `json:"key"`
	Type            Opt[string]         `json:"type,omitzero"` // present => reject (O02)
	Enabled         Opt[bool]           `json:"enabled,omitzero"`
	DefaultValue    Opt[core.Value]     `json:"default_value,omitzero"`
	OffValue        Opt[core.Value]     `json:"off_value,omitzero"`
	Rules           Opt[[]WireRule]     `json:"rules,omitzero"`
	RulesMode       Opt[RuleListMode]   `json:"rules_mode,omitzero"` // required whenever Rules is present
	Rollout         Opt[OverlayRollout] `json:"rollout,omitzero"`    // deep merge; explicit null deletes the block
	EvaluationOrder Opt[string]         `json:"evaluation_order,omitzero"`
}

func (f OverlayFlag) clone() OverlayFlag {
	c := f
	if f.Rules.IsValue() {
		c.Rules = Some(cloneWireRules(f.Rules.Val))
	}
	return c
}

// -----------------------------------------------------------------------------
// L3 -- ops override. Whitelisted fields only, TTL mandatory.
// -----------------------------------------------------------------------------

// MaxOpsOverrideTTL is the hard cap on an ops override's lifetime. A kill switch
// with no expiry -- or with a year-long one -- stops being an incident tool and
// becomes a second, invisible config system.
const MaxOpsOverrideTTL = 30 * 24 * time.Hour

// OpsOverrideTTLWarn is the point past which a TTL is legal but suspicious.
const OpsOverrideTTLWarn = 72 * time.Hour

// opsAllowedFields is the L3 whitelist. Mutable fields are `enabled` and the
// rollout's `basis_points`; the rest is mandatory provenance metadata.
//
// L3 exists because Set is CI-driven: an on-call who kills a flag by editing the
// prod overlay gets it silently resurrected by the next pipeline run. L3 outranks
// L2 so the kill survives that pipeline run, and self-expires so it does not
// become permanent config.
var opsAllowedFields = map[string]bool{
	"key":          true,
	"enabled":      true,
	"basis_points": true,
	"expires_at":   true,
	"reason":       true,
	"owner":        true,
}

// OpsLayer holds one environment's active ops overrides.
type OpsLayer struct {
	SchemaVersion int           `json:"schema_version"`
	Environment   string        `json:"environment"`
	Overrides     []OpsOverride `json:"overrides"`
}

func (*OpsLayer) LayerID() LayerID    { return LayerOps }
func (l *OpsLayer) LayerEnv() string  { return l.Environment }
func (l *OpsLayer) CloneLayer() Layer { return l.Clone() }

// Clone returns a deep copy.
func (l *OpsLayer) Clone() *OpsLayer {
	if l == nil {
		return nil
	}
	c := &OpsLayer{SchemaVersion: l.SchemaVersion, Environment: l.Environment}
	if l.Overrides != nil {
		c.Overrides = make([]OpsOverride, len(l.Overrides))
		for i := range l.Overrides {
			c.Overrides[i] = l.Overrides[i].clone()
		}
	}
	return c
}

// OpsOverride is a TTL-bound emergency patch over the resolved flag.
type OpsOverride struct {
	Key         string         `json:"key"`
	Enabled     Opt[bool]      `json:"enabled,omitzero"`
	BasisPoints Opt[int32]     `json:"basis_points,omitzero"`
	ExpiresAt   Opt[time.Time] `json:"expires_at,omitzero"` // REQUIRED
	Reason      string         `json:"reason,omitempty"`    // REQUIRED
	Owner       string         `json:"owner,omitempty"`     // REQUIRED

	// disallowed records keys outside the whitelist, captured at decode time so
	// the validator can name each one instead of silently ignoring it.
	disallowed []string
}

// DisallowedFields lists keys the override carried that are outside the L3
// whitelist.
func (o *OpsOverride) DisallowedFields() []string { return append([]string(nil), o.disallowed...) }

// UnmarshalJSON decodes the override and records any non-whitelisted keys.
func (o *OpsOverride) UnmarshalJSON(b []byte) error {
	type alias OpsOverride
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	*o = OpsOverride(a)
	o.disallowed = nil
	for k := range probe {
		if !opsAllowedFields[k] {
			o.disallowed = append(o.disallowed, k)
		}
	}
	sortStrings(o.disallowed)
	return nil
}

func (o OpsOverride) clone() OpsOverride {
	c := o
	c.disallowed = append([]string(nil), o.disallowed...)
	return c
}

// -----------------------------------------------------------------------------
// Parsing helpers
// -----------------------------------------------------------------------------

// ParseBaseLayer decodes a base layer document.
func ParseBaseLayer(b []byte) (*BaseLayer, error) {
	var l BaseLayer
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("config: parse base layer: %w", err)
	}
	return &l, nil
}

// ParseOverlayLayer decodes an environment overlay document.
func ParseOverlayLayer(b []byte) (*OverlayLayer, error) {
	var l OverlayLayer
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("config: parse overlay layer: %w", err)
	}
	return &l, nil
}

// ParseOpsLayer decodes an ops override document.
func ParseOpsLayer(b []byte) (*OpsLayer, error) {
	var l OpsLayer
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("config: parse ops layer: %w", err)
	}
	return &l, nil
}

// parseLogicOp maps the wire combiner. Empty means AND, the safer default.
func parseLogicOp(s string) (core.LogicOp, bool) {
	switch s {
	case "", "and":
		return core.LogicAnd, true
	case "or":
		return core.LogicOr, true
	default:
		return core.LogicAnd, false
	}
}

// parseEvaluationOrder maps the wire evaluation order.
//
// Empty maps to core.OrderUnspecified, which is legal ONLY when the flag does not
// carry both rules and a rollout. It is never defaulted to a real ordering:
// two orderings accept byte-identical config and mean different things, so
// defaulting would decide question O2 by accident and make any later change a
// silent behavioural migration across every flag.
func parseEvaluationOrder(s string) (core.EvaluationOrder, bool) {
	switch s {
	case "":
		return core.OrderUnspecified, true
	case "rules_first", "rules_then_rollout":
		return core.OrderRulesFirst, true
	default:
		return core.OrderUnspecified, false
	}
}

func cloneWireRules(in []WireRule) []WireRule {
	if in == nil {
		return nil
	}
	out := make([]WireRule, len(in))
	for i, r := range in {
		c := r
		if r.Conditions != nil {
			c.Conditions = make([]WireCondition, len(r.Conditions))
			for j, cond := range r.Conditions {
				cc := cond
				if cond.Values != nil {
					cc.Values = append([]core.Value(nil), cond.Values...)
				}
				c.Conditions[j] = cc
			}
		}
		out[i] = c
	}
	return out
}

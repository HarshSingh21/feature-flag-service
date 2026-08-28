package config

import (
	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// Field path constants used as provenance keys and as the Field of a Finding.
// They are dotted paths so an operator can map a finding straight onto the
// document that produced it.
const (
	FieldKey                    = "key"
	FieldType                   = "type"
	FieldEnabled                = "enabled"
	FieldDefaultValue           = "default_value"
	FieldOffValue               = "off_value"
	FieldRules                  = "rules"
	FieldRulesMode              = "rules_mode"
	FieldEvaluationOrder        = "evaluation_order"
	FieldRollout                = "rollout"
	FieldRolloutBasisPoints     = "rollout.basis_points"
	FieldRolloutBucketNamespace = "rollout.bucket_namespace"
	FieldRolloutBucketBy        = "rollout.bucket_by"
	FieldRolloutOnValue         = "rollout.on_value"
	FieldRolloutOffValue        = "rollout.off_value"
	FieldExpiresAt              = "expires_at"
	FieldReason                 = "reason"
	FieldOwner                  = "owner"
)

// FlagProvenance records which layer supplied each field of a resolved flag.
//
// "What did the base say versus the prod overlay?" is the first question asked
// during an incident, and a merged object without provenance cannot answer it.
// Provenance is built once on the write path and is read-only thereafter.
type FlagProvenance struct {
	Key    string
	Fields map[string]LayerID
}

// Layer reports which layer supplied the given field path.
func (p FlagProvenance) Layer(field string) LayerID {
	if p.Fields == nil {
		return LayerNone
	}
	return p.Fields[field]
}

// Clone returns a deep copy, so a debug endpoint cannot mutate snapshot state.
func (p FlagProvenance) Clone() FlagProvenance {
	c := FlagProvenance{Key: p.Key}
	if p.Fields != nil {
		c.Fields = make(map[string]LayerID, len(p.Fields))
		for k, v := range p.Fields {
			c.Fields[k] = v
		}
	}
	return c
}

func (p FlagProvenance) set(field string, l LayerID) {
	p.Fields[field] = l
}

// mergeFlag resolves one flag by deep-merging L1 -> L2 -> L3.
//
// Inputs are assumed to have passed pre-merge validation; anything merge cannot
// decide (type agreement across layers, rule id collisions, O2's explicit
// evaluation order) is left for validateResolved. Every slice and every pointer
// in the result is freshly allocated: snapshots for different environments never
// share a backing array, even when the resolved content is byte-identical.
//
// Per-field policy:
//
//	key, type       identity   base only; an overlay restating them is rejected
//	enabled         scalar     highest layer that supplies it wins (L1<L2<L3)
//	default_value   scalar     L1<L2            (outside the L3 whitelist)
//	off_value       scalar     L1<L2            (absent in base => default_value)
//	rules           list       replace or append, whole-list, never element-wise
//	rollout         object     RECURSIVE DEEP MERGE; explicit null deletes it
//	rollout.*       scalar     L1<L2; basis_points additionally L3
//	evaluation_order scalar    L1<L2
func mergeFlag(base *BaseFlag, ov *OverlayFlag, ops *OpsOverride) (*core.Flag, FlagProvenance) {
	prov := FlagProvenance{Key: base.Key, Fields: make(map[string]LayerID, 14)}

	typ, _ := core.ParseValueType(base.Type)
	order, _ := parseEvaluationOrder(base.EvaluationOrder)

	out := &core.Flag{
		Key:             base.Key,
		Type:            typ,
		Enabled:         base.Enabled,
		DefaultValue:    base.DefaultValue,
		OffValue:        base.OffValue,
		Rules:           wireRulesToCore(base.Rules),
		Rollout:         base.Rollout.toCore(),
		EvaluationOrder: order,
	}
	// An omitted base off_value means "serve the default when switched off".
	if out.OffValue.IsUnknown() {
		out.OffValue = base.DefaultValue
	}
	prov.set(FieldKey, LayerBase)
	prov.set(FieldType, LayerBase)
	prov.set(FieldEnabled, LayerBase)
	prov.set(FieldDefaultValue, LayerBase)
	prov.set(FieldOffValue, LayerBase)
	prov.set(FieldRules, LayerBase)
	prov.set(FieldEvaluationOrder, LayerBase)
	if out.Rollout != nil {
		prov.set(FieldRollout, LayerBase)
		prov.set(FieldRolloutBasisPoints, LayerBase)
		prov.set(FieldRolloutBucketNamespace, LayerBase)
		prov.set(FieldRolloutBucketBy, LayerBase)
		prov.set(FieldRolloutOnValue, LayerBase)
		prov.set(FieldRolloutOffValue, LayerBase)
	}

	// ---- L2: environment overlay --------------------------------------------
	if ov != nil {
		if v, ok := ov.Enabled.Get(); ok {
			out.Enabled = v
			prov.set(FieldEnabled, LayerOverlay)
		}
		if v, ok := ov.DefaultValue.Get(); ok {
			out.DefaultValue = v
			prov.set(FieldDefaultValue, LayerOverlay)
		}
		if v, ok := ov.OffValue.Get(); ok {
			out.OffValue = v
			prov.set(FieldOffValue, LayerOverlay)
		}
		if v, ok := ov.EvaluationOrder.Get(); ok {
			if o, ok := parseEvaluationOrder(v); ok {
				out.EvaluationOrder = o
				prov.set(FieldEvaluationOrder, LayerOverlay)
			}
		}
		if ov.Rules.Set {
			// An explicit null is equivalent to an empty list: "this environment
			// has no rules". Order is semantics, so this is whole-list only.
			var incoming []core.Rule
			if !ov.Rules.Null {
				incoming = wireRulesToCore(ov.Rules.Val)
			}
			if ov.RulesMode.OrElse(RuleModeReplace) == RuleModeAppend {
				merged := make([]core.Rule, 0, len(out.Rules)+len(incoming))
				merged = append(merged, out.Rules...)
				merged = append(merged, incoming...)
				out.Rules = merged
			} else {
				out.Rules = incoming
			}
			prov.set(FieldRules, LayerOverlay)
		}
		if ov.Rollout.Set {
			if ov.Rollout.Null {
				// "prod has no percentage rollout stage at all" -- a different
				// code path from basis_points: 0, which runs the stage and puts
				// everyone in the off cohort.
				out.Rollout = nil
				prov.set(FieldRollout, LayerOverlay)
				delete(prov.Fields, FieldRolloutBasisPoints)
				delete(prov.Fields, FieldRolloutBucketNamespace)
				delete(prov.Fields, FieldRolloutBucketBy)
				delete(prov.Fields, FieldRolloutOnValue)
				delete(prov.Fields, FieldRolloutOffValue)
			} else {
				if out.Rollout == nil {
					out.Rollout = &core.Rollout{}
					prov.set(FieldRollout, LayerOverlay)
					prov.set(FieldRolloutBasisPoints, LayerOverlay)
					prov.set(FieldRolloutBucketNamespace, LayerOverlay)
					prov.set(FieldRolloutBucketBy, LayerOverlay)
					prov.set(FieldRolloutOnValue, LayerOverlay)
					prov.set(FieldRolloutOffValue, LayerOverlay)
				}
				r := ov.Rollout.Val
				// Deep merge, field by field. Every field the overlay does not
				// mention keeps the base value -- in particular bucket_namespace,
				// whose blanking would re-bucket every enrolled user.
				if v, ok := r.BasisPoints.Get(); ok {
					out.Rollout.BasisPoints = v
					prov.set(FieldRolloutBasisPoints, LayerOverlay)
				}
				if v, ok := r.BucketNamespace.Get(); ok {
					out.Rollout.BucketNamespace = v
					prov.set(FieldRolloutBucketNamespace, LayerOverlay)
				}
				if v, ok := r.BucketBy.Get(); ok {
					out.Rollout.BucketBy = v
					prov.set(FieldRolloutBucketBy, LayerOverlay)
				}
				if v, ok := r.OnValue.Get(); ok {
					out.Rollout.OnValue = v
					prov.set(FieldRolloutOnValue, LayerOverlay)
				}
				if v, ok := r.OffValue.Get(); ok {
					out.Rollout.OffValue = v
					prov.set(FieldRolloutOffValue, LayerOverlay)
				}
			}
		}
	}

	// ---- L3: ops override ----------------------------------------------------
	// Outranks L2 on purpose: an on-call kill must survive the next CI overlay push.
	if ops != nil {
		if v, ok := ops.Enabled.Get(); ok {
			out.Enabled = v
			prov.set(FieldEnabled, LayerOps)
		}
		if v, ok := ops.BasisPoints.Get(); ok && out.Rollout != nil {
			out.Rollout.BasisPoints = v
			prov.set(FieldRolloutBasisPoints, LayerOps)
		}
	}

	return out, prov
}

func wireRulesToCore(in []WireRule) []core.Rule {
	if in == nil {
		return nil
	}
	out := make([]core.Rule, len(in))
	for i, r := range in {
		combiner, _ := parseLogicOp(r.Combiner)
		cr := core.Rule{ID: r.ID, Combiner: combiner, Value: r.Value}
		if r.Conditions != nil {
			cr.Conditions = make([]core.Condition, len(r.Conditions))
			for j, c := range r.Conditions {
				op, _ := core.ParseOperator(c.Op)
				cc := core.Condition{Attribute: c.Attribute, Op: op, Negate: c.Negate}
				if c.Values != nil {
					cc.Values = append([]core.Value(nil), c.Values...)
				}
				cr.Conditions[j] = cc
			}
		}
		out[i] = cr
	}
	return out
}

// cloneFlag deep copies a resolved flag. Used when a quarantined flag carries its
// last-known-good version into a new generation: the copy is unconditional so that
// no two snapshots ever share a slice backing array, which forecloses a whole
// class of bug where a future in-place optimisation corrupts one generation from
// another.
func cloneFlag(f *core.Flag) *core.Flag {
	if f == nil {
		return nil
	}
	c := *f
	if f.Rules != nil {
		c.Rules = make([]core.Rule, len(f.Rules))
		for i, r := range f.Rules {
			rc := r
			if r.Conditions != nil {
				rc.Conditions = make([]core.Condition, len(r.Conditions))
				for j, cond := range r.Conditions {
					cc := cond
					if cond.Values != nil {
						cc.Values = append([]core.Value(nil), cond.Values...)
					}
					rc.Conditions[j] = cc
				}
			}
			c.Rules[i] = rc
		}
	}
	if f.Rollout != nil {
		r := *f.Rollout
		c.Rollout = &r
	}
	return &c
}

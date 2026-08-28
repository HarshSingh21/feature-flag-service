package config

import (
	"testing"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// resolve merges the canonical base with optional overlay/ops documents.
func resolve(t *testing.T, baseDoc, overlayDoc, opsDoc string) (*core.Flag, FlagProvenance) {
	t.Helper()
	base := mustBase(t, baseDoc)
	var ov *OverlayFlag
	if overlayDoc != "" {
		l := mustOverlay(t, overlayDoc)
		for i := range l.Flags {
			if l.Flags[i].Key == base.Flags[0].Key {
				ov = &l.Flags[i]
			}
		}
	}
	var ops *OpsOverride
	if opsDoc != "" {
		l := mustOps(t, opsDoc)
		for i := range l.Overrides {
			if l.Overrides[i].Key == base.Flags[0].Key {
				ops = &l.Overrides[i]
			}
		}
	}
	return mergeFlag(&base.Flags[0], ov, ops)
}

func TestMergeBaseAloneResolvesTotalRecord(t *testing.T) {
	t.Parallel()
	f, prov := resolve(t, canonicalBase, "", "")
	if f.Key != flagKey || f.Type != core.TypeBool || !f.Enabled {
		t.Fatalf("unexpected resolved flag: %+v", f)
	}
	if f.EvaluationOrder != core.OrderRulesFirst {
		t.Fatalf("evaluation_order: %v", f.EvaluationOrder)
	}
	for _, field := range []string{FieldKey, FieldType, FieldEnabled, FieldDefaultValue, FieldRules, FieldRolloutBasisPoints} {
		if got := prov.Layer(field); got != LayerBase {
			t.Fatalf("provenance[%s] = %s, want L1", field, got)
		}
	}
}

func TestMergeOmittedBaseOffValueFallsBackToDefault(t *testing.T) {
	t.Parallel()
	f, _ := resolve(t, `{"flags":[{"key":"a.b","type":"string","owner":"o","enabled":true,"default_value":"d"}]}`, "", "")
	if v, _ := f.OffValue.AsString(); v != "d" {
		t.Fatalf("off_value: got %v, want the default value", f.OffValue)
	}
}

func TestMergeScalarOverrideByOverlay(t *testing.T) {
	t.Parallel()
	f, prov := resolve(t, canonicalBase, `{
      "environment":"prod",
      "flags":[{"key":"`+flagKey+`","enabled":false,"default_value":true}]
    }`, "")
	if f.Enabled {
		t.Fatal("overlay enabled:false must win over base enabled:true")
	}
	if v, _ := f.DefaultValue.AsBool(); !v {
		t.Fatal("overlay default_value must win")
	}
	if prov.Layer(FieldEnabled) != LayerOverlay || prov.Layer(FieldDefaultValue) != LayerOverlay {
		t.Fatalf("provenance did not record the overlay: %+v", prov.Fields)
	}
	// Untouched fields keep base provenance -- that is the incident question
	// "what did BASE say versus the prod overlay" being answerable.
	if prov.Layer(FieldOffValue) != LayerBase {
		t.Fatalf("off_value provenance: %s", prov.Layer(FieldOffValue))
	}
}

func TestMergeAbsentOverlayFieldInherits(t *testing.T) {
	t.Parallel()
	f, _ := resolve(t, canonicalBase, `{"environment":"prod","flags":[{"key":"`+flagKey+`"}]}`, "")
	if !f.Enabled {
		t.Fatal("an overlay that mentions nothing must inherit everything")
	}
	if len(f.Rules) != 2 || f.Rollout == nil || f.Rollout.BasisPoints != 500 {
		t.Fatalf("empty overlay changed the resolved flag: %+v", f)
	}
}

// This is the incident-shaped bug. A whole-block replace would let
// {basis_points: 2500} blank bucket_namespace, which re-buckets every user and
// flips already-enrolled users off during a routine percentage bump.
func TestMergeRolloutDeepMergePreservesBucketNamespace(t *testing.T) {
	t.Parallel()
	f, prov := resolve(t, canonicalBase, `{
      "environment":"prod",
      "flags":[{"key":"`+flagKey+`","rollout":{"basis_points":2500}}]
    }`, "")

	if f.Rollout == nil {
		t.Fatal("rollout was deleted by a basis-points-only overlay")
	}
	if f.Rollout.BasisPoints != 2500 {
		t.Fatalf("basis_points: got %d want 2500", f.Rollout.BasisPoints)
	}
	if f.Rollout.BucketNamespace != "checkout-cohort-a" {
		t.Fatalf("bucket_namespace was blanked by the merge: got %q -- this re-buckets every enrolled user",
			f.Rollout.BucketNamespace)
	}
	if f.Rollout.BucketBy != "user_id" {
		t.Fatalf("bucket_by was blanked: got %q", f.Rollout.BucketBy)
	}
	if v, ok := f.Rollout.OnValue.AsBool(); !ok || !v {
		t.Fatalf("on_value was blanked: %v", f.Rollout.OnValue)
	}
	if v, ok := f.Rollout.OffValue.AsBool(); !ok || v {
		t.Fatalf("off_value was blanked: %v", f.Rollout.OffValue)
	}
	if prov.Layer(FieldRolloutBasisPoints) != LayerOverlay {
		t.Fatal("basis_points provenance must name the overlay")
	}
	if prov.Layer(FieldRolloutBucketNamespace) != LayerBase {
		t.Fatal("bucket_namespace provenance must still name the base")
	}
}

func TestMergeRolloutEmptyNamespaceIsPreservedFaithfully(t *testing.T) {
	t.Parallel()
	// O1: an empty bucket_namespace means "bucket by the flag key". Empty and set
	// are different states and the merge must not confuse them in either direction.
	base := `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":false,
	  "rollout":{"basis_points":100,"bucket_namespace":"","bucket_by":"user_id","on_value":true,"off_value":false}}]}`

	f, _ := resolve(t, base, "", "")
	if f.Rollout.BucketNamespace != "" {
		t.Fatalf("base empty namespace became %q", f.Rollout.BucketNamespace)
	}

	// An overlay may explicitly set it...
	f, prov := resolve(t, base, `{"environment":"prod","flags":[{"key":"a.b","rollout":{"bucket_namespace":"shared"}}]}`, "")
	if f.Rollout.BucketNamespace != "shared" {
		t.Fatalf("overlay namespace not applied: %q", f.Rollout.BucketNamespace)
	}
	if prov.Layer(FieldRolloutBucketNamespace) != LayerOverlay {
		t.Fatal("namespace provenance must name the overlay")
	}

	// ...and may explicitly set it back to empty, which is a real value meaning
	// "bucket by flag key", not "inherit".
	nonEmpty := `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":false,
	  "rollout":{"basis_points":100,"bucket_namespace":"shared","bucket_by":"user_id","on_value":true,"off_value":false}}]}`
	f, prov = resolve(t, nonEmpty, `{"environment":"prod","flags":[{"key":"a.b","rollout":{"bucket_namespace":""}}]}`, "")
	if f.Rollout.BucketNamespace != "" {
		t.Fatalf("explicit empty namespace not applied: %q", f.Rollout.BucketNamespace)
	}
	if prov.Layer(FieldRolloutBucketNamespace) != LayerOverlay {
		t.Fatal("explicit empty must still be recorded as an overlay decision")
	}
}

func TestMergeRolloutExplicitNullDeletesTheBlock(t *testing.T) {
	t.Parallel()
	// rollout: null means "this environment has no percentage rollout stage at
	// all" -- a different code path from basis_points: 0, which runs the stage and
	// puts everyone in the off cohort.
	f, prov := resolve(t, canonicalBase, `{"environment":"prod","flags":[{"key":"`+flagKey+`","rollout":null}]}`, "")
	if f.Rollout != nil {
		t.Fatalf("rollout: null must delete the block, got %+v", f.Rollout)
	}
	if prov.Layer(FieldRollout) != LayerOverlay {
		t.Fatal("the deletion must be attributed to the overlay")
	}
	if _, ok := prov.Fields[FieldRolloutBasisPoints]; ok {
		t.Fatal("provenance for deleted rollout subfields must be dropped")
	}
}

func TestMergeRuleListReplace(t *testing.T) {
	t.Parallel()
	f, prov := resolve(t, canonicalBase, `{
      "environment":"prod",
      "flags":[{"key":"`+flagKey+`","rules_mode":"replace",
        "rules":[{"id":"only-rule","conditions":[{"attribute":"country","op":"eq","values":["IN"]}],"value":true}]}]
    }`, "")
	if len(f.Rules) != 1 || f.Rules[0].ID != "only-rule" {
		t.Fatalf("replace must discard the base list, got %d rules: %+v", len(f.Rules), f.Rules)
	}
	if prov.Layer(FieldRules) != LayerOverlay {
		t.Fatal("rules provenance must name the overlay")
	}
}

func TestMergeRuleListAppendPreservesOrder(t *testing.T) {
	t.Parallel()
	// Order IS the semantics under first-match-wins: base rules first, in order,
	// then the appended ones. Never prepend, never interleave.
	f, _ := resolve(t, canonicalBase, `{
      "environment":"prod",
      "flags":[{"key":"`+flagKey+`","rules_mode":"append",
        "rules":[{"id":"prod-enterprise","conditions":[{"attribute":"tenant_tier","op":"eq","values":["enterprise"]}],"value":true}]}]
    }`, "")
	want := []string{"internal-staff", "block-sanctioned", "prod-enterprise"}
	if len(f.Rules) != len(want) {
		t.Fatalf("want %d rules, got %d", len(want), len(f.Rules))
	}
	for i, id := range want {
		if f.Rules[i].ID != id {
			t.Fatalf("rules[%d] = %q, want %q (order is the semantics)", i, f.Rules[i].ID, id)
		}
	}
}

func TestMergeRuleListNullClearsList(t *testing.T) {
	t.Parallel()
	f, _ := resolve(t, canonicalBase, `{"environment":"prod","flags":[{"key":"`+flagKey+`","rules":null,"rules_mode":"replace"}]}`, "")
	if len(f.Rules) != 0 {
		t.Fatalf("rules: null must resolve to an empty list, got %+v", f.Rules)
	}
}

func TestMergeAppendDoesNotAliasBaseRuleBacking(t *testing.T) {
	t.Parallel()
	base := mustBase(t, canonicalBase)
	ov := mustOverlay(t, `{"environment":"prod","flags":[{"key":"`+flagKey+`","rules_mode":"append",
	  "rules":[{"id":"extra","conditions":[{"attribute":"country","op":"eq","values":["IN"]}],"value":true}]}]}`)
	f1, _ := mergeFlag(&base.Flags[0], &ov.Flags[0], nil)
	f2, _ := mergeFlag(&base.Flags[0], nil, nil)

	f1.Rules[0].ID = "mutated"
	f1.Rules[0].Conditions[0].Values[0] = core.String("mutated")
	if f2.Rules[0].ID != "internal-staff" {
		t.Fatal("two merges of the same base share a rule backing array")
	}
	if v, _ := f2.Rules[0].Conditions[0].Values[0].AsString(); v != "acme.com" {
		t.Fatal("two merges of the same base share a condition values slice")
	}
	if base.Flags[0].Rules[0].ID != "internal-staff" {
		t.Fatal("merge mutated the raw base layer")
	}
}

// L3 exists because Set is CI-driven: an on-call who kills a flag by editing the
// prod overlay gets it silently resurrected by the next pipeline run.
func TestMergeOpsOverrideOutranksOverlay(t *testing.T) {
	t.Parallel()
	f, prov := resolve(t, canonicalBase,
		`{"environment":"prod","flags":[{"key":"`+flagKey+`","enabled":true,"rollout":{"basis_points":2500}}]}`,
		`{"environment":"prod","overrides":[{"key":"`+flagKey+`","enabled":false,"basis_points":0,
		  "expires_at":"2026-01-01T01:00:00Z","reason":"INC-1","owner":"oncall"}]}`)

	if f.Enabled {
		t.Fatal("L3 enabled:false must outrank the L2 overlay's enabled:true")
	}
	if f.Rollout.BasisPoints != 0 {
		t.Fatalf("L3 basis_points must outrank L2: got %d", f.Rollout.BasisPoints)
	}
	if prov.Layer(FieldEnabled) != LayerOps || prov.Layer(FieldRolloutBasisPoints) != LayerOps {
		t.Fatalf("provenance must attribute both fields to L3: %+v", prov.Fields)
	}
	// L3 touches nothing else. bucket_namespace in particular survives.
	if f.Rollout.BucketNamespace != "checkout-cohort-a" {
		t.Fatalf("L3 blanked bucket_namespace: %q", f.Rollout.BucketNamespace)
	}
	if len(f.Rules) != 2 {
		t.Fatalf("L3 must not touch the rule list, got %d rules", len(f.Rules))
	}
}

func TestMergeOpsOverrideAbsentFieldsInheritL2(t *testing.T) {
	t.Parallel()
	f, prov := resolve(t, canonicalBase,
		`{"environment":"prod","flags":[{"key":"`+flagKey+`","rollout":{"basis_points":2500}}]}`,
		`{"environment":"prod","overrides":[{"key":"`+flagKey+`","enabled":false,
		  "expires_at":"2026-01-01T01:00:00Z","reason":"INC-1","owner":"oncall"}]}`)
	if f.Rollout.BasisPoints != 2500 {
		t.Fatalf("an L3 override that does not mention basis_points must inherit L2: got %d", f.Rollout.BasisPoints)
	}
	if prov.Layer(FieldRolloutBasisPoints) != LayerOverlay {
		t.Fatal("basis_points provenance must still name the overlay")
	}
}

func TestCloneFlagIsDeep(t *testing.T) {
	t.Parallel()
	f, _ := resolve(t, canonicalBase, "", "")
	c := cloneFlag(f)
	c.Rules[0].ID = "mutated"
	c.Rules[0].Conditions[0].Values[0] = core.String("mutated")
	c.Rollout.BasisPoints = 9999
	if f.Rules[0].ID != "internal-staff" || f.Rollout.BasisPoints != 500 {
		t.Fatal("cloneFlag aliased its source")
	}
	if v, _ := f.Rules[0].Conditions[0].Values[0].AsString(); v != "acme.com" {
		t.Fatal("cloneFlag aliased the condition values slice")
	}
}

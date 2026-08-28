package config

import (
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// ---- shared test helpers ----------------------------------------------------

// testNow is the fixed build clock used across the suite, so TTL arithmetic is
// deterministic.
var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return testNow } }

func mustBase(t *testing.T, doc string) *BaseLayer {
	t.Helper()
	l, err := ParseBaseLayer([]byte(doc))
	if err != nil {
		t.Fatalf("parse base layer: %v", err)
	}
	return l
}

func mustOverlay(t *testing.T, doc string) *OverlayLayer {
	t.Helper()
	l, err := ParseOverlayLayer([]byte(doc))
	if err != nil {
		t.Fatalf("parse overlay layer: %v", err)
	}
	return l
}

func mustOps(t *testing.T, doc string) *OpsLayer {
	t.Helper()
	l, err := ParseOpsLayer([]byte(doc))
	if err != nil {
		t.Fatalf("parse ops layer: %v", err)
	}
	return l
}

// canonicalBase is the worked example from HLD B.3.1, adapted to the frozen core
// schema (basis points rather than percent, a single bucket_by attribute).
const canonicalBase = `{
  "schema_version": 1,
  "flags": [{
    "key": "checkout.new_pricing_engine",
    "type": "bool",
    "owner": "payments-team",
    "enabled": true,
    "default_value": false,
    "off_value": false,
    "rules": [
      {"id": "internal-staff",    "conditions": [{"attribute": "email_domain", "op": "eq", "values": ["acme.com"]}], "value": true},
      {"id": "block-sanctioned",  "conditions": [{"attribute": "country", "op": "in", "values": ["KP","IR"]}],       "value": false}
    ],
    "rollout": {
      "basis_points": 500,
      "bucket_namespace": "checkout-cohort-a",
      "bucket_by": "user_id",
      "on_value": true,
      "off_value": false
    },
    "evaluation_order": "rules_first"
  }]
}`

const flagKey = "checkout.new_pricing_engine"

// ---- tests ------------------------------------------------------------------

func TestBaseLayerParsesTotalRecord(t *testing.T) {
	t.Parallel()
	l := mustBase(t, canonicalBase)
	if len(l.Flags) != 1 {
		t.Fatalf("want 1 flag, got %d", len(l.Flags))
	}
	f := l.Flags[0]
	if f.Key != flagKey || f.Type != "bool" || !f.Enabled {
		t.Fatalf("unexpected base flag: %+v", f)
	}
	if len(f.missing) != 0 {
		t.Fatalf("complete record reported missing fields: %v", f.missing)
	}
	if f.Rollout == nil || f.Rollout.BucketNamespace != "checkout-cohort-a" || f.Rollout.BasisPoints != 500 {
		t.Fatalf("rollout not decoded: %+v", f.Rollout)
	}
	if len(f.Rules) != 2 || f.Rules[0].ID != "internal-staff" {
		t.Fatalf("rules not decoded in order: %+v", f.Rules)
	}
}

func TestBaseFlagRecordsMissingRequiredFields(t *testing.T) {
	t.Parallel()
	// A base layer is a TOTAL record. Missing keys are captured rather than
	// returned as a decode error, so one bad record does not hide every other
	// problem in the document.
	l := mustBase(t, `{"flags":[{"key":"a.b"}]}`)
	got := l.Flags[0].MissingFields()
	want := map[string]bool{"type": true, "enabled": true, "default_value": true}
	if len(got) != len(want) {
		t.Fatalf("missing fields: got %v want %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("unexpected missing field %q (got %v)", g, got)
		}
	}
}

func TestOverlayLayerIsSparse(t *testing.T) {
	t.Parallel()
	l := mustOverlay(t, `{
      "schema_version": 1,
      "environment": "prod",
      "flags": [{"key": "`+flagKey+`", "rollout": {"basis_points": 2500}, "rules": [], "rules_mode": "append"}]
    }`)
	f := l.Flags[0]
	if !f.Enabled.IsAbsent() || !f.DefaultValue.IsAbsent() || !f.OffValue.IsAbsent() {
		t.Fatal("unmentioned overlay scalars must be absent")
	}
	if !f.Type.IsAbsent() {
		t.Fatal("type must be absent in this overlay")
	}
	if !f.Rules.IsValue() || len(f.Rules.Val) != 0 {
		t.Fatalf("rules: %+v", f.Rules)
	}
	if mode, ok := f.RulesMode.Get(); !ok || mode != RuleModeAppend {
		t.Fatalf("rules_mode: %v %v", mode, ok)
	}
}

func TestOpsOverrideRecordsNonWhitelistedFields(t *testing.T) {
	t.Parallel()
	l := mustOps(t, `{
      "environment": "prod",
      "overrides": [{
        "key": "`+flagKey+`",
        "enabled": false,
        "default_value": true,
        "rules": [],
        "expires_at": "2026-01-01T01:00:00Z",
        "reason": "INC-1",
        "owner": "oncall"
      }]
    }`)
	got := l.Overrides[0].DisallowedFields()
	if len(got) != 2 || got[0] != "default_value" || got[1] != "rules" {
		t.Fatalf("disallowed fields: got %v", got)
	}
}

func TestOpsOverrideDecodesExpiry(t *testing.T) {
	t.Parallel()
	l := mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false,"expires_at":"2026-01-01T01:00:00Z","reason":"r","owner":"o"}]}`)
	exp, ok := l.Overrides[0].ExpiresAt.Get()
	if !ok {
		t.Fatal("expires_at must decode")
	}
	if !exp.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("expires_at: got %s", exp)
	}
}

func TestParseEvaluationOrderNeverInvents(t *testing.T) {
	t.Parallel()
	// The empty string maps to OrderUnspecified, never to a real ordering.
	// Defaulting here would decide O2 by accident.
	if o, ok := parseEvaluationOrder(""); !ok || o != core.OrderUnspecified {
		t.Fatalf(`"" -> %v,%v`, o, ok)
	}
	if o, ok := parseEvaluationOrder("rules_first"); !ok || o != core.OrderRulesFirst {
		t.Fatalf(`"rules_first" -> %v,%v`, o, ok)
	}
	if _, ok := parseEvaluationOrder("rollout_gates_rules"); ok {
		t.Fatal("an unimplemented ordering must not parse")
	}
}

func TestCloneLayersDoNotAlias(t *testing.T) {
	t.Parallel()
	l := mustBase(t, canonicalBase)
	c := l.Clone()
	c.Flags[0].Rules[0].ID = "mutated"
	c.Flags[0].Rules[0].Conditions[0].Values[0] = core.String("mutated")
	c.Flags[0].Rollout.BasisPoints = 9999
	if l.Flags[0].Rules[0].ID != "internal-staff" {
		t.Fatal("clone aliased the rule slice")
	}
	if v, _ := l.Flags[0].Rules[0].Conditions[0].Values[0].AsString(); v != "acme.com" {
		t.Fatal("clone aliased the condition values slice")
	}
	if l.Flags[0].Rollout.BasisPoints != 500 {
		t.Fatal("clone aliased the rollout pointer")
	}
}

package core

import (
	"encoding/json"
	"testing"
)

func TestValueNeverCoercesAcrossTypes(t *testing.T) {
	t.Parallel()
	// A string "true" is not a bool. Silent coercion here is how a flag that
	// reads `enabled: "false"` in YAML ends up switched on in production.
	if _, ok := String("true").AsBool(); ok {
		t.Fatal("String(\"true\").AsBool() succeeded; must not coerce")
	}
	if _, ok := Int(1).AsBool(); ok {
		t.Fatal("Int(1).AsBool() succeeded; must not coerce")
	}
	if _, ok := String("1").AsInt(); ok {
		t.Fatal("String(\"1\").AsInt() succeeded; must not coerce")
	}
}

func TestValueEqualIsTypeSensitive(t *testing.T) {
	t.Parallel()
	if Int(1).Equal(String("1")) {
		t.Fatal("Int(1) must not equal String(\"1\")")
	}
	if Bool(true).Equal(Int(1)) {
		t.Fatal("Bool(true) must not equal Int(1)")
	}
	if !Int(7).Equal(Int(7)) {
		t.Fatal("Int(7) must equal Int(7)")
	}
}

func TestValueJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for _, want := range []Value{Bool(true), Bool(false), String("IN"), String(""), Int(0), Int(-42)} {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %v: %v", want, err)
		}
		var got Value
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if !got.Equal(want) {
			t.Fatalf("round trip: got %v (%s), want %v (%s)", got, got.Type(), want, want.Type())
		}
	}
}

func TestValueRejectsFractionalNumber(t *testing.T) {
	t.Parallel()
	// Truncating 1.5 to 1 in a config file is how a rollout silently lands at the
	// wrong percentage. Reject rather than round.
	var v Value
	if err := json.Unmarshal([]byte("1.5"), &v); err == nil {
		t.Fatal("expected error for fractional number, got none")
	}
}

func TestZeroValueIsUnknownAndSafe(t *testing.T) {
	t.Parallel()
	var v Value
	if !v.IsUnknown() {
		t.Fatal("zero Value must be unknown")
	}
	if v.String() != "<unset>" {
		t.Fatalf("zero Value renders %q", v.String())
	}
}

func TestEvalContextDistinguishesAbsentFromZero(t *testing.T) {
	t.Parallel()
	// This distinction is what stops `country != "IN"` matching everyone when an
	// upstream geo lookup fails and the attribute is simply missing.
	ctx := EvalContext{Attributes: map[string]Value{"country": String("")}}
	if v, ok := ctx.Attribute("country"); !ok || !v.Equal(String("")) {
		t.Fatal("present-but-empty attribute must report ok=true")
	}
	if _, ok := ctx.Attribute("plan"); ok {
		t.Fatal("absent attribute must report ok=false")
	}
}

func TestEvalContextNilAttributesDoesNotPanic(t *testing.T) {
	t.Parallel()
	var ctx EvalContext
	if _, ok := ctx.Attribute("anything"); ok {
		t.Fatal("nil Attributes must report ok=false, not panic")
	}
}

func TestEvalContextShorthandAttributes(t *testing.T) {
	t.Parallel()
	ctx := EvalContext{UserID: "u1", TenantID: "t1"}
	if v, ok := ctx.Attribute("user_id"); !ok || !v.Equal(String("u1")) {
		t.Fatal("user_id shorthand must resolve")
	}
	if v, ok := ctx.Attribute("tenant_id"); !ok || !v.Equal(String("t1")) {
		t.Fatal("tenant_id shorthand must resolve")
	}
	// An explicit entry must win over the shorthand.
	ctx.Attributes = map[string]Value{"user_id": String("override")}
	if v, _ := ctx.Attribute("user_id"); !v.Equal(String("override")) {
		t.Fatal("explicit attribute must override shorthand")
	}
}

func TestReasonFallbackClassification(t *testing.T) {
	t.Parallel()
	fallback := []Reason{ReasonFlagNotFound, ReasonTypeMismatch, ReasonMissingSubject, ReasonError}
	for _, r := range fallback {
		if !r.IsFallback() {
			t.Fatalf("%s must classify as fallback", r)
		}
	}
	configured := []Reason{ReasonRuleMatch, ReasonRolloutIn, ReasonRolloutOut, ReasonFallthrough, ReasonDisabled}
	for _, r := range configured {
		if r.IsFallback() {
			t.Fatalf("%s must not classify as fallback", r)
		}
	}
}

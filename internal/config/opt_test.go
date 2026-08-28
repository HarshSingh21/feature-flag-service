package config

import (
	"encoding/json"
	"testing"
)

type triState struct {
	A Opt[int]    `json:"a,omitzero"`
	B Opt[string] `json:"b,omitzero"`
	C Opt[bool]   `json:"c,omitzero"`
}

func TestOptDistinguishesAbsentNullAndSet(t *testing.T) {
	t.Parallel()
	// The whole reason Opt exists: a pointer field would report nil for both the
	// absent key and the explicit null, collapsing "inherit from the layer below"
	// into "this environment has none at all".
	var got triState
	if err := json.Unmarshal([]byte(`{"a":7,"b":null}`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !got.A.IsValue() || got.A.Val != 7 {
		t.Fatalf("a: want set value 7, got %+v", got.A)
	}
	if v, ok := got.A.Get(); !ok || v != 7 {
		t.Fatalf("a.Get(): want 7,true got %v,%v", v, ok)
	}

	if !got.B.IsNull() {
		t.Fatalf("b: want explicit null, got %+v", got.B)
	}
	if !got.B.Set {
		t.Fatal("b: explicit null must still report Set=true")
	}
	if _, ok := got.B.Get(); ok {
		t.Fatal("b.Get(): explicit null must report ok=false")
	}

	if !got.C.IsAbsent() {
		t.Fatalf("c: want absent, got %+v", got.C)
	}
	if got.C.Set {
		t.Fatal("c: absent key must report Set=false")
	}
}

func TestOptRoundTripPreservesAllThreeStates(t *testing.T) {
	t.Parallel()
	const in = `{"a":7,"b":null}`
	var v triState
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// omitzero drops the absent field; the explicit null survives as null.
	if string(b) != in {
		t.Fatalf("round trip: got %s, want %s", b, in)
	}

	var again triState
	if err := json.Unmarshal(b, &again); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if again.A != v.A || again.B != v.B || again.C != v.C {
		t.Fatalf("second round trip diverged: %+v vs %+v", again, v)
	}
}

func TestOptZeroValueRoundTripsAsSetNotAbsent(t *testing.T) {
	t.Parallel()
	// enabled:false must not be mistaken for "enabled absent". This is the exact
	// failure a plain bool field has.
	var v struct {
		Enabled Opt[bool] `json:"enabled,omitzero"`
	}
	if err := json.Unmarshal([]byte(`{"enabled":false}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !v.Enabled.IsValue() {
		t.Fatalf("enabled:false must be a SET value, got %+v", v.Enabled)
	}
	if got, ok := v.Enabled.Get(); !ok || got {
		t.Fatalf("want false,true got %v,%v", got, ok)
	}
	b, _ := json.Marshal(v)
	if string(b) != `{"enabled":false}` {
		t.Fatalf("marshal: got %s", b)
	}
}

func TestOptHelpers(t *testing.T) {
	t.Parallel()
	if v, ok := Some(3).Get(); !ok || v != 3 {
		t.Fatalf("Some: got %v,%v", v, ok)
	}
	if _, ok := None[int]().Get(); ok {
		t.Fatal("None must report ok=false")
	}
	if _, ok := NullOpt[int]().Get(); ok {
		t.Fatal("NullOpt must report ok=false")
	}
	if !NullOpt[int]().IsNull() || NullOpt[int]().IsAbsent() {
		t.Fatal("NullOpt must be null, not absent")
	}
	if got := None[string]().OrElse("fallback"); got != "fallback" {
		t.Fatalf("OrElse on absent: got %q", got)
	}
	if got := NullOpt[string]().OrElse("fallback"); got != "fallback" {
		t.Fatalf("OrElse on null: got %q", got)
	}
	if got := Some("v").OrElse("fallback"); got != "v" {
		t.Fatalf("OrElse on value: got %q", got)
	}
}

func TestOptNestedStructDecodes(t *testing.T) {
	t.Parallel()
	var v struct {
		R Opt[OverlayRollout] `json:"rollout,omitzero"`
	}
	if err := json.Unmarshal([]byte(`{"rollout":{"basis_points":2500}}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	r, ok := v.R.Get()
	if !ok {
		t.Fatal("rollout must be a set value")
	}
	if bp, ok := r.BasisPoints.Get(); !ok || bp != 2500 {
		t.Fatalf("basis_points: got %v,%v", bp, ok)
	}
	// Everything the overlay did not mention must be ABSENT, not zero -- that is
	// what makes the deep merge preserve bucket_namespace.
	if !r.BucketNamespace.IsAbsent() {
		t.Fatalf("bucket_namespace must be absent, got %+v", r.BucketNamespace)
	}
	if !r.BucketBy.IsAbsent() || !r.OnValue.IsAbsent() || !r.OffValue.IsAbsent() {
		t.Fatal("unmentioned rollout fields must all be absent")
	}
}

// Package config implements the layered configuration subsystem: the wire types
// for the three config layers, the deep-merge pipeline that resolves them into
// core.Flag values, the two-pass validator, the immutable per-environment
// snapshot, and the in-memory store that publishes snapshots atomically.
//
// The read path (Store.Get) performs one atomic pointer load and one map lookup.
// It never merges, never validates, never allocates and never blocks -- every
// merge and every validation happens eagerly on the write path (HLD B.4, LLD 3.5).
package config

import (
	"bytes"
	"encoding/json"
)

// Opt is the tri-state carrier for every sparse-layer field.
//
//	{Set:false}                   -> key ABSENT        -> inherit from the layer below
//	{Set:true, Null:true}         -> explicit NULL     -> unset (composite fields only)
//	{Set:true, Null:false, Val:v} -> explicit VALUE    -> override the layer below
//
// The mechanic is that encoding/json invokes a field's custom UnmarshalJSON only
// when the key is present in the document, and does invoke it for an explicit
// null. So UnmarshalJSON can set Set unconditionally and Null only for a literal
// null, giving three unambiguous states with no author bookkeeping.
//
// A pointer field (*bool) is NOT an acceptable substitute: encoding/json leaves a
// pointer nil both for an absent key and for an explicit null, collapsing exactly
// the two states the merge pipeline has to tell apart -- "inherit the layer below"
// versus "this environment has none at all".
//
// IsZero is implemented so a struct field tagged `json:",omitzero"` round trips:
// an absent Opt marshals back to an absent key rather than to a literal null.
type Opt[T any] struct {
	// Set reports that the key was present in the source document.
	Set bool
	// Null reports that the present key carried an explicit JSON null.
	Null bool
	// Val is the decoded payload. Meaningful only when Set && !Null.
	Val T
}

// Some builds an Opt carrying an explicit value.
func Some[T any](v T) Opt[T] { return Opt[T]{Set: true, Val: v} }

// None builds an absent Opt: "inherit from the layer below".
func None[T any]() Opt[T] { return Opt[T]{} }

// NullOpt builds an explicit-null Opt: "this layer has none at all".
// Legal only on nullable composite fields; on a scalar it is author confusion and
// the validator rejects it (rule O04).
func NullOpt[T any]() Opt[T] { return Opt[T]{Set: true, Null: true} }

// Get returns the carried value. ok is false for both absent and explicit null,
// because neither supplies a value to merge.
func (o Opt[T]) Get() (T, bool) {
	if !o.Set || o.Null {
		var zero T
		return zero, false
	}
	return o.Val, true
}

// OrElse returns the carried value, or def when the Opt is absent or null.
func (o Opt[T]) OrElse(def T) T {
	if v, ok := o.Get(); ok {
		return v
	}
	return def
}

// IsAbsent reports that the key was not present in the source document.
func (o Opt[T]) IsAbsent() bool { return !o.Set }

// IsNull reports that the key was present and carried an explicit null.
func (o Opt[T]) IsNull() bool { return o.Set && o.Null }

// IsValue reports that the key was present and carried a value.
func (o Opt[T]) IsValue() bool { return o.Set && !o.Null }

// IsZero makes `json:",omitzero"` omit absent Opts on marshal, which is what
// makes an absent field survive a round trip as absent rather than as null.
func (o Opt[T]) IsZero() bool { return !o.Set }

// UnmarshalJSON is invoked by encoding/json only when the key is present, which
// is the whole mechanism behind the tri-state.
func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Null = true
		var zero T
		o.Val = zero
		return nil
	}
	o.Null = false
	return json.Unmarshal(b, &o.Val)
}

// MarshalJSON emits the bare payload. Absent Opts are normally omitted by
// `omitzero`; if one is marshalled anyway it emits null, which decodes back as
// explicit-null -- so always pair Opt fields with `omitzero`.
func (o Opt[T]) MarshalJSON() ([]byte, error) {
	if !o.Set || o.Null {
		return []byte("null"), nil
	}
	return json.Marshal(o.Val)
}

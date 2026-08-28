// Package core is ring 0 and ring 1 of the dependency model: the domain types and
// the evaluation engine.
//
// It imports nothing that performs I/O -- no logger, no clock, no network, no disk.
// Errors are returned as data (Result.Reason), never logged or panicked from here.
// That constraint is what makes the never-throw contract fuzzable rather than
// aspirational, and it is enforced by TestNoIOImports.
package core

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ValueType is the declared type of a flag. A flag's type is fixed at config time;
// every value the flag can resolve to must match it.
type ValueType uint8

const (
	TypeUnknown ValueType = iota
	TypeBool
	TypeString
	TypeInt
)

func (t ValueType) String() string {
	switch t {
	case TypeBool:
		return "bool"
	case TypeString:
		return "string"
	case TypeInt:
		return "int"
	default:
		return "unknown"
	}
}

// ParseValueType maps the wire representation to a ValueType.
func ParseValueType(s string) (ValueType, bool) {
	switch s {
	case "bool", "boolean":
		return TypeBool, true
	case "string":
		return TypeString, true
	case "int", "integer":
		return TypeInt, true
	default:
		return TypeUnknown, false
	}
}

// Value is a tagged union over the three supported flag types.
//
// It is deliberately not `any`. At 2.4M evaluations/sec an interface-boxed int
// escapes to the heap on every evaluation; this struct is copied by value and
// allocates nothing. Keep it that way -- adding a pointer or a slice field to
// this struct would put the evaluation hot path back on the allocator.
type Value struct {
	typ ValueType
	b   bool
	i   int64
	s   string
}

func Bool(v bool) Value     { return Value{typ: TypeBool, b: v} }
func String(v string) Value { return Value{typ: TypeString, s: v} }
func Int(v int64) Value     { return Value{typ: TypeInt, i: v} }

// Type reports the value's type. The zero Value has type TypeUnknown.
func (v Value) Type() ValueType { return v.typ }

// IsUnknown reports whether this is the zero Value, i.e. absent rather than set.
func (v Value) IsUnknown() bool { return v.typ == TypeUnknown }

// AsBool returns the bool payload. ok is false if the value is not a bool.
// It never coerces: Value{"true"} is not a bool.
func (v Value) AsBool() (val bool, ok bool) {
	if v.typ != TypeBool {
		return false, false
	}
	return v.b, true
}

func (v Value) AsString() (val string, ok bool) {
	if v.typ != TypeString {
		return "", false
	}
	return v.s, true
}

func (v Value) AsInt() (val int64, ok bool) {
	if v.typ != TypeInt {
		return 0, false
	}
	return v.i, true
}

// Equal reports deep equality including type. Values of different types are never
// equal, so Int(1) != Bool(true) and Int(1) != String("1").
func (v Value) Equal(o Value) bool {
	if v.typ != o.typ {
		return false
	}
	switch v.typ {
	case TypeBool:
		return v.b == o.b
	case TypeString:
		return v.s == o.s
	case TypeInt:
		return v.i == o.i
	default:
		return true // both TypeUnknown
	}
}

// String renders the value for logs and error messages. It is not a wire format.
func (v Value) String() string {
	switch v.typ {
	case TypeBool:
		return strconv.FormatBool(v.b)
	case TypeString:
		return v.s
	case TypeInt:
		return strconv.FormatInt(v.i, 10)
	default:
		return "<unset>"
	}
}

// MarshalJSON emits the bare scalar, so a config file reads naturally:
// `"default_value": true` rather than `{"type":"bool","value":true}`.
func (v Value) MarshalJSON() ([]byte, error) {
	switch v.typ {
	case TypeBool:
		return json.Marshal(v.b)
	case TypeString:
		return json.Marshal(v.s)
	case TypeInt:
		return json.Marshal(v.i)
	default:
		return []byte("null"), nil
	}
}

// UnmarshalJSON infers the type from the JSON scalar.
//
// A JSON number carrying a fractional part is rejected rather than truncated:
// silently turning 1.5 into 1 in a config file is how a rollout ends up at the
// wrong percentage with nothing in the logs.
func (v *Value) UnmarshalJSON(data []byte) error {
	var raw any
	dec := json.NewDecoder(newBytesReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	switch t := raw.(type) {
	case nil:
		*v = Value{}
		return nil
	case bool:
		*v = Bool(t)
		return nil
	case string:
		*v = String(t)
		return nil
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return fmt.Errorf("core: value %s is not an integer: %w", t.String(), err)
		}
		*v = Int(n)
		return nil
	default:
		return fmt.Errorf("core: unsupported value type %T", raw)
	}
}

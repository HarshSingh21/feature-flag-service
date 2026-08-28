package core

import (
	"encoding/json"
	"fmt"
	"testing"
	"unsafe"
)

func TestSizes(t *testing.T) {
	fmt.Println("sizeof Value      =", unsafe.Sizeof(Value{}))
	fmt.Println("align Value       =", unsafe.Alignof(Value{}))
	fmt.Println("offset typ        =", unsafe.Offsetof(Value{}.typ))
	fmt.Println("offset b          =", unsafe.Offsetof(Value{}.b))
	fmt.Println("offset i          =", unsafe.Offsetof(Value{}.i))
	fmt.Println("offset s          =", unsafe.Offsetof(Value{}.s))
	fmt.Println("sizeof EvalContext=", unsafe.Sizeof(EvalContext{}))
	fmt.Println("sizeof Flag       =", unsafe.Sizeof(Flag{}))
	fmt.Println("sizeof Rule       =", unsafe.Sizeof(Rule{}))
	fmt.Println("sizeof Condition  =", unsafe.Sizeof(Condition{}))
	fmt.Println("sizeof Result     =", unsafe.Sizeof(Result{}))
	fmt.Println("sizeof Rollout    =", unsafe.Sizeof(Rollout{}))
}

var sinkV Value
var sinkB bool

func BenchmarkAttributeHit(b *testing.B) {
	ctx := EvalContext{UserID: "u-123456", TenantID: "t-42", Attributes: map[string]Value{
		"country": String("IN"), "plan": String("pro"), "app_version": String("4.2.1"),
	}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sinkV, sinkB = ctx.Attribute("country")
	}
}

func BenchmarkAttributeShorthandUserID(b *testing.B) {
	ctx := EvalContext{UserID: "u-123456", TenantID: "t-42", Attributes: map[string]Value{
		"country": String("IN"), "plan": String("pro"), "app_version": String("4.2.1"),
	}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sinkV, sinkB = ctx.Attribute("user_id")
	}
}

func BenchmarkAttributeMiss(b *testing.B) {
	ctx := EvalContext{UserID: "u-123456", Attributes: map[string]Value{
		"country": String("IN"), "plan": String("pro"), "app_version": String("4.2.1"),
	}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sinkV, sinkB = ctx.Attribute("nope")
	}
}

func BenchmarkValueEqual(b *testing.B) {
	x, y := String("some-moderately-long-attribute-value"), String("some-moderately-long-attribute-value")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sinkB = x.Equal(y)
	}
}

func BenchmarkValueUnmarshalInt(b *testing.B) {
	data := []byte("12345")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var v Value
		if err := v.UnmarshalJSON(data); err != nil {
			b.Fatal(err)
		}
		sinkV = v
	}
}

func BenchmarkValueUnmarshalString(b *testing.B) {
	data := []byte(`"hello"`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var v Value
		if err := v.UnmarshalJSON(data); err != nil {
			b.Fatal(err)
		}
		sinkV = v
	}
}

func BenchmarkStdUnmarshalIntoAny(b *testing.B) {
	data := []byte("12345")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var raw any
		if err := json.Unmarshal(data, &raw); err != nil {
			b.Fatal(err)
		}
	}
}

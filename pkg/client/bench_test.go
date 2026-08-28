package client

import (
	"context"
	"strconv"
	"testing"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// benchSnapshot is a corpus shaped like a real environment: mostly plain flags,
// a slice with targeting rules, and a slice with percentage rollouts.
func benchSnapshot(n int) *MemSnapshot {
	flags := make([]core.Flag, 0, n)
	for i := 0; i < n; i++ {
		key := "flag_" + strconv.Itoa(i)
		switch i % 5 {
		case 0:
			flags = append(flags, core.Flag{
				Key: key, Type: core.TypeBool, Enabled: true,
				DefaultValue: core.Bool(false), OffValue: core.Bool(false),
				Rollout: &core.Rollout{BasisPoints: 5000, OnValue: core.Bool(true), OffValue: core.Bool(false)},
			})
		case 1:
			flags = append(flags, core.Flag{
				Key: key, Type: core.TypeBool, Enabled: true,
				DefaultValue: core.Bool(false), OffValue: core.Bool(false),
				EvaluationOrder: core.OrderRulesFirst,
				Rules: []core.Rule{
					{ID: "r1", Combiner: core.LogicAnd, Value: core.Bool(true), Conditions: []core.Condition{
						{Attribute: "country", Op: core.OpIn, Values: []core.Value{core.String("DE"), core.String("FR")}},
					}},
					{ID: "r2", Combiner: core.LogicAnd, Value: core.Bool(true), Conditions: []core.Condition{
						{Attribute: "plan", Op: core.OpEquals, Values: []core.Value{core.String("enterprise")}},
						{Attribute: "country", Op: core.OpIn, Values: []core.Value{core.String("IN"), core.String("US")}},
					}},
				},
			})
		default:
			flags = append(flags, boolFlag(key, true))
		}
	}
	return NewMemSnapshot("prod", 42, flags)
}

func benchClient(b *testing.B, n int) *Client {
	b.Helper()
	c, err := New(WithEnvironment("prod"))
	if err != nil {
		b.Fatal(err)
	}
	c.cache.apply(&entry{snap: benchSnapshot(n), gen: 42, instanceID: "inst-a"})
	c.sm.set(StateHealthy, "bench")
	return c
}

func benchContext() core.EvalContext {
	return core.EvalContext{
		UserID:   "user-8f3c1d9e",
		TenantID: "tenant-42",
		Attributes: map[string]core.Value{
			"country": core.String("IN"),
			"plan":    core.String("enterprise"),
		},
	}
}

// BenchmarkBoolValue is the reference cost of one evaluation on a cached
// snapshot: the number docs/03-lld.md §2 claims is ~0.3 µs.
func BenchmarkBoolValue(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	ec := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !c.BoolValue(ctx, "flag_2", false, ec) {
			b.Fatal("unexpected value")
		}
	}
}

// BenchmarkBoolValueTargeted evaluates a flag with two rules and four
// conditions, the shape the §4.1 pathological corner is built from.
func BenchmarkBoolValueTargeted(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	ec := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.BoolValue(ctx, "flag_1", false, ec)
	}
}

// BenchmarkBoolValueRollout includes the bucketing hash.
func BenchmarkBoolValueRollout(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	ec := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.BoolValue(ctx, "flag_0", false, ec)
	}
}

func BenchmarkBoolDetail(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	ec := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.BoolDetail(ctx, "flag_2", false, ec)
	}
}

// BenchmarkFlagNotFound is the cost of the answer, not of a miss: there is no
// fetch behind it.
func BenchmarkFlagNotFound(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	ec := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.BoolValue(ctx, "no_such_flag", true, ec)
	}
}

// BenchmarkUninitialized is the cost of the failure mode, which must be cheap:
// during a total outage every evaluation in the fleet takes this path.
func BenchmarkUninitialized(b *testing.B) {
	c, err := New(WithEnvironment("prod"))
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	ec := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.BoolValue(ctx, "flag_2", true, ec)
	}
}

func benchRequests(n int) []Request {
	ec := benchContext()
	reqs := make([]Request, n)
	for i := range reqs {
		reqs[i] = Request{Flag: "flag_" + strconv.Itoa(i), Default: core.Bool(false), EvalContext: ec}
	}
	return reqs
}

// BenchmarkBatch100 is THE number: the p99 request shape of docs/03-lld.md §1,
// 100 flags in one call, against the sub-millisecond budget of S3.
func BenchmarkBatch100(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	reqs := benchRequests(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := c.Batch(ctx, reqs)
		if len(out) != 100 {
			b.Fatal("short batch")
		}
	}
}

// BenchmarkBatch30 is the typical request shape.
func BenchmarkBatch30(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	reqs := benchRequests(30)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Batch(ctx, reqs)
	}
}

// BenchmarkBatch100Append shows what pooling the result slice buys.
func BenchmarkBatch100Append(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	reqs := benchRequests(100)
	buf := make([]core.Result, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = c.BatchAppend(ctx, buf, reqs)
	}
}

// BenchmarkSingle100 is the same 100 flags as 100 separate calls, which is what
// the batch API exists to replace. The delta is the amortised entry boundary.
func BenchmarkSingle100(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	ec := benchContext()
	keys := make([]string, 100)
	for i := range keys {
		keys[i] = "flag_" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, k := range keys {
			c.BoolValue(ctx, k, false, ec)
		}
	}
}

// BenchmarkBatch100Parallel runs the p99 shape across all cores, which is the
// regime the 2.4M evaluations/sec figure actually describes.
func BenchmarkBatch100Parallel(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		reqs := benchRequests(100)
		buf := make([]core.Result, 100)
		for pb.Next() {
			buf = c.BatchAppend(ctx, buf, reqs)
		}
	})
}

func BenchmarkBoolValueParallel(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ec := benchContext()
		for pb.Next() {
			c.BoolValue(ctx, "flag_2", false, ec)
		}
	})
}

// BenchmarkBoolValueDuringSwap measures the read path while the config plane is
// swapping snapshots underneath it, which is where a mutex-based cache would
// show up as contention and the atomic pointer does not.
func BenchmarkBoolValueDuringSwap(b *testing.B) {
	c := benchClient(b, 5000)
	defer c.Close()
	ctx := context.Background()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		snap := benchSnapshot(5000)
		for gen := int64(43); ; gen++ {
			select {
			case <-stop:
				return
			default:
			}
			c.cache.apply(&entry{snap: snap, gen: gen, instanceID: "inst-a"})
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ec := benchContext()
		for pb.Next() {
			c.BoolValue(ctx, "flag_2", false, ec)
		}
	})
	b.StopTimer()
	close(stop)
	<-done
}

// nullEvaluator isolates SDK overhead -- recover frame, atomic load, map
// lookup, type check -- from the cost of evaluation itself.
type nullEvaluator struct{}

func (nullEvaluator) Evaluate(snap core.Snapshot, key string, _ core.EvalContext, _ core.ValueType, def core.Value) core.Result {
	f, ok := snap.Flag(key)
	if !ok {
		return core.Result{Value: def, Reason: core.ReasonFlagNotFound, Bucket: core.NoBucket, Generation: snap.Generation()}
	}
	return core.Result{Value: f.DefaultValue, Reason: core.ReasonFallthrough, Bucket: core.NoBucket, Generation: snap.Generation()}
}

func BenchmarkSDKOverheadSingle(b *testing.B) {
	c, err := New(WithEnvironment("prod"), WithEvaluator(nullEvaluator{}))
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	c.cache.apply(&entry{snap: benchSnapshot(5000), gen: 42})
	c.sm.set(StateHealthy, "bench")
	ctx := context.Background()
	ec := benchContext()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.BoolValue(ctx, "flag_2", false, ec)
	}
}

func BenchmarkSDKOverheadBatch100(b *testing.B) {
	c, err := New(WithEnvironment("prod"), WithEvaluator(nullEvaluator{}))
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	c.cache.apply(&entry{snap: benchSnapshot(5000), gen: 42})
	c.sm.set(StateHealthy, "bench")
	ctx := context.Background()
	reqs := benchRequests(100)
	buf := make([]core.Result, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = c.BatchAppend(ctx, buf, reqs)
	}
}

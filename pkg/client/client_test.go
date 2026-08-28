package client

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

func TestNewDefaultsToTheShippedEngine(t *testing.T) {
	t.Parallel()
	c, err := New(WithEnvironment("prod"))
	if err != nil {
		t.Fatalf("New with no options must work: %v", err)
	}
	defer c.Close()
	// The default engine is the one the service links, which is what makes
	// client/server divergence structurally impossible (docs/03-lld.md §3.3).
	if c.ev == nil {
		t.Fatal("New must default the evaluator to the shipped engine")
	}
}

func TestTypedAccessorsReturnConfiguredValues(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 7)
	ctx := context.Background()
	ec := core.EvalContext{UserID: "u1"}

	if got := c.BoolValue(ctx, "checkout_v2", false, ec); got != true {
		t.Fatalf("BoolValue = %v, want true", got)
	}
	if got := c.StringValue(ctx, "theme", "light", ec); got != "dark" {
		t.Fatalf("StringValue = %q, want dark", got)
	}
	if got := c.IntValue(ctx, "max_items", 1, ec); got != 42 {
		t.Fatalf("IntValue = %d, want 42", got)
	}

	d := c.BoolDetail(ctx, "checkout_v2", false, ec)
	if d.Reason != core.ReasonFallthrough || d.Generation != 7 {
		t.Fatalf("BoolDetail = %+v, want FALLTHROUGH at generation 7", d)
	}
	if d.Bucket != core.NoBucket {
		t.Fatalf("Bucket = %d, want NoBucket for a flag with no rollout", d.Bucket)
	}
}

func TestDetailCarriesRuleAndBucket(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 3)
	ctx := context.Background()

	d := c.StringDetail(ctx, "targeted", "fallback", core.EvalContext{
		UserID:     "u1",
		Attributes: map[string]core.Value{"country": core.String("IN")},
	})
	if d.Reason != core.ReasonRuleMatch || d.RuleID != "r-country-in" {
		t.Fatalf("targeted detail = %+v, want RULE_MATCH r-country-in", d)
	}
	if v, _ := d.Value.AsString(); v != "treatment" {
		t.Fatalf("targeted value = %q, want treatment", v)
	}

	r := c.BoolDetail(ctx, "rolled_out", false, core.EvalContext{UserID: "u1"})
	if r.Reason != core.ReasonRolloutIn && r.Reason != core.ReasonRolloutOut {
		t.Fatalf("rollout reason = %s, want ROLLOUT_IN or ROLLOUT_OUT", r.Reason)
	}
	if r.Bucket < 0 || r.Bucket >= core.BucketSpace {
		t.Fatalf("bucket %d out of range", r.Bucket)
	}
}

func TestUnknownFlagIsAnAnswerNotAMiss(t *testing.T) {
	t.Parallel()
	src := newFakeSource() // configured with no Fetch: any read-path fetch would error
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 1)

	d := c.BoolDetail(context.Background(), "never_shipped", true, core.EvalContext{UserID: "u"})
	if d.Reason != core.ReasonFlagNotFound {
		t.Fatalf("reason = %s, want FLAG_NOT_FOUND", d.Reason)
	}
	if v, _ := d.Value.AsBool(); v != true {
		t.Fatal("unknown flag must return the caller default")
	}
	if d.Generation != 1 {
		t.Fatalf("generation = %d, want the pinned snapshot's generation", d.Generation)
	}
	if f, _ := src.counts(); f != 0 {
		t.Fatalf("read path performed %d fetches; it must perform none (CACHE-3)", f)
	}
}

func TestUninitializedReturnsCallerDefault(t *testing.T) {
	t.Parallel()
	met := newRecMetrics()
	c := newTestClient(t, refEvaluator(), WithMetrics(met))
	defer c.Close()

	if c.State() != StateUninitialized {
		t.Fatalf("state = %s, want UNINITIALIZED", c.State())
	}
	if c.Ready() {
		t.Fatal("/ready must be false in UNINITIALIZED")
	}
	ctx := context.Background()
	if got := c.BoolValue(ctx, "checkout_v2", true, core.EvalContext{}); got != true {
		t.Fatal("uninitialized BoolValue must return the caller default")
	}
	if got := c.StringValue(ctx, "theme", "light", core.EvalContext{}); got != "light" {
		t.Fatal("uninitialized StringValue must return the caller default")
	}
	if got := c.IntValue(ctx, "max_items", 9, core.EvalContext{}); got != 9 {
		t.Fatal("uninitialized IntValue must return the caller default")
	}
	d := c.BoolDetail(ctx, "checkout_v2", true, core.EvalContext{})
	if !d.Reason.IsFallback() {
		t.Fatalf("reason %s must classify as a fallback so H1 can alert on it", d.Reason)
	}
	if met.uninitCount() != 4 {
		t.Fatalf("uninitialized counter = %d, want 4; this is the alarm", met.uninitCount())
	}
}

func TestBatchReturnsDefaultsWhenUninitialized(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	out := c.Batch(context.Background(), []Request{
		{Flag: "checkout_v2", Default: core.Bool(true)},
		{Flag: "theme", Default: core.String("light")},
	})
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if v, _ := out[0].Value.AsBool(); v != true {
		t.Fatal("batch element 0 must carry the caller default")
	}
	if v, _ := out[1].Value.AsString(); v != "light" {
		t.Fatal("batch element 1 must carry the caller default")
	}
}

func TestTypeMismatchReturnsCallerDefault(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 1)
	ctx := context.Background()

	// "theme" is a string flag read through the bool accessor.
	d := c.BoolDetail(ctx, "theme", true, core.EvalContext{UserID: "u"})
	if d.Reason != core.ReasonTypeMismatch {
		t.Fatalf("reason = %s, want TYPE_MISMATCH", d.Reason)
	}
	if v, ok := d.Value.AsBool(); !ok || v != true {
		t.Fatal("type mismatch must return the caller default, not a coerced value")
	}
	if got := c.IntValue(ctx, "theme", 5, core.EvalContext{}); got != 5 {
		t.Fatalf("IntValue on a string flag = %d, want the default 5", got)
	}
}

func TestDisabledFlagReturnsOffValueNotDefault(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 1)
	d := c.BoolDetail(context.Background(), "disabled_flag", true, core.EvalContext{UserID: "u"})
	if d.Reason != core.ReasonDisabled {
		t.Fatalf("reason = %s, want DISABLED", d.Reason)
	}
	if v, _ := d.Value.AsBool(); v != false {
		t.Fatal("a disabled flag resolves to its off value, which is a configured answer")
	}
	if d.IsFallback() {
		t.Fatal("DISABLED is a configured answer and must not count as a fallback")
	}
}

func TestNilEvalContextAttributes(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 1)
	ctx := context.Background()

	// A zero EvalContext: nil map, no user, no tenant. Must not panic anywhere.
	var ec core.EvalContext
	if got := c.StringValue(ctx, "targeted", "fallback", ec); got != "control" {
		t.Fatalf("targeted with nil attributes = %q, want control", got)
	}
	d := c.BoolDetail(ctx, "rolled_out", false, ec)
	if d.Reason != core.ReasonMissingSubject {
		t.Fatalf("reason = %s, want MISSING_SUBJECT for a rollout with no subject", d.Reason)
	}
	if v, _ := d.Value.AsBool(); v != false {
		t.Fatal("MISSING_SUBJECT is a fallback and must return the caller default")
	}
	out := c.Batch(ctx, []Request{
		{Flag: "targeted", Default: core.String("x")},
		{Flag: "rolled_out", Default: core.Bool(true)},
	})
	if out[0].Reason != core.ReasonFallthrough {
		t.Fatalf("batch nil-context reason = %s", out[0].Reason)
	}
	if v, _ := out[1].Value.AsBool(); v != true {
		t.Fatal("batch MISSING_SUBJECT must return the caller default")
	}
}

func TestPanickingEvaluatorIsContained(t *testing.T) {
	t.Parallel()
	met := newRecMetrics()
	c := newTestClient(t, &panicEvaluator{}, WithMetrics(met))
	defer c.Close()
	applyFixture(c, 4)
	ctx := context.Background()

	if got := c.BoolValue(ctx, "checkout_v2", true, core.EvalContext{UserID: "u"}); got != true {
		t.Fatal("a panicking evaluator must still yield the caller default")
	}
	if got := c.StringValue(ctx, "theme", "light", core.EvalContext{}); got != "light" {
		t.Fatal("a panicking evaluator must still yield the caller default")
	}
	if got := c.IntValue(ctx, "max_items", 11, core.EvalContext{}); got != 11 {
		t.Fatal("a panicking evaluator must still yield the caller default")
	}
	d := c.BoolDetail(ctx, "checkout_v2", true, core.EvalContext{})
	if d.Reason != core.ReasonError {
		t.Fatalf("reason = %s, want ERROR", d.Reason)
	}
	if met.evalCount(core.ReasonError) == 0 {
		t.Fatal("a recovered panic must be counted, not swallowed")
	}
}

func TestPanicInOneBatchElementDoesNotPoisonTheRest(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, &panicEvaluator{on: "theme"})
	defer c.Close()
	applyFixture(c, 4)

	out := c.Batch(context.Background(), []Request{
		{Flag: "checkout_v2", Default: core.Bool(false)},
		{Flag: "theme", Default: core.String("light")},
		{Flag: "max_items", Default: core.Int(1)},
	})
	if v, _ := out[0].Value.AsBool(); v != true {
		t.Fatal("element before the panicking one must keep its configured value")
	}
	if out[1].Reason != core.ReasonError {
		t.Fatalf("panicking element reason = %s, want ERROR", out[1].Reason)
	}
	if v, _ := out[1].Value.AsString(); v != "light" {
		t.Fatal("panicking element must carry the caller default")
	}
	if v, _ := out[2].Value.AsInt(); v != 42 {
		t.Fatalf("element after the panicking one = %d, want its configured value 42", v)
	}
}

func TestPanickingHooksDoNotCorruptAGoodResult(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator(), WithMetrics(explodingMetrics{}),
		WithLogger(LoggerFunc(func(context.Context, Level, string, ...any) { panic("logger exploded") })))
	defer c.Close()
	applyFixture(c, 2)
	if got := c.BoolValue(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"}); got != true {
		t.Fatal("a panicking metrics hook must not downgrade a correct evaluation to the default")
	}
}

type explodingMetrics struct{ NopMetrics }

func (explodingMetrics) Evaluation(string, core.Reason) { panic("metrics exploded") }

// allExplodingMetrics panics from every hook, including the ones the client
// calls from *inside* its own recover handlers. It deliberately does not embed
// NopMetrics: the point is that no hook is safe.
type allExplodingMetrics struct{}

func (allExplodingMetrics) Evaluation(string, core.Reason) { panic("Evaluation exploded") }
func (allExplodingMetrics) UninitializedEvaluation(string) { panic("UninitializedEvaluation exploded") }
func (allExplodingMetrics) StateChanged(State, State)      { panic("StateChanged exploded") }
func (allExplodingMetrics) Generation(int64)               { panic("Generation exploded") }
func (allExplodingMetrics) Connected(bool)                 { panic("Connected exploded") }
func (allExplodingMetrics) Staleness(float64)              { panic("Staleness exploded") }
func (allExplodingMetrics) Resync(string)                  { panic("Resync exploded") }
func (allExplodingMetrics) L2Write(error)                  { panic("L2Write exploded") }

// TestPanicInsideARecoverHandlerStillYieldsTheDefault is the regression test for
// the guardedMetrics recover boundary, and the only thing standing between this
// package and a silent loss of the never-throw contract.
//
// The failure mode is specific. The evaluator panics; detail's deferred handler
// recovers it and calls onPanic; onPanic calls a caller-supplied metrics hook,
// which panics too. That second panic is raised *inside* the deferred function,
// so no recover further up the stack can ever see it — it escapes detail
// entirely and reaches the application as a crash. It is contained only because
// each guardedMetrics method calls recover directly from its own deferred
// literal, which is the one and only form the language honours: rewriting that
// literal as `defer func() { sharedHelper() }()` puts recover a frame too deep,
// returns nil, and re-opens exactly this hole while still looking correct. If
// that ever happens, this test is what fails.
func TestPanicInsideARecoverHandlerStillYieldsTheDefault(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, &panicEvaluator{}, WithMetrics(allExplodingMetrics{}),
		WithLogger(LoggerFunc(func(context.Context, Level, string, ...any) { panic("logger exploded") })))
	ctx := context.Background()

	// Uninitialized: no snapshot, so UninitializedEvaluation panics on the way
	// out of the evaluation.
	if got := c.BoolValue(ctx, "checkout_v2", true, core.EvalContext{UserID: "u"}); !got {
		t.Fatal("uninitialized evaluation with a panicking hook must return the caller default")
	}

	// Applying a fixture drives a state transition, so StateChanged panics too.
	applyFixture(c, 4)

	if got := c.BoolValue(ctx, "checkout_v2", true, core.EvalContext{UserID: "u"}); !got {
		t.Fatal("a panicking evaluator plus a panicking hook must still return the caller default")
	}
	if got := c.StringValue(ctx, "theme", "light", core.EvalContext{}); got != "light" {
		t.Fatal("a panicking evaluator plus a panicking hook must still return the caller default")
	}
	if got := c.IntValue(ctx, "max_items", 11, core.EvalContext{}); got != 11 {
		t.Fatal("a panicking evaluator plus a panicking hook must still return the caller default")
	}
	d := c.BoolDetail(ctx, "checkout_v2", true, core.EvalContext{})
	if d.Reason != core.ReasonError {
		t.Fatalf("reason = %s, want ERROR", d.Reason)
	}
	if v, ok := d.Value.AsBool(); !ok || !v {
		t.Fatal("detail must carry the caller default after a recovered panic")
	}

	// The batch path has the same shape: a per-element recover handler that
	// calls the same hooks.
	out := c.Batch(ctx, []Request{
		{Flag: "theme", Default: core.String("light")},
		{Flag: "max_items", Default: core.Int(11)},
	})
	if v, _ := out[0].Value.AsString(); v != "light" {
		t.Fatal("batch element must carry the caller default after a recovered panic")
	}
	if v, _ := out[1].Value.AsInt(); v != 11 {
		t.Fatal("batch element must carry the caller default after a recovered panic")
	}

	// Close transitions the state machine, so the hook panics once more on the
	// way down; a shutdown must not throw either.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := c.BoolValue(ctx, "checkout_v2", true, core.EvalContext{}); !got {
		t.Fatal("evaluation after Close with panicking hooks must return the caller default")
	}
}

// TestBatchPinsOneSnapshotAcrossAConcurrentSwap is invariant CACHE-1.
//
// A swapper goroutine advances the generation continuously while batches of 64
// flags are evaluated. Every result within one batch must report the same
// generation: if the client loaded the pointer per flag instead of once per
// batch, a swap landing mid-batch would produce a result set spanning two
// generations, which is the cross-flag inconsistency the invariant exists to
// prevent. Run under -race.
func TestBatchPinsOneSnapshotAcrossAConcurrentSwap(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, slowEvaluator{})
	defer c.Close()
	applyFixture(c, 1)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for gen := int64(2); !stop.Load(); gen++ {
			c.cache.apply(&entry{snap: fixture(gen), gen: gen, instanceID: "inst-1", appliedAt: time.Now()})
		}
	}()

	reqs := make([]Request, 64)
	for i := range reqs {
		reqs[i] = Request{Flag: "checkout_v2", Default: core.Bool(false), EvalContext: core.EvalContext{UserID: "u"}}
	}

	var readers sync.WaitGroup
	fail := make(chan string, 8)
	for w := 0; w < 4; w++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 200; i++ {
				out := c.Batch(context.Background(), reqs)
				gen := out[0].Generation
				for _, r := range out {
					if r.Generation != gen {
						select {
						case fail <- "batch spanned generations":
						default:
						}
						return
					}
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	wg.Wait()

	select {
	case msg := <-fail:
		t.Fatal(msg + ": the snapshot must be pinned once per batch (CACHE-1)")
	default:
	}
}

// slowEvaluator widens the window in which a swap can land mid-batch.
type slowEvaluator struct{}

func (slowEvaluator) Evaluate(snap core.Snapshot, key string, ec core.EvalContext, want core.ValueType, def core.Value) core.Result {
	for i := 0; i < 32; i++ {
		_ = bucketOf(key)
	}
	return core.New().Evaluate(snap, key, ec, want, def)
}

func TestSingleFlagReadsAreRaceFreeUnderSwap(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 1)

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for gen := int64(2); !stop.Load(); gen++ {
			c.cache.apply(&entry{snap: fixture(gen), gen: gen, appliedAt: time.Now()})
		}
	}()
	var readers sync.WaitGroup
	for w := 0; w < 8; w++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 2000; i++ {
				if !c.BoolValue(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"}) {
					t.Error("configured value lost during a swap")
					return
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	wg.Wait()
}

func TestBatchAppendReusesCallerSlice(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 1)
	buf := make([]core.Result, 0, 8)
	reqs := []Request{{Flag: "checkout_v2", Default: core.Bool(false)}, {Flag: "theme", Default: core.String("x")}}
	out := c.BatchAppend(context.Background(), buf, reqs)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if &out[0] != &buf[:1][0] {
		t.Fatal("BatchAppend must write into the caller's buffer when capacity allows")
	}
}

func TestFallbackLoggingIsRateLimited(t *testing.T) {
	t.Parallel()
	var lines atomic.Int64
	c := newTestClient(t, refEvaluator(),
		WithLogger(LoggerFunc(func(_ context.Context, _ Level, event string, _ ...any) {
			if strings.HasPrefix(event, "flag.evaluation.") {
				lines.Add(1)
			}
		})))
	defer c.Close()
	applyFixture(c, 1)
	for i := 0; i < 20000; i++ {
		c.BoolValue(context.Background(), "never_shipped", false, core.EvalContext{UserID: "u"})
	}
	// One misconfigured flag at full request rate must not become one log line
	// per evaluation; that is a second-order outage worse than the flag bug.
	if n := lines.Load(); n > 5 {
		t.Fatalf("emitted %d fallback lines for 20000 evaluations; rate limiting is not working", n)
	}
	if lines.Load() == 0 {
		t.Fatal("fallbacks must be reported at least once")
	}
}

func TestLogLimiterReportsSuppressedCount(t *testing.T) {
	t.Parallel()
	l := newLogLimiter(time.Second)
	now := time.Now().UnixNano()
	if n, ok := l.allow("f", core.ReasonError, now); !ok || n != 1 {
		t.Fatalf("first allow = (%d, %v), want (1, true)", n, ok)
	}
	for i := 0; i < 9; i++ {
		if _, ok := l.allow("f", core.ReasonError, now); ok {
			t.Fatal("second emit inside the interval must be suppressed")
		}
	}
	n, ok := l.allow("f", core.ReasonError, now+int64(2*time.Second))
	if !ok || n != 10 {
		t.Fatalf("after the interval = (%d, %v), want (10, true) so true volume is reconstructible", n, ok)
	}
}

func TestCloseIsIdempotentAndEvaluationsStaySafe(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	applyFixture(c, 1)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.State() != StateClosed {
		t.Fatalf("state = %s, want CLOSED", c.State())
	}
	if got := c.BoolValue(context.Background(), "checkout_v2", false, core.EvalContext{UserID: "u"}); !got {
		t.Fatal("evaluation after Close must not panic")
	}
}

func TestContextCancellationDoesNotChangeTheAnswer(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, refEvaluator())
	defer c.Close()
	applyFixture(c, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Cancelling a request must not turn a configured flag into its default:
	// the read path does no I/O, so there is nothing for cancellation to abort.
	if !c.BoolValue(ctx, "checkout_v2", false, core.EvalContext{UserID: "u"}) {
		t.Fatal("a cancelled context must not change the value of a flag")
	}
}

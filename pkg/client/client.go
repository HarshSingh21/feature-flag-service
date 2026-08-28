package client

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// Evaluator is the flag evaluation engine, declared here as the narrow
// interface this package consumes.
//
// There is exactly one implementation, in internal/core, and it is compiled
// into both the service binary and this client. That is the structural answer
// to the objection in docs/03-lld.md §3.3: a client-side cache means a
// client-side evaluator, and two evaluators that must agree will eventually
// disagree — so there is no second evaluator to diverge. This interface is a
// seam for injection and testing, not an invitation to write another engine.
//
// The signature is core.Evaluator's, so *core.Evaluator satisfies it directly
// and the client never has to reimplement any part of evaluation policy —
// lookup, the requested-type check, the caller-default substitution and the
// never-throw boundary all live in the engine, once.
//
// Implementations must be pure and safe for concurrent use: no I/O, no locks,
// no mutation of snap or ctx. A panic here is contained by the client, but it
// costs the caller a correct answer, so it is a bug, not a mode of operation.
type Evaluator interface {
	Evaluate(snap core.Snapshot, key string, ec core.EvalContext, want core.ValueType, callerDefault core.Value) core.Result
}

// Request is one flag in a batch.
type Request struct {
	// Flag is the flag key.
	Flag string

	// Default is the call-site default, returned whenever evaluation cannot
	// produce a configured value of the right type. It also declares the type
	// the caller expects: a Default of core.Bool means a non-bool result is a
	// ReasonTypeMismatch rather than a surprise.
	Default core.Value

	// EvalContext describes the subject. Usually the same value for every entry
	// in a batch; it is per-request so a caller evaluating on behalf of several
	// subjects in one pass does not need several batches.
	EvalContext core.EvalContext
}

// Level is the severity of a client log event.
type Level uint8

const (
	LevelInfo Level = iota
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

// Logger receives client lifecycle and fallback events. attrs are alternating
// key/value pairs, slog-style.
//
// The evaluation context is never passed to a logger. It is arbitrary
// caller-supplied data and will contain PII; only attribute *names* are ever
// emitted, per docs/02-hld.md §D.6.
type Logger interface {
	Log(ctx context.Context, lvl Level, event string, attrs ...any)
}

// LoggerFunc adapts a function to Logger.
type LoggerFunc func(ctx context.Context, lvl Level, event string, attrs ...any)

func (f LoggerFunc) Log(ctx context.Context, lvl Level, event string, attrs ...any) {
	f(ctx, lvl, event, attrs...)
}

// Metrics receives the client-side signals of docs/02-hld.md §D.6.
//
// Embed NopMetrics to implement only what you care about and stay
// forward-compatible when a signal is added.
//
// Cardinality is the caller's responsibility and the trap is well known: flag
// and reason are bounded and safe as labels; user id, tenant id and bucket
// value are not, and one of them added during an incident by someone who just
// wants to see which users got the treatment will take the metrics backend
// down before the flag service notices.
type Metrics interface {
	// Evaluation is called once per evaluation, including batch elements. It is
	// on the hot path at up to 2.4M calls/sec: it must not allocate, lock, or
	// block. It is not called at all unless WithMetrics was supplied.
	Evaluation(flag string, reason core.Reason)

	// UninitializedEvaluation counts evaluations served before any snapshot
	// existed. Sustained non-zero means a pod is serving compiled-in defaults.
	UninitializedEvaluation(flag string)

	// StateChanged reports a client state machine transition.
	StateChanged(from, to State)

	// Generation reports the generation of a newly applied snapshot.
	Generation(gen int64)

	// Connected reports push-stream connectivity.
	Connected(connected bool)

	// Staleness reports seconds since the last frame from the source.
	Staleness(seconds float64)

	// Resync counts recoveries, labelled by cause: heartbeat_gap,
	// instance_changed, stream_dead, generation_gap, reconcile_diff.
	Resync(reason string)

	// L2Write counts last-known-good disk writes by outcome.
	L2Write(err error)
}

// guardedMetrics contains a panicking hook.
//
// Hooks are caller-supplied code. Off the evaluation path they run on the
// updater goroutine, where an unrecovered panic kills the process; and inside
// the evaluation path's recover handler, where a second panic escapes the
// boundary entirely and defeats the never-throw contract. Both are real: the
// second was caught by TestPanickingHooksDoNotCorruptAGoodResult.
//
// The hot per-evaluation counter deliberately bypasses this wrapper, because it
// is already inside a recover boundary and does not need a second defer at
// 2.4M calls/sec.
//
// Every method below repeats `defer func() { _ = recover() }()` verbatim, and
// that duplication is deliberate rather than an oversight. recover reports a
// panic only when it is called *directly* by the deferred function, so factoring
// the line into a shared helper is a loaded gun: `defer helper()` does work, but
// the moment anyone rewrites it as `defer func() { helper() }()` — which
// compiles, reads correctly and passes review — recover sits one frame too deep,
// returns nil, and the panic carries on unwinding. Because these hooks also run
// inside the evaluation path's own recover handler, such a panic escapes the
// never-throw boundary entirely. Eight honest lines cannot be broken that way.
// TestPanicInsideARecoverHandlerStillYieldsTheDefault is the regression test.
type guardedMetrics struct{ m Metrics }

func (g guardedMetrics) Evaluation(f string, r core.Reason) {
	defer func() { _ = recover() }()
	g.m.Evaluation(f, r)
}

func (g guardedMetrics) UninitializedEvaluation(f string) {
	defer func() { _ = recover() }()
	g.m.UninitializedEvaluation(f)
}

func (g guardedMetrics) StateChanged(from, to State) {
	defer func() { _ = recover() }()
	g.m.StateChanged(from, to)
}

func (g guardedMetrics) Generation(gen int64) {
	defer func() { _ = recover() }()
	g.m.Generation(gen)
}

func (g guardedMetrics) Connected(c bool) {
	defer func() { _ = recover() }()
	g.m.Connected(c)
}

func (g guardedMetrics) Staleness(s float64) {
	defer func() { _ = recover() }()
	g.m.Staleness(s)
}

func (g guardedMetrics) Resync(r string) {
	defer func() { _ = recover() }()
	g.m.Resync(r)
}

func (g guardedMetrics) L2Write(err error) {
	defer func() { _ = recover() }()
	g.m.L2Write(err)
}

// NopMetrics discards every signal. Embed it to get forward compatibility.
type NopMetrics struct{}

func (NopMetrics) Evaluation(string, core.Reason) {}
func (NopMetrics) UninitializedEvaluation(string) {}
func (NopMetrics) StateChanged(from, to State)    {}
func (NopMetrics) Generation(int64)               {}
func (NopMetrics) Connected(bool)                 {}
func (NopMetrics) Staleness(float64)              {}
func (NopMetrics) Resync(string)                  {}
func (NopMetrics) L2Write(error)                  {}

type clientConfig struct {
	ev           Evaluator
	env          string
	source       Source
	l2           SnapshotStore
	logger       Logger
	metrics      Metrics
	deadStream   time.Duration
	reconcile    time.Duration
	fetchTimeout time.Duration
	staleWarn    time.Duration
	backoff      BackoffFunc
	now          func() time.Time
}

// Option configures a Client.
type Option func(*clientConfig)

// WithEnvironment names the environment whose snapshot this client serves.
// Defaults to "default".
func WithEnvironment(env string) Option {
	return func(c *clientConfig) { c.env = env }
}

// WithEvaluator replaces the evaluation engine.
//
// The default is core.New(), the same engine the service binary links, with the
// shipped bucketing decisions. That default is the structural answer to
// docs/03-lld.md §3.3: client and server cannot disagree about what a flag
// means because there is only one implementation. Override this for tests or
// for a deliberate plug-point swap, never to write a second engine.
func WithEvaluator(ev Evaluator) Option {
	return func(c *clientConfig) {
		if ev != nil {
			c.ev = ev
		}
	}
}

// WithSource supplies the config plane. Without one the client never fetches
// and stays on whatever L2 hydration produced — useful for tests and for a
// deliberately offline mode, never for production.
func WithSource(s Source) Option {
	return func(c *clientConfig) { c.source = s }
}

// WithL2Store supplies the last-known-good store directly.
func WithL2Store(s SnapshotStore) Option {
	return func(c *clientConfig) { c.l2 = s }
}

// WithL2DiskCache enables the on-disk last-known-good tier at dir, using the
// default JSON codec. It is what shrinks the "cold start during a total flag
// service outage" window from routine to rare.
func WithL2DiskCache(dir string) Option {
	return func(c *clientConfig) { c.l2 = NewFileStore(dir, nil) }
}

// WithL2DiskCacheCodec is WithL2DiskCache with a transport-native codec, for
// when snapshots are not MemSnapshot.
func WithL2DiskCacheCodec(dir string, codec Codec) Option {
	return func(c *clientConfig) { c.l2 = NewFileStore(dir, codec) }
}

// WithLogger installs the log hook.
func WithLogger(l Logger) Option { return func(c *clientConfig) { c.logger = l } }

// WithMetrics installs the metrics hook. Supplying one enables the per-
// evaluation counter, which is skipped entirely when it is absent so the hot
// path does not pay for a hook nobody installed.
func WithMetrics(m Metrics) Option { return func(c *clientConfig) { c.metrics = m } }

// WithDeadStreamThreshold sets how long the push stream may be silent —
// no snapshot and no heartbeat — before it is treated as dead and the client
// enters StateDegradedStale. Default 30s. It must be comfortably larger than
// the source's heartbeat interval.
func WithDeadStreamThreshold(d time.Duration) Option {
	return func(c *clientConfig) { c.deadStream = d }
}

// WithReconcileInterval sets the correctness-backstop poll. Default 5m. Zero
// disables it, which is not recommended: it is the only thing that catches a
// push path that believes it succeeded and did not.
func WithReconcileInterval(d time.Duration) Option {
	return func(c *clientConfig) { c.reconcile = d }
}

// WithFetchTimeout bounds a single unary fetch. Default 5s.
func WithFetchTimeout(d time.Duration) Option {
	return func(c *clientConfig) { c.fetchTimeout = d }
}

// WithStalenessWarning sets the age past which the client warns that its
// snapshot is old. It never stops serving. Default 5m.
func WithStalenessWarning(d time.Duration) Option {
	return func(c *clientConfig) { c.staleWarn = d }
}

// WithBackoff replaces the reconnect strategy. The default is full jitter over
// 100ms..30s; replacing it with anything unjittered is a mistake.
func WithBackoff(b BackoffFunc) Option { return func(c *clientConfig) { c.backoff = b } }

// Client is the feature flag SDK handle. It is safe for concurrent use.
type Client struct {
	env     string
	ev      Evaluator
	cache   *cache
	sm      *stateMachine
	logger  Logger
	metrics Metrics
	// evalMetrics is the caller's hook, unwrapped, for the hot path only.
	evalMetrics Metrics
	limiter     *logLimiter

	// emitEval is false unless a Metrics hook was installed, so the default
	// configuration makes no interface call per evaluation.
	emitEval bool

	nowFn func() time.Time

	lastLiveNanos atomic.Int64
	staleWarn     time.Duration

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// New constructs a client. It never blocks on the flag service: the first fetch
// runs on a background goroutine and evaluations return caller defaults until
// it lands. It does read the L2 disk cache synchronously, which is a local file
// read of a few megabytes, not a network dependency.
//
// The returned error means the client was constructed wrongly — a programming
// error, surfaced at init. Runtime unavailability is never an error here,
// because turning it into one would push every application into writing its own
// ad-hoc fallback, and most of them would write it wrong.
func New(opts ...Option) (*Client, error) {
	cfg := clientConfig{
		ev:           core.New(),
		env:          "default",
		deadStream:   30 * time.Second,
		reconcile:    5 * time.Minute,
		fetchTimeout: 5 * time.Second,
		staleWarn:    5 * time.Minute,
		now:          time.Now,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if cfg.backoff == nil {
		cfg.backoff = FullJitterBackoff(100*time.Millisecond, 30*time.Second)
	}
	if cfg.metrics == nil {
		cfg.metrics = NopMetrics{}
	}

	c := &Client{
		env:         cfg.env,
		ev:          cfg.ev,
		logger:      cfg.logger,
		metrics:     guardedMetrics{cfg.metrics},
		evalMetrics: cfg.metrics,
		limiter:     newLogLimiter(time.Second),
		emitEval:    cfg.metrics != Metrics(NopMetrics{}),
		nowFn:       cfg.now,
		staleWarn:   cfg.staleWarn,
	}
	c.sm = newStateMachine(func(from, to State, reason string) {
		c.metrics.StateChanged(from, to)
		lvl := LevelWarn
		if to == StateHealthy {
			lvl = LevelInfo
		}
		c.logf(context.Background(), lvl, "flag.client.state",
			"from", from.String(), "to", to.String(), "reason", reason, "env", c.env)
	})
	c.cache = newCache(cfg.env, cfg.l2, func(err error) {
		c.metrics.L2Write(err)
		if err != nil {
			// A failed L2 write degrades cold-start recovery on the next
			// restart. It has already not failed the apply, because the swap
			// happened before this call.
			c.logf(context.Background(), LevelWarn, "flag.client.l2.write.failed", "err", err, "env", c.env)
		}
	})

	c.hydrateFromL2(cfg.l2)

	if cfg.source != nil {
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		u := &updater{
			c: c, src: cfg.source, env: cfg.env,
			deadStream: cfg.deadStream, reconcile: cfg.reconcile,
			fetchTimeout: cfg.fetchTimeout, backoff: cfg.backoff,
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			u.run(ctx)
		}()
	} else {
		c.logf(context.Background(), LevelWarn, "flag.client.no_source",
			"env", cfg.env, "detail", "client will never receive config updates")
	}

	if !c.sm.state().Serving() {
		// The loudest line this package emits. A pod in this state is serving
		// compiled-in defaults for every flag and nothing downstream will look
		// wrong while it does.
		c.logf(context.Background(), LevelError, "flag.client.uninitialized",
			"env", cfg.env, "detail", "serving call-site defaults until first snapshot")
	}
	return c, nil
}

func (c *Client) hydrateFromL2(l2 SnapshotStore) {
	if l2 == nil {
		return
	}
	snap, writtenAt, err := l2.Load(c.env)
	if err != nil {
		if !errors.Is(err, ErrNoSnapshot) {
			c.logf(context.Background(), LevelWarn, "flag.client.l2.load.failed", "err", err, "env", c.env)
		}
		return
	}
	if snap == nil {
		return
	}
	c.cache.hydrate(&entry{
		snap:      snap,
		gen:       snap.Generation(),
		appliedAt: writtenAt,
		fromDisk:  true,
	})
	c.metrics.Generation(snap.Generation())
	c.sm.onHydratedFromDisk()
	c.logf(context.Background(), LevelWarn, "flag.client.l2.hydrated",
		"env", c.env, "generation", snap.Generation(), "flags", snap.Len(),
		"age_seconds", c.now().Sub(writtenAt).Seconds())
}

func (c *Client) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

func (c *Client) markLive() { c.lastLiveNanos.Store(c.now().UnixNano()) }

func (c *Client) logf(ctx context.Context, lvl Level, event string, attrs ...any) {
	if c.logger == nil {
		return
	}
	// A logger is caller-supplied code. It runs on the updater goroutine or
	// inside an evaluation's recover boundary; either way it must not be able
	// to take the process down.
	defer func() { _ = recover() }()
	c.logger.Log(ctx, lvl, event, attrs...)
}

// State reports the current lifecycle state, for metrics and diagnostics.
func (c *Client) State() State { return c.sm.state() }

// Ready is the /ready gate: true once the client has config to serve.
//
// It deliberately does not require StateHealthy. A DEGRADED_STALE pod serving
// last-known-good config is a working pod, and refusing traffic from it during
// a flag-service outage converts a control-plane incident into a data-plane
// one across the entire fleet at once.
func (c *Client) Ready() bool { return c.sm.state().Serving() }

// Generation reports the generation currently serving, or 0 if none.
func (c *Client) Generation() int64 {
	if e := c.cache.load(); e != nil {
		return e.gen
	}
	return 0
}

// WaitForReady blocks until the client has config or ctx is done, and reports
// which happened. It is init-time sugar for a team that wants a bounded wait
// before serving traffic; it must never be called from a request path, and New
// never calls it, because the flag service must not become a startup dependency
// of the fleet.
func (c *Client) WaitForReady(ctx context.Context) bool { return c.sm.waitForReady(ctx) }

// Stats is a point-in-time view for diagnostics endpoints.
type Stats struct {
	Env              string
	State            State
	Generation       int64
	Flags            int
	AppliedAt        time.Time
	StalenessSeconds float64
	FromDisk         bool
}

// Stats returns a coherent snapshot of client status.
func (c *Client) Stats() Stats {
	s := Stats{Env: c.env, State: c.sm.state()}
	if e := c.cache.load(); e != nil {
		s.Generation = e.gen
		s.Flags = e.snap.Len()
		s.AppliedAt = e.appliedAt
		s.FromDisk = e.fromDisk
	}
	s.StalenessSeconds = c.stalenessSeconds()
	return s
}

// stalenessSeconds is time since the last frame of any kind from the source. It
// is zero before the first frame, because "infinitely stale" and "never
// connected" are different conditions and the state gauge already reports the
// second one.
func (c *Client) stalenessSeconds() float64 {
	n := c.lastLiveNanos.Load()
	if n == 0 {
		return 0
	}
	return c.now().Sub(time.Unix(0, n)).Seconds()
}

// reportStaleness publishes the staleness gauge and warns once the snapshot is
// older than the configured threshold. It warns; it never stops serving.
func (c *Client) reportStaleness(ctx context.Context) {
	sec := c.stalenessSeconds()
	c.metrics.Staleness(sec)
	if c.staleWarn <= 0 || sec < c.staleWarn.Seconds() || c.lastLiveNanos.Load() == 0 {
		return
	}
	if n, ok := c.limiter.allow("<staleness>", core.ReasonError, c.now().UnixNano()); ok {
		c.logf(ctx, LevelWarn, "flag.client.stale",
			"env", c.env, "staleness_seconds", sec,
			"threshold_seconds", c.staleWarn.Seconds(),
			"generation", c.Generation(), "state", c.sm.state().String(),
			"detail", "still serving last-known-good", "sampled_of", n)
	}
}

// Close stops the background updater and flushes the pending L2 write.
// Evaluations after Close return caller defaults rather than panicking.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		c.wg.Wait()
		c.cache.close()
		c.sm.close()
	})
	return nil
}

// ---------------------------------------------------------------------------
// Evaluation. Everything below is the read path.
// ---------------------------------------------------------------------------

// BoolValue evaluates flag and returns a bool, or def.
//
// ctx is used only for trace correlation in the logger and metrics hooks. The
// read path never blocks on it and never checks Done: cancelling a context must
// not change which value a flag has, and a cancelled request that is still
// running deserves a correct answer, not a surprise default.
func (c *Client) BoolValue(ctx context.Context, flag string, def bool, ec core.EvalContext) bool {
	if v, ok := c.BoolDetail(ctx, flag, def, ec).Value.AsBool(); ok {
		return v
	}
	return def
}

// StringValue evaluates flag and returns a string, or def.
func (c *Client) StringValue(ctx context.Context, flag string, def string, ec core.EvalContext) string {
	if v, ok := c.StringDetail(ctx, flag, def, ec).Value.AsString(); ok {
		return v
	}
	return def
}

// IntValue evaluates flag and returns an int64, or def.
//
// int64 rather than int because that is what core.Value carries; narrowing at
// the API boundary would make the value silently platform-dependent.
func (c *Client) IntValue(ctx context.Context, flag string, def int64, ec core.EvalContext) int64 {
	if v, ok := c.IntDetail(ctx, flag, def, ec).Value.AsInt(); ok {
		return v
	}
	return def
}

// BoolDetail is BoolValue with the full reasoning: reason, matched rule,
// computed bucket and the snapshot generation that answered. This is the method
// that turns "the flag returned false" into something actionable at 3am.
//
// The returned Result.Value is guaranteed to be a bool.
func (c *Client) BoolDetail(ctx context.Context, flag string, def bool, ec core.EvalContext) core.Result {
	return c.detail(ctx, flag, core.Bool(def), core.TypeBool, ec)
}

// StringDetail is StringValue with the full reasoning.
func (c *Client) StringDetail(ctx context.Context, flag string, def string, ec core.EvalContext) core.Result {
	return c.detail(ctx, flag, core.String(def), core.TypeString, ec)
}

// IntDetail is IntValue with the full reasoning.
func (c *Client) IntDetail(ctx context.Context, flag string, def int64, ec core.EvalContext) core.Result {
	return c.detail(ctx, flag, core.Int(def), core.TypeInt, ec)
}

// detail is the single-flag entry point and the panic-recover boundary for it.
//
// The recover runs on the calling goroutine — a panic cannot be caught across
// goroutines, so any design that evaluated on a worker would forfeit the
// never-throw contract — and the return is named so the recover can actually
// replace it. Returning the zero Result from a recovered panic would hand the
// caller Value{} with ReasonUnknown, which is a silent wrong answer where the
// contract promises a loud safe one.
func (c *Client) detail(ctx context.Context, flag string, def core.Value, want core.ValueType, ec core.EvalContext) (res core.Result) {
	defer func() {
		if r := recover(); r != nil {
			// Only overwrite a result that was never produced. If the
			// evaluation itself succeeded and a caller-supplied metrics or log
			// hook panicked afterwards, the good answer stands.
			if res.Reason == core.ReasonUnknown {
				res = core.Result{Value: def, Reason: core.ReasonError, Bucket: core.NoBucket}
			}
			c.onPanic(ctx, flag, r)
		}
	}()
	res = c.evaluate(c.cache.load(), flag, def, want, ec)
	c.record(ctx, flag, res)
	return res
}

// Batch evaluates many flags against one pinned snapshot.
//
// This is the mandatory API at the p99 request shape of 100 flags, not a
// convenience wrapper. Two reasons, and the second is the important one.
//
// Cost: 100 individual calls pay the entry boundary — recover frame, atomic
// load, hook dispatch — 100 times. One batch amortises it across all of them.
//
// Correctness: the snapshot pointer is pinned ONCE for the whole batch
// (invariant CACHE-1). Loading it per flag would let a config swap land
// mid-batch and hand the caller a result set where flag A came from generation
// N and flag B from generation N+1 — a cross-flag inconsistency that violates
// no single flag's semantics, produces a request that is individually explicable
// and jointly impossible, and is essentially unreproducible from a bug report.
//
// The returned slice is freshly allocated and has one entry per request, in
// order. Every entry is populated on every path, including panic.
func (c *Client) Batch(ctx context.Context, reqs []Request) []core.Result {
	return c.BatchAppend(ctx, nil, reqs)
}

// BatchAppend is Batch writing into dst, for callers pooling result slices to
// keep 100-element allocations out of a 12k RPS hot path. dst is truncated
// first; the returned slice may be a reallocation.
func (c *Client) BatchAppend(ctx context.Context, dst []core.Result, reqs []Request) (out []core.Result) {
	if cap(dst) >= len(reqs) {
		out = dst[:len(reqs)]
	} else {
		out = make([]core.Result, len(reqs))
	}

	// Pre-fill with the caller's defaults BEFORE evaluating anything, so that
	// however this function leaves — normally, or through a panic in code the
	// recover below cannot resume — every element the caller reads is a safe
	// answer rather than a zero Result.
	for i := range reqs {
		out[i] = core.Result{Value: reqs[i].Default, Reason: core.ReasonError, Bucket: core.NoBucket}
	}

	defer func() {
		if r := recover(); r != nil {
			c.onPanic(ctx, "<batch>", r)
		}
	}()

	// Pinned once. This single load is invariant CACHE-1.
	e := c.cache.load()

	for i := range reqs {
		out[i] = c.evalGuarded(ctx, e, &reqs[i])
	}
	return out
}

// evalGuarded is the per-element panic boundary inside a batch.
//
// Containment is per element rather than per batch on purpose: a single flag
// with a pathological rule must not silently convert the other 99 flags in the
// request to defaults. The cost is one deferred call per element, which at the
// ~300ns cost of an evaluation is under a percent.
func (c *Client) evalGuarded(ctx context.Context, e *entry, req *Request) (res core.Result) {
	defer func() {
		if r := recover(); r != nil {
			if res.Reason == core.ReasonUnknown {
				res = core.Result{Value: req.Default, Reason: core.ReasonError, Bucket: core.NoBucket}
			}
			c.onPanic(ctx, req.Flag, r)
		}
	}()
	res = c.evaluate(e, req.Flag, req.Default, req.Default.Type(), req.EvalContext)
	c.record(ctx, req.Flag, res)
	return res
}

// evaluate is the whole read path: an atomic load already done by the caller,
// then one call into the engine. No I/O, no lock, no allocation that can fail
// (invariant CACHE-3).
//
// Everything after the uninitialized check is the engine's job — flag lookup,
// FLAG_NOT_FOUND, the requested-type check, caller-default substitution. Doing
// any of it here would be a second implementation of evaluation policy living
// in the client, which is exactly the divergence §3.3 rules out.
func (c *Client) evaluate(e *entry, flag string, def core.Value, want core.ValueType, ec core.EvalContext) core.Result {
	if e == nil {
		// UNINITIALIZED. There is no config, and therefore no flag default to
		// read — which is exactly why the caller had to supply one.
		//
		// core.Reason has no CLIENT_UNINITIALIZED member and the contract is
		// frozen, so this maps onto ReasonError, which classifies as a fallback
		// and therefore lands in the fallback rate that hazard H1 alerts on.
		// The distinguishing signal is the dedicated counter and the state
		// gauge, not the reason code.
		c.metrics.UninitializedEvaluation(flag)
		return core.Result{Value: def, Reason: core.ReasonError, Bucket: core.NoBucket}
	}

	res := c.ev.Evaluate(e.snap, flag, ec, want, def)

	if res.Generation == 0 {
		// A third-party evaluator that forgot to stamp the generation still owes
		// the caller an answer to "which config decided this?".
		res.Generation = e.gen
	}
	if res.Reason == core.ReasonUnknown {
		// A completed evaluation must carry a reason. The shipped engine
		// guarantees this; an injected one might not, and an unlabelled result
		// is worse than an honest error.
		return core.Result{Value: def, Reason: core.ReasonError, Bucket: res.Bucket, Generation: res.Generation}
	}
	return res
}

// record emits the per-evaluation signals. It is inside the recover boundary,
// so a hook that panics costs an event, never a request.
func (c *Client) record(ctx context.Context, flag string, res core.Result) {
	if c.emitEval {
		c.evalMetrics.Evaluation(flag, res.Reason)
	}
	if c.logger == nil || !res.Reason.IsFallback() {
		return
	}
	// Rate limiting here is a correctness requirement, not politeness. One
	// misconfigured flag at 2.4M evaluations/sec writes 2.4M log lines/sec and
	// takes down the logging pipeline: a second-order outage substantially
	// worse than the flag bug that caused it.
	n, ok := c.limiter.allow(flag, res.Reason, c.now().UnixNano())
	if !ok {
		return
	}
	c.logf(ctx, LevelWarn, "flag.evaluation.fallback",
		"flag", flag, "reason", res.Reason.String(), "env", c.env,
		"generation", res.Generation, "returned_value_source", "call_site_default",
		"state", c.sm.state().String(), "sampled_of", n)
}

func (c *Client) onPanic(ctx context.Context, flag string, r any) {
	c.metrics.Evaluation(flag, core.ReasonError)
	if n, ok := c.limiter.allow(flag, core.ReasonError, c.now().UnixNano()); ok {
		c.logf(ctx, LevelError, "flag.evaluation.panic",
			"flag", flag, "env", c.env, "panic", r,
			"returned_value_source", "call_site_default", "sampled_of", n)
	}
}

// ---------------------------------------------------------------------------

// logLimiter is a lock-free per-(flag, reason) token bucket.
//
// It is a fixed array of cells addressed by hash rather than a map, because the
// alternative is a mutex-guarded map on a path that, during exactly the
// incident it exists to report, is taken by every one of 2.4M evaluations per
// second. Collisions between distinct flags merely share a budget, which costs
// a log line; a contended mutex there would cost the service.
type logLimiter struct {
	cells    [256]limCell
	interval int64
}

type limCell struct {
	next       atomic.Int64 // earliest permitted emit, unix nanos
	suppressed atomic.Int64
	_          [48]byte // keep cells on separate cache lines
}

func newLogLimiter(interval time.Duration) *logLimiter {
	if interval <= 0 {
		interval = time.Second
	}
	return &logLimiter{interval: int64(interval)}
}

// allow reports whether an event may be emitted, and how many events the
// returned one stands for, so true volume is reconstructible from sampled_of.
func (l *logLimiter) allow(flag string, reason core.Reason, now int64) (sampledOf int64, ok bool) {
	h := uint32(2166136261)
	for i := 0; i < len(flag); i++ {
		h ^= uint32(flag[i])
		h *= 16777619
	}
	h ^= uint32(reason)
	h *= 16777619
	cell := &l.cells[h&255]

	for {
		next := cell.next.Load()
		if now < next {
			cell.suppressed.Add(1)
			return 0, false
		}
		if cell.next.CompareAndSwap(next, now+l.interval) {
			return cell.suppressed.Swap(0) + 1, true
		}
	}
}

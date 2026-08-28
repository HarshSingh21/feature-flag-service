// Package client is the thin, in-process feature-flag SDK that application
// backends import.
//
// # The one-paragraph model
//
// Every evaluation is local. The client holds one immutable, fully resolved
// [core.Snapshot] of an environment's flags behind an atomic pointer, and an
// evaluation is an atomic load, a map lookup, a predicate walk and a hash.
// Nothing on the read path touches the network, the disk, a mutex, or the
// config source. That is invariant CACHE-3 of docs/03-lld.md §3.6, and it is
// what makes "sub-millisecond, never blocks, never throws" satisfiable at the
// design load of 2.4M evaluations/sec.
//
// # Usage
//
//	fc, err := client.New(
//	    client.WithEnvironment("prod"),
//	    client.WithSource(tr),                       // internal/transport
//	    client.WithL2DiskCache("/var/cache/flags"),  // optional, survives restart
//	    client.WithLogger(myLogger),
//	    client.WithMetrics(myMetrics),
//	)
//	if err != nil {
//	    return err // construction errors are programming errors, not runtime ones
//	}
//	defer fc.Close()
//
//	// One flag.
//	if fc.BoolValue(ctx, "checkout_v2", false, core.EvalContext{UserID: uid}) {
//	    ...
//	}
//
//	// The p99 request shape: 100 flags, one pinned snapshot, one boundary crossing.
//	res := fc.Batch(ctx, []client.Request{
//	    {Flag: "checkout_v2", Default: core.Bool(false), EvalContext: ec},
//	    {Flag: "max_items", Default: core.Int(50), EvalContext: ec},
//	})
//
// # The default is supplied at the call site, and that is not negotiable
//
// Every accessor takes the caller's default positionally and mandatorily. This
// is forced by the failure model, not by taste: in [StateUninitialized] there is
// no config from which to read a flag's own default, so the only value the
// client can honestly return is one the call site handed it. Do not "simplify"
// this to BoolValue(name, ctx) — see docs/02-hld.md §D.5.
//
// The review rule that makes fail-open safe: the call-site default must be the
// value that was correct before the flag existed.
//
// # One evaluator, not two
//
// The client evaluates with core.New() by default: the same engine, with the
// same bucketing decisions, that the service binary links. A client-side cache
// implies a client-side evaluator, and two evaluators that must agree will
// eventually disagree — so this package contains no evaluation logic of its own
// to diverge. It owns the cache, the state machine and the update consumer;
// every question about what a flag *means* is the engine's. Use
// [WithEvaluator] to inject a double in tests, never to write a second engine.
//
// # Typed accessors, not a generic one
//
// Go does not permit type parameters on methods, so a generic accessor would
// have to be a free function taking the client — worse ergonomics, unmockable,
// and undiscoverable from the [Client] godoc. Three typed methods per shape is
// the smaller cost.
//
// # Startup never blocks on the flag service
//
// [New] returns a usable client immediately. If it blocked on a first fetch, the
// flag service would become a hard dependency of every deploy and every pod
// restart across the fleet — a single point of failure strictly worse than the
// problem flags were bought to solve. The first fetch happens on a background
// goroutine; until it lands, evaluations return caller defaults with a loud,
// rate-limited alarm. A team that genuinely wants a bounded wait can opt into
// one with [Client.WaitForReady].
//
// # Freshness never costs availability
//
// There is no hard cache expiry. A snapshot that stops being refreshed keeps
// serving forever, because expiring it would convert a control-plane freshness
// problem into a fleet-wide data-plane outage. The named, accepted cost: a kill
// switch flipped during a total flag-service outage will not propagate. If a
// control needs a hard freshness guarantee it must not be a feature flag.
//
// # Concurrency
//
// A [Client] is safe for concurrent use by any number of goroutines. All
// evaluation methods are wait-free with respect to config updates: a snapshot
// swap never blocks a reader and a reader never blocks a swap.
package client

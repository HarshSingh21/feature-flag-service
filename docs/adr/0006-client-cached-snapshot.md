# ADR-0006: The client caches a resolved snapshot and evaluates locally

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-28 |
| **Decision id** | **O5** in [`PLAN.md`](../../PLAN.md) |
| **Source analysis** | [`docs/03-lld.md`](../03-lld.md) §2, §3, §6 · [`docs/02-hld.md`](../02-hld.md) §A, §D |
| **Implements** | `pkg/client`, `core.Snapshot`, invariants CACHE-1/2/3 |
| **Related** | [ADR-0007](0007-stdlib-only-metrics.md) — how the fail-open is detected |

## Context

Section A of the HLD designed the client as a thin RPC wrapper: the application
calls the flag service per evaluation, the service owns the one evaluator, and
there is exactly one implementation of the semantics. Section D, sizing the
propagation path, assumed a client-side snapshot. The two sections reached
opposite conclusions, which is how O5 came to exist as an open decision.

It was closed by arithmetic, not preference.

### The load nobody quotes

```
F = flags evaluated per request = 30 typical, 100 at p99

12,000 RPS x 30   =   360,000 evaluations/sec   steady state
24,000 RPS x 100  = 2,400,000 evaluations/sec   peak worst case
```

**The unit of load is evaluations, not requests.** `F` is invisible in the "12,000
RPS" figure the product team quotes, and this is the number capacity plans miss.
The design must hold at 2.4M/sec, not 12k.

### The decisive latency table

| Path | p50 | p99 | Meets the sub-millisecond budget? |
|---|---|---|---|
| Remote RPC per evaluation, F=100 | 44 ms | **208 ms** | No — off by 200x. It is 100 sequential round trips |
| Remote RPC, batched into one call | 0.44 ms | **2.08 ms** | No — misses by 2x, before any load |
| **Local evaluation on a cached snapshot, F=100** | **~30 µs** | **~340 µs** | **Yes, with ~3x margin** |
| Local evaluation, single flag | ~0.3 µs | ~3.4 µs | reference cost |

The remote numbers are not slow code. They are scheduling, cross-AZ round-trip
time, and TLS — irreducible. **No amount of server optimisation moves a network
hop under a millisecond at p99.** Per-RPC overhead (HTTP/2 framing, decode,
encode, flow control, goroutine scheduling, syscalls) is roughly **80x** the cost
of the evaluation it carries; the network is roughly **1,300x**. Micro-optimising
the matcher is a rounding error against either.

And the RPC design puts **2.4M RPC/sec** on the flag service at peak, which is a
second tier-1 distributed system to build, run, and page for.

## Decision

**The client library holds a full resolved snapshot of its environment in memory
and evaluates locally. The flag service becomes a control plane, not a
request-path dependency.**

Three cache tiers, and nothing else is cached:

| Tier | Contents | Lifetime | Purpose |
|---|---|---|---|
| **L0** | The caller's default at the call site | compile time | Terminal fallback. Always available, cannot fail |
| **L1** | Full resolved environment snapshot, immutable | process | The hot path. Every evaluation reads only this |
| **L2** | Last-known-good snapshot on local disk | pod | Survives a restart during a flag-service outage |

Three invariants make it safe:

- **CACHE-1 — pin once per request, not once per flag.** A swap landing mid-request
  would otherwise produce a result set where flag A came from generation N and
  flag B from generation N+1: a cross-flag inconsistency that is close to
  impossible to reproduce from a bug report.
- **CACHE-2 — build then swap, never mutate.** The snapshot is constructed to
  completion in a fresh allocation and published with one atomic pointer store.
  This is a correctness requirement, not a preference: a concurrent map read and
  write in Go is a **fatal runtime error, not a panic**, and `recover()` cannot
  catch it. A mutate-in-place design has an uncontainable failure mode.
- **CACHE-3 — the read path performs no I/O.** No network, no disk, no lock, no
  allocation that can fail. An atomic load, a map lookup, a predicate walk, a
  hash. Nothing else.

**This is not a read-through cache.** The cache is filled by the **write path**,
as the final stage of the merge pipeline: merge, validate, build, then swap. An
unknown flag is an *answer* (`FLAG_NOT_FOUND` plus the caller's default), never a
cache miss to be filled. Fill-on-miss would make the first request after every
config change pay a fetch and would let concurrent cold-key requests stampede the
origin — at 2.4M evaluations/sec that is an outage, not a latency blip.

**L3 is not violated.** The snapshot lives inside the *application backend*
process. Browser and mobile tiers still never evaluate and never receive flag
config.

## Consequences

### Positive — the load inversion, which is the point

| | Without client cache | With client cache |
|---|---|---|
| Flag service RPS | **2,400,000/s** at peak | **~0** |
| Load driven by | user traffic | fleet size and config change rate |
| Scaling trigger | every traffic spike | adding pods, or editing a flag |
| Capacity plan | a second tier-1 distributed system | a control plane |

Caching converts an **O(traffic)** problem into an **O(pods)** one. At 40
application pods serving 12,000 RPS, the flag service holds 40 long-lived streams
and pushes on change. **Traffic could grow 100x without the flag service
noticing.**

### Positive — availability decouples, which is the stronger argument

| Design | Effective availability |
|---|---|
| Per-request RPC | `min(app, flag service)` — a 99.9% flag service **caps the app at 99.9%** and adds its outages to yours |
| Client-cached snapshot | app availability is **independent** of the flag service. An outage degrades config *freshness*, never evaluation |

This is the strongest argument for the cache, stronger than latency. It removes
the flag service from the request-path critical dependency set entirely — which is
what the brief means by "evaluation must survive a total flag-service outage."

Memory cost is negligible: ~6 MB for a 5,000-flag resolved snapshot, ~10–18 MB per
pod with three retained generations and structural sharing. The whole config
corpus for an environment is smaller than one JPEG, which is exactly why caching a
*subset* would be pointless and eviction is unnecessary.

### Negative — and the answer to each

**"It is a second evaluator that must agree with the first."** Section A's
objection, and it is correct. The resolution is structural rather than
argumentative: **`internal/core` is compiled into both the service binary and the
client library.** There is one implementation. The service's own evaluate RPC
calls the identical package — it is a debugging and admin surface, not a second
evaluator. A golden-vector contract test runs a shared fixture corpus through both
linkages and asserts byte-identical `Result`, including `Reason`, `RuleID`, and
bucket. There is no second implementation to diverge.

**Staleness is now possible.** Answered by the propagation design: push, plus a
500 ms generation-bearing heartbeat, plus a 1,500 ms dead-stream threshold, plus a
30 s reconcile poll. Worst case across *all* delivery paths is **2,040 ms** against
the 5 s budget — 59% margin. Freshness is asserted by generation equality, never
by connection state.

**No hard cache expiry.** Expiring converts a freshness problem into an
availability outage, which is precisely backwards. Named and accepted risk: **a
kill switch will not propagate during a total flag-service outage.** That is the
cost of never failing closed.

**Startup must not block on the flag service** — otherwise the flag service
becomes a hard dependency of every deploy and every pod restart fleet-wide, a
single point of failure strictly worse than the problem flags exist to solve. The
accepted consequence: a pod starting during a total outage with no L2 cache serves
compiled-in defaults. `/ready` gates on `state != UNINITIALIZED` — a
`DEGRADED_STALE` pod serving last-known-good is a working pod, and refusing its
traffic would convert a control-plane incident into a data-plane one.

**Silent fail-open is now the characteristic failure.** A degraded client returns
the caller's default for everything, nothing errors, and nobody is paged. This is
hazard H1, and without its detector this design is a trap:
`flag_client_fallback_total` must be **alerted when unexpectedly low as well as
high** — a metric that is always zero is usually a metric that is not wired up.
`core.Reason.IsFallback()` is what that counter counts.

## Alternatives considered

**Per-evaluation RPC** (section A's original design). Disqualified twice over: 208
ms p99 at F=100 against a sub-millisecond budget, and 2.4M RPC/sec at peak.

**Batched RPC, one call per request.** The honest near-miss, and it is why the
batch API is mandatory *within* the chosen design. As the sole mechanism it still
misses at 2.08 ms p99 — 2x over budget before any load — and it leaves app
availability capped at the flag service's.

**Shared external cache (Redis) in front of the flag service.** Adds back the
network hop the whole design exists to remove, plus a new availability dependency,
to solve a problem that 6 MB of process memory already solves.

**Per-evaluation result caching.** Rejected: an evaluation costs ~0.3 µs. A map
lookup plus invalidation bookkeeping costs more than it saves, and it introduces
an invalidation-correctness problem where none existed.

**Per-user bucket assignment caching.** Rejected on arithmetic: bucketing is pure
computation, so caching it converts an O(1) design into an O(users) one. At 1B
users that is ~16 GB *per flag*, replicated and invalidated. Computed instead: **0
bytes, independent of population.** One billion users and one thousand users cost
exactly the same, which is why user count appears nowhere in the capacity model.

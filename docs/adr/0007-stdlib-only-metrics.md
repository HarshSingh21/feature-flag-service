# ADR-0007: A `Metrics` interface with an expvar-backed default, not a Prometheus dependency

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-28 |
| **Source analysis** | [`docs/02-hld.md`](../02-hld.md) §D.6 · [`docs/03-lld.md`](../03-lld.md) §5.2, §7 |
| **Implements** | `internal/obs` |
| **Related** | [ADR-0006](0006-client-cached-snapshot.md) — the fallback counter is this system's most important signal |

## Context

This repository ships two things: a service binary, and **a library that other
teams import into their own application processes**. Those are different products
with different dependency politics.

A metrics client is not a neutral dependency for a library. `prometheus/client_golang`
pulls in `prometheus/common`, `prometheus/procfs`, `protobuf`, and their transitive
graph, and it registers into a **process-global default registry**. Every consumer
of `pkg/client` inherits all of it, in their `go.mod`, in their binary size, and in
their upgrade schedule. A team already on OpenTelemetry, or on a vendor SDK, or
pinned to an older Prometheus major, now has a version conflict caused by a feature
flag library.

At the same time, the observability requirements here are unusually load-bearing.
Three of this design's signals are the *only* detection for failures that are
otherwise completely silent:

- `flag_client_fallback_total` — hazard H1, silent fail-open. Alerted **low** as
  well as high.
- `pending_changes_seconds` — hazard H5, stale config. "We shipped the fix and it
  never took effect" is otherwise undetectable.
- `bucketing_scheme_hash` — hazard H3, silent re-bucketing. See [ADR-0001](0001-bucketing-key.md).

So metrics cannot be optional, and they cannot be a mandatory dependency either.

## Decision

**Define a narrow `Metrics` interface in `internal/obs` and depend on that.** Ship
an implementation backed by `expvar` from the standard library as the default, and
a no-op implementation for tests.

Roughly:

```go
type Metrics interface {
    Inc(name string, labels ...Label)
    Observe(name string, v float64, labels ...Label)
    Set(name string, v float64, labels ...Label)
}
```

The interface is what the rest of the codebase calls. Consumers who want
Prometheus, OpenTelemetry, StatsD, or a vendor SDK supply an adapter — perhaps 60
lines — in **their** module, where the dependency belongs. The default requires no
configuration and no dependency, so a service that ignores the question still gets
observable behaviour rather than none.

Label cardinality is enforced at this boundary rather than by convention:

| Field | Label? | Reason |
|---|---|---|
| flag key | yes | bounded, ~5,000 |
| environment | yes | bounded, ~3 |
| reason | yes | bounded, 9 — `core.Reason` is low-cardinality by construction |
| **user id** | **never** | 1B series would destroy the metrics backend before the flag service noticed |
| **rule id** | **never** | unbounded; it belongs in `Result.RuleID` and in logs |
| tenant id | only if bounded and verified | check the ceiling first |

`core.Reason` was designed to be a safe metric label. `Result.RuleID` was designed
not to be. That distinction is the whole cardinality strategy, and enforcing it at
the `Metrics` boundary means it cannot be violated by a well-meaning patch.

## Consequences

### Positive

- `pkg/client` stays importable by anyone. A feature flag library that forces a
  metrics stack on its consumers gets vendored, forked, or wrapped — all of which
  are worse than the dependency it avoided.
- Tests inject a fake `Metrics` and assert on emissions directly, without a
  registry, without global state, and without registration-order flakiness.
- `expvar` is already exposed on `/debug/vars` and is enough to answer "is the
  fallback counter moving?" during an incident with `curl` and nothing else.
- The interface is the enforcement point for cardinality.

### Negative — stated plainly

- **`expvar` has no native histogram.** Latency percentiles are the obvious gap.
  The mitigation is that the two latency SLOs here (`< 50 µs` single flag, `< 500 µs`
  for a batch of 100) are validated by **benchmarks in CI**, which is where a
  microsecond-scale budget is actually measurable; production alerting on those is
  a client-side adapter's job. This is a real limitation, not a non-issue.
- `expvar` publishes into a process-global map with panic-on-duplicate
  registration — the same class of global state the Prometheus objection names.
  It is contained by registering once, at construction, in one place.
- An adapter is a small amount of work pushed onto each consumer, and two
  consumers will write slightly different ones.
- No exemplars, so no metric-to-trace correlation out of the box. Trace ids
  propagate through logs regardless.

### What would change this decision

Any one of these, and Prometheus (or an OTel meter) becomes the right default:

1. **The service binary needs real latency histograms in production**, not just in
   benchmarks — i.e. the p99 evaluation SLO becomes an alert rather than a build
   gate. `expvar` cannot express that and should not be extended to.
2. **The library stops being a public artefact** — if `pkg/client` is only ever
   consumed inside one organisation with one agreed metrics stack, the dependency
   objection evaporates and the adapter is pure overhead.
3. **More than two adapters get written**, or two of them disagree about metric
   names. At that point the abstraction is costing more than it saves and the
   right move is to pick one library and expose an escape hatch.
4. **Exemplars become necessary** to correlate a fallback spike with a trace.
5. **Push-based delivery is required** (no scrape endpoint reachable), which
   `expvar` fundamentally does not do.

## Alternatives considered

**Depend on `prometheus/client_golang` directly.** The default choice, and correct
for a pure service. Rejected because half of what ships here is a library.

**Depend on OpenTelemetry metrics.** A more defensible mandatory dependency than
Prometheus — it is the vendor-neutral one, and the HLD's package table anticipates
OTel in `internal/obs`, with a separate `pkg/flagclient/otel` subpackage precisely
so consumers can opt out of the dependency tree. That subpackage split is the same
instinct as this ADR, arrived at one layer higher up: an adapter package rather
than an adapter interface. Rejected because the interface keeps the dependency out
of `go.sum` entirely rather than merely out of the import path, and because the
OTel metrics API has churned more than the stdlib has.

> **Unreconciled between documents.** `02-hld.md` §A.2.2 lists `internal/obs` as
> importing "stdlib + otel". This ADR narrows that to stdlib only, with OTel as a
> consumer-supplied adapter. The HLD table should be updated, or this ADR
> superseded — but not both left standing.

**No abstraction — log everything and derive metrics from logs.** Rejected. The
fallback counter must be alertable at sub-second resolution and must be cheap
enough to increment on a path that runs 2.4M times per second. A log line per
evaluation is a logging-pipeline outage (hazard, failure mode 11), which is worse
than the bug it was meant to reveal.

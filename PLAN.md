# PLAN.md — Feature Flag Service
**Status:** Phase 0 ✅ PASSED — all decisions closed. Phase 1 ✅ complete. Implementation open.
**Owner:** harsh · **Date:** 2026-08-28

> **How to read this plan.** Work proceeds phase by phase. Every phase ends at a
> **VERIFICATION GATE**. No phase begins until you have explicitly approved the one
> before it. Nothing in a later phase is started speculatively.

---

## Locked decisions

| # | Decision | Value | Source |
|---|---|---|---|
| L1 | Deployment shape | Standalone Flag Evaluation Service + thin Go client | your call |
| L2 | Language / runtime | Go 1.26 | your call |
| L3 | Evaluation locus | Backend service ONLY. Browser/mobile never evaluate | your call |
| L4 | Config layering | Helm-style: base layer + per-environment overlays, deep-merged | your call |
| L5 | Persistence | In-memory only. No DB, no admin UI, no multi-region | brief, out of scope |
| L6 | Flag types | boolean, string, integer | brief |
| L7 | Never-throw contract | Any internal error returns the flag default + structured error log | brief |
| L8 | Propagation budget | Config change live in under 5 seconds, no restart | brief |
| L9 | Backend throughput | 12,000 RPS sustained, 24,000 peak assumed | your call |
| L10 | Evaluation latency budget | Sub-millisecond per call | your call |
| L11 | User population | 1,000,000,000 | your call |
| L12 | Availability | Evaluation must survive a total flag-service outage | your call |

## Decisions — ALL CLOSED

Recorded here as the decision log. Phase 2 onward is unblocked.

| # | Question | Candidates | Blocks |
|---|---|---|---|
| ~~O1~~ | **CLOSED — `bucket_namespace`, a configurable salt defaulting to the flag name.** Empty → independent per flag; shared literal → deliberately correlated | Phase 6 |
| ~~O2~~ | **CLOSED — rules first, rollout on fallthrough.** First rule match returns and stops; the rollout runs only for users who fell through every rule. Written as an explicit `evaluation_order` field, never an implicit default | Phases 5, 6 |
| ~~O3~~ | **CLOSED — both.** Reject at config time, fail-safe at evaluation time. Per-flag quarantine so one bad flag cannot freeze an environment | Phase 3 |
| ~~O4~~ | **CLOSED — hybrid.** Push, 500 ms generation-bearing heartbeat, 1.5 s dead-stream threshold, 30 s reconcile poll | Phase 7 |
| ~~O5~~ | ~~Does the thin client cache config?~~ **CLOSED by L9–L12** — see [`docs/03-lld.md`](docs/03-lld.md) §2. At F=100 a per-evaluation RPC is 208 ms p99 and 2.4M RPC/sec. **Client-cached snapshot, local evaluation** | Phases 7, 8 |
| ~~S-O1~~ | **CLOSED — F = 30 typical, 100 p99.** Peak 2.4M evaluations/sec | Phase 2 |
| S-O2 | L2 on-disk last-known-good cache — Phase 1 or later? | in · out | Phase 8 |
| S-O3 | Max acceptable config staleness during a total outage | currently unbounded by design | Phase 8 |

**O1 and O2 were closed together**, as §C.9 required — they interact through the
question of whether two rules on one flag share a bucket space. Under the chosen
rules-first ordering, a rule and the rollout are mutually exclusive for any given
user, so that question does not arise.

### Capacity consequences of F = 100

| | Value |
|---|---|
| Peak evaluations/sec | **2,400,000** |
| CPU, peak realistic | 0.72 core |
| CPU, peak pathological | **8.16 cores** — a line item now, not a rounding error |
| RPC-per-evaluation alternative | 2.4M RPC/sec, or 208 ms p99 per request. Disqualified twice over |
| Batch API | **Mandatory**, not a convenience |
| Rules per flag | Now a budgeted resource. Lint warns above 10 |

---

## Phase 1 — High-Level Architecture

**Objective.** Fix the shape of the system: what the deployables are, where the
boundaries fall, how config is layered and resolved, how an evaluation actually runs,
and how a change reaches a running process in under five seconds.

**Status:** ✅ COMPLETE — authored in four parallel workstreams. Awaiting your sign-off.

| § | Subsection | Covers |
|---|---|---|
| 1.1 | System decomposition | Container and component views, Go package spine, repo layout, the hot read path, thin-client responsibilities, the network-hop trade-off |
| 1.2 | Layered config model | Helm-style base + environment overlays, merge semantics per field kind, the list-merge decision, snapshot resolution, validation table, last-known-good posture |
| 1.3 | Evaluation engine | Pipeline stages, evaluation reasons, targeting rule model, hash and bucketing mechanism, type safety, how never-throw is actually enforced in Go |
| 1.4 | Live updates and operability | Propagation chain with latency budget, push vs pull, atomic snapshot swap, versioning, metrics and SLOs, failure-mode table |

**Full design:** [`docs/02-hld.md`](docs/02-hld.md) — 3,024 lines, 10 Mermaid diagrams,
every diagram machine-verified with a real `mermaid.parse()` run rather than eyeballed.
Condensed diagram set also at [`docs/diagrams/hld.md`](docs/diagrams/hld.md).

### Calls the architecture makes

| # | Call | Why it is not arbitrary |
|---|---|---|
| A1 | Two deployables, not three — admin is the same binary on a second listener | Separate accept queue so a config-push storm cannot starve evaluation. A third deployable buys nothing and adds a pager |
| A2 | Concentric dependency rings, imports inward only | Core cannot import a logger, a clock, or any I/O — so errors are returned as data. This is what makes never-panics fuzzable rather than aspirational |
| A3 | `recover()` at exactly two sites | A recover in the core is a review reject |
| A4 | All cost on the write path | Read path is an atomic pointer load, a map lookup, a predicate walk, and a hash |
| A5 | Batching is the design, not an optimisation | Per-RPC overhead is ~80x evaluation cost; network ~1300x. Micro-optimising the matcher is a rounding error |
| B1 | Rule lists take `replace` or `append`, never a deep merge, and never `prepend` | With first-match-wins the *order* is the semantics. Index merge silently re-pairs every overlay patch when a base rule is inserted at position 0; keyed merge defines content but not order, so you would have to simulate the merge to know prod behaviour |
| B2 | `rollout` deep-merges rather than replacing | Whole-block replace lets `rollout: {percentage: 25}` blank `bucket_by`, reshuffling every user's bucket and flipping enrolled users off during a routine percentage bump. A merge rule that produces an incident |
| B3 | Four layers: caller default, base, env overlay, **ops override** | L3 is a correctness argument, not convenience: `set()` is CI-driven, so an on-call who kills a flag by editing the prod overlay gets it **silently resurrected** by the next pipeline run. Whitelisted to 2 fields, TTL mandatory, capped at 30 days |
| B4 | Tri-state `Opt[T]`, not pointer fields | Pointers collapse absent and explicit-null into `nil`. Consequence: base and overlay are *different Go types* — total record versus sparse patch |
| B5 | ConfigStore holds raw unmerged layers | Preserves what BASE said versus the prod overlay, for incident forensics |
| B6 | Eager resolution at write time | Justified on fail-safe posture first: the post-merge checks are only decidable after merging, so lazy merge would push type-mismatch discovery onto the hot path and violate never-throw. Latency merely backs it up |
| B7 | Per-environment transactionality, not global | Global atomicity buys "all envs agree" — worthless, since envs are meant to differ — and costs "a prod typo blocks an urgent dev fix" |
| B8 | Ambiguous config is **rejected**, never defaulted | A flag with both rules and a rollout and no explicit `evaluation_order` is rejected outright. Shipping a default would decide O2 by accident |
| C1 | Absent attribute makes a condition false, *before* negation | Otherwise `country != IN` with a failed geo lookup silently matches everyone |
| C2 | xxhash64, pinned by golden vectors, treated as a wire format | `maphash` is the trap a reviewer will suggest — its per-process seed reshuffles every rollout on every deploy |
| C3 | Monotone ramp: raising a percentage only ever adds users | Destroyed by any `hash(key + percentage)` scheme |
| C4 | Typed accessors, not a generic one | Go does not permit type parameters on methods, so a generic accessor must be a free function — unmockable and undiscoverable |
| C5 | Immutable snapshot is a correctness requirement, not a preference | Concurrent map read/write in Go is a **fatal runtime error, not a panic** — `recover` cannot catch it. A mutate-in-place design has an uncontainable failure mode |
| D1 | Hybrid push + 500ms heartbeat + 30s reconcile poll | A 2s poll loses on worst case, typical case by 22x, *and* cost — 2,500 rps versus 167 at 5,000 clients. Dominated on every axis |
| D2 | `atomic.Pointer` over `RWMutex` | An RWMutex config write stalls ~5,000 in-flight evaluations per change |
| D3 | `SnapshotID` is `(instance_id, generation)`, never a bare counter | Generation resets on restart, so a client at gen 900 meeting a restarted server at gen 3 silently concludes it is ahead |

### Numbers this design commits to

| Budget | Value | Margin |
|---|---|---|
| Propagation, push path p99 | 90 ms | — |
| Propagation, **worst case across all paths** | 2,040 ms | 59% under the 5s requirement |
| Evaluation, service-side p99 | < 50 µs | worst realistic flag is ~3.4 µs |
| End-to-end client call p50 / p99 | 0.44 ms / 2.08 ms | 31% under a 3 ms target |
| Subscriber memory at 5,000 clients | 320 MB of HTTP/2 stream buffers | against ~6 MB of actual snapshots |

### Hazards named, not buried

| # | Hazard | Detection |
|---|---|---|
| H1 | **Silent fail-open.** A degraded flag service makes every flag read as its default. Nothing errors, nobody is paged | `flag_client_fallback_total` alerted low. Without it this design is a trap |
| H2 | **"Slow, not down" is the real failure mode.** A fast shed beats a successful 20 ms response | Hard timeout, breaker treating slow as failed, in-flight semaphore, server-side admission control — the design, not optional hardening |
| H3 | **Silent re-bucketing.** Changing the bucketing key flips ~18% of users at a 10% rollout while every dashboard stays green | `bucketing_scheme_hash` gauge pages on change; a 1,000-ID golden vector fails the build and refuses boot on diff |
| H4 | **In-memory restart loses the whole corpus** while clients hold stale snapshots with no source of truth | Config-apply audit log must ship off-box and be replayable. Leads Phase 2 |
| H5 | **Serving stale config is silent by construction.** "We shipped the fix and it never took effect" is otherwise undetectable | `pending_changes_seconds` is the load-bearing alert. If it is not wired, this design has no failure signal |
| H6 | **Cold start is the only case with no last-known-good** — a deploy can quietly serve the whole fleet on compiled-in defaults | `/ready` must gate on generation ≥ 1 per environment |
| H7 | **The `append` operator lets base rule edits reach prod without a prod edit** | Intended semantics, but it is the one path by which a base change alters prod behaviour. Provenance table plus change review |

### Escalated to you

1. **O5 is new** — sections A and D reached opposite conclusions on whether the thin client caches config. See the open-decisions table above.
2. **O1 and O2 must be closed in one ADR.** They interact: rollout-nested-in-rules asks whether two rules on one flag share a bucket space.
3. **O2 options 1↔2 are the expensive migration** — identical JSON, different meaning, so every flag silently changes behaviour. Write an explicit `precedence` discriminator into the schema now, even if only one value is legal.
4. **Strike user-id-only from O1?** It cannot express the brief's opt-in sharing requirement and concentrates every canary onto one cohort. Section C calls it a trap option.
5. **Defer time-based targeting out of Phase 1** — in a client-cached model it is a distributed clock problem; as a cron calling `set()` it is a single-machine one.
6. **Unscoped work:** section C assumes a `flagctl lint` CI step cross-referencing Go source against declared flag types. Someone owns it or the claim softens.
7. **Cross-section contract for Phase 2:** load the snapshot pointer **once per request**, not once per flag, or cross-flag read consistency is forfeited. Section D's `Snapshot` and section B's `ResolvedSnapshot` are the same object under two names — reconcile the vocabulary before contracts are written.

**Exit criteria.** Every component has a named owner package and a one-line contract;
every open decision O1–O4 has an identified plug point; the propagation budget sums to
under 5s with margin.

> **VERIFICATION GATE 1** — you approve the architecture before any contract or code is written.

---
## Phase 2 — Low-Level Design & Contracts

**Objective.** Turn the Phase 1 architecture into signatures a reviewer can argue with.

| Deliverable | Detail |
|---|---|
| `docs/03-lld.md` | Package-by-package types, interfaces, and their contracts |
| Wire contract | Evaluate request/response schema, error envelope, version field |
| Error taxonomy | Full reason enum, log schema, which reasons are page-worthy |
| Interface stubs | `BucketKeyStrategy`, `PrecedenceStrategy`, `Operator`, `ConfigSource` |
| Test plan | Case list per requirement, mapped to the brief's five mandated areas |

**Diagrams:** [`docs/diagrams/lld.md`](docs/diagrams/lld.md) — evaluation pipeline,
evaluate-call sequence, core type model, config validation passes, sticky bucketing
mechanism, never-throw boundary, evaluation reason enum.

**Scale, caching, throughput and availability:** [`docs/03-lld.md`](docs/03-lld.md) —
the capacity model at 12,000 RPS and 1B users, the three-tier cache, the load
inversion, and the availability decoupling argument.

| Finding | Consequence |
|---|---|
| The unit of load is **evaluations, not requests** — 12,000 RPS x 30 flags = 360,000/s, peak worst case 2,400,000/s | `F` is invisible in the quoted RPS figure. This is the number capacity plans miss |
| A per-evaluation RPC is **2.08 ms p99** against a sub-ms budget | Disqualified on arithmetic. No server optimisation moves a network hop under 1 ms at p99 |
| Local evaluation on a cached snapshot is **~0.3 to 3.4 µs** | ~300x margin. Resolves O5 |
| Caching converts **O(traffic) into O(pods)** | Flag service goes from 2.4M RPS to ~0. Traffic could grow 100x unnoticed |
| Sticky bucketing is **computed, never stored** | 0 bytes at 1B users. Storing assignments would be ~16 GB *per flag* |
| Per-evaluation result caching is **rejected** | Evaluation is 0.3 µs; a cache lookup plus invalidation costs more than it saves |
| Availability **decouples** | Per-request RPC caps app availability at the flag service's. Cached, a flag-service outage degrades freshness only |
| Section A's divergence objection is **answered structurally** | One `internal/core` package compiled into both binaries, plus a golden-vector contract test. There is no second implementation to diverge |
| Startup must **not** block on the flag service | Otherwise it becomes a hard dependency of every deploy, fleet-wide. Accepted cost: a cold pod during a total outage serves compiled-in defaults |
| **No hard cache expiry** | Expiring converts a freshness problem into an availability outage. Named risk: kill switches will not propagate during a total outage |
| **Cache is filled by the write path, post-merge** | Cache update is the last stage of the merge pipeline — build, validate, then swap. Never lazy, never fill-on-miss |
| **Read path is cache-first with no fallback fetch** | An unknown flag is an *answer*, not a cache miss. The read path performs no I/O at all — invariant CACHE-3 |
| **Read-through caching rejected** | Fill-on-miss makes the first request after every change pay a fetch, and concurrent cold-key requests stampede the origin. At 2.4M evaluations/sec that is an outage, not a blip. Filling on the write path removes the failure mode instead of mitigating it |

**Entry condition.** O1–O4 closed.
**Exit criteria.** Every exported symbol has a stated contract, including its behaviour on nil, missing attribute, and wrong type.

> **VERIFICATION GATE 2** — you approve the contracts before any code exists.

---

## Phase 3 — Config Layer

**Objective.** Base + overlay layering, resolution to immutable per-environment snapshots, and validation.

| Deliverable | Detail |
|---|---|
| Schema types | Flag, Rule, Rollout, Layer, Snapshot with presence-aware `Opt[T]` fields |
| Merge engine | Scalar override, map deep-merge, `replace`-or-`append` for rule lists, deep-merge for `rollout` |
| `ConfigStore` | `Set(layer)` and `Get(flag, env)` over the resolved snapshot; holds raw unmerged layers for forensics |
| Validator | Rule table with reject-vs-warn severity, pre-merge and post-merge passes |
| **Post-merge cache update** | The final pipeline stage: build to completion, validate, then a single atomic swap. All-or-nothing per environment. Async L2 disk write after the swap, never before |
| Last-known-good | Invalid new version never replaces a serving snapshot — a rejection is a no-op on the cache, not a flush |

**Exit criteria addition.** A test proves no reader observes a partially-built snapshot
during a swap under concurrent load, and that a rejected config leaves the cache byte-identical.

**Exit criteria.** A prod overlay that raises a percentage and adds a rule while inheriting the base resolves correctly and is proven by test. An invalid overlay is rejected with the previous snapshot still serving.

> **VERIFICATION GATE 3**

---

## Phase 4 — Evaluation Core

**Objective.** The pipeline spine and the never-throw guarantee, before any targeting exists.

| Deliverable | Detail |
|---|---|
| Pipeline | Snapshot pin, flag lookup, enabled check, fallthrough, type check, reason |
| **Cache-first read** | Invariants CACHE-1 pin once per request, CACHE-2 build then swap, CACHE-3 read path performs no I/O. Enforced by test, not convention |
| Typed accessors | `BoolValue`, `StringValue`, `IntValue` |
| Panic boundary | Recover at every exported entry point, on the calling goroutine |
| Default resolution | The default must return even when config is entirely broken |

**Exit criteria.** A deliberately corrupted snapshot returns defaults for every call and logs structured errors. A fault-injection test panics inside the engine and the caller still receives a value.

> **VERIFICATION GATE 4**

---

## Phase 5 — Targeting Rules

**Objective.** Attribute matching with defined behaviour on the two cases real systems get wrong.

| Deliverable | Detail |
|---|---|
| Operator set | The subset justified in Phase 1, each with a type contract |
| Rule ordering | First match wins, rule id carried into the evaluation reason |
| Missing attribute | Defined, tested, documented — never an implicit true |
| Wrong-type attribute | Defined, tested, documented — no silent coercion |

**Exit criteria.** `country == "IN"` returns true for IN and the fallthrough value otherwise, and the absent-country case matches its documented behaviour.

> **VERIFICATION GATE 5**

---

## Phase 6 — Percentage Rollout & Sticky Bucketing

**Objective.** Implement O1 and O2 as decided.

| Deliverable | Detail |
|---|---|
| Hash | The stable non-cryptographic 64-bit hash chosen in Phase 1 |
| Bucketing | Subject to bucket space, with the granularity justified in Phase 1 |
| Strategy | `BucketKeyStrategy` implementation per O1 |
| Precedence | Single swappable stage per O2 |
| Missing subject | Anonymous request behaviour, defined and tested |

**Exit criteria.** Stickiness proven over repeated calls; independence proven across two flags at the same percentage; sharing proven when two flags opt into the same key; distribution within tolerance across a large synthetic population.

> **VERIFICATION GATE 6**

---

## Phase 7 — Live Update Propagation

**Objective.** Meet the under-5s budget with a measured number, not an assertion.

| Deliverable | Detail |
|---|---|
| Transport | The push / pull / hybrid choice from O4 |
| Snapshot swap | Atomic pointer swap, readers pinned for the life of one evaluation |
| Versioning | Monotonic generation counter, surfaced in response and logs |
| Miss detection | Reconnect, gap detection, backstop reconciliation |

**Exit criteria.** A measured end-to-end propagation test reports p99 well inside 5s. A concurrent test proves no torn reads during a swap under load.

> **VERIFICATION GATE 7**

---

## Phase 8 — Service Transport & Thin Client

**Objective.** Expose evaluation over the wire and give the app backend a client that cannot become the outage.

| Deliverable | Detail |
|---|---|
| Service API | Evaluate single and batch, health `/health` `/ready` `/live` |
| Trace propagation | Trace id through every hop into every log line |
| Thin client | Timeouts on every call, retry posture, fail-open stance |
| Cold start | Behaviour when the client has never fetched config |

**Exit criteria.** Killing the flag service leaves the app backend serving defaults within its stated timeout, with no thrown error and no latency cliff.

> **VERIFICATION GATE 8**

---

## Phase 9 — Observability

**Objective.** Make a 3am debug possible.

| Deliverable | Detail |
|---|---|
| Metrics | Evaluation count by flag and reason, latency histogram, default-fallback rate, propagation lag, config-apply failures |
| Cardinality guard | Flag name is a label. User id is never a label |
| Logs | Structured evaluation-error and config-apply schemas |
| SLOs and alerts | Target and alert condition per signal |

**Exit criteria.** For any past evaluation you can answer which config version served it and which rule or bucket decided it.

> **VERIFICATION GATE 9**

---

## Phase 10 — Test Suite & Benchmarks

**Objective.** Prove the brief's five mandated areas plus the failure paths.

| Area | Mandated by brief |
|---|---|
| Flag types | bool, string, int across every path |
| Targeting rules | match, no-match, missing attribute, wrong type |
| Percentage stickiness | repeat, independence, opt-in sharing, distribution |
| Environment isolation | prod change does not leak to dev |
| Default on error | corrupt config, panic injection, unknown flag, type mismatch |

Plus: concurrency race detector run, propagation-latency benchmark, evaluation p99 benchmark.

**Exit criteria.** `go test -race ./...` green. Benchmarks recorded as the baseline.

> **VERIFICATION GATE 10**

---

## Phase 11 — Go-Live

**Objective.** Hand it to on-call without a knowledge transfer meeting.

| Deliverable | Detail |
|---|---|
| `docs/04-runbook.md` | Rollout, rollback, and the alert-to-action map |
| Kill switch | How to disable a flag under incident conditions and how fast it takes effect |
| Re-bucketing warning | Changing the bucket strategy post-launch silently re-buckets every user mid-rollout. Documented as a change-controlled operation |
| Known limits | In-memory store, single region, config lost on restart |

> **VERIFICATION GATE 11 — go-live sign-off**

---

## Traceability

| Brief requirement | Phase |
|---|---|
| Flag types bool/string/int | 3, 4, 10 |
| Targeting rules | 5, 10 |
| Percentage rollout + sticky bucketing | 6, 10 |
| Independent vs shared buckets | 6 via O1 |
| Environments | 3, 10 |
| Live updates under 5s | 7 |
| Sync low-latency, never throws | 4, 8, 10 |
| Config store set/get | 3 |
| Unit tests, five areas | 10 |
| O1 bucketing key | 0 decision, 6 build |
| O2 precedence | 0 decision, 5 and 6 build |
| O3 misconfiguration | 0 decision, 3 build |
| O4 pull vs push | 0 decision, 7 build |

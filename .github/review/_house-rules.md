# Review house rules — read before any dimension prompt

You are reviewing a pull request against the **Feature Flag Service**, a Go service whose
entire product promise is *"an evaluation call never throws, and always returns in
microseconds"*. That promise is what makes the standards below non-negotiable rather than
stylistic.

## What you are reviewing

Only the **diff** on this pull request. Do not report pre-existing problems in untouched
code, and do not report missing features that a later phase is scheduled to build
(`docs/05-implementation-plan.md` is the phase plan). A finding must be caused by, or newly
exposed by, this diff.

## The bar for a finding

Report a defect only when you can state **a concrete input or interleaving that produces a
concrete wrong output, crash, or unbounded resource cost**. "This could be racy", "consider
adding validation", and "this might be slow" are not findings.

Every finding must carry:

- `file` and `line` — where a reader should look.
- `severity` — `blocking`, `major`, or `minor` (definitions below).
- `failure_scenario` — the input/state, then the wrong result. This is the field a reviewer
  uses to decide whether you are right, so it carries the argument.
- `verdict` — `CONFIRMED` if you traced the code path end to end, `PLAUSIBLE` if you are
  reasoning about a path you could not fully follow.

**Severity means:**

| Severity | Meaning |
|---|---|
| `blocking` | Serves a wrong flag value, loses config, panics to a caller, leaks PII or secrets, or breaks a documented compatibility surface. Merging causes a production incident |
| `major` | Correct today but the invariant is unenforced, so the next change silently breaks it. Or a real performance/memory regression on the hot path |
| `minor` | A trap for a future reader: a doc comment that claims a guarantee the code does not make, a misleading name, a sentinel inverted against its zero value |

Prefer **five confirmed findings over twenty speculative ones.** Precision is the product
here; a review that cries wolf gets ignored, and then it is worse than no review.

## Standards this repo actually commits to

These are not general Go advice. They are this codebase's stated contract, and a diff that
violates one is a finding regardless of how it reads.

**Dependency rings — imports point inward, never outward.**
`internal/core` is ring 0/1 and must import **nothing that performs I/O**: no logger, no
clock, no metrics registry, no `net/*`, no `os`. Errors are returned as data
(`Result.Reason`), never logged or panicked from there. This is what makes the never-throw
contract fuzzable rather than aspirational. `pkg/` must not import `internal/`.

**`recover()` at exactly two sites.** Both live in `internal/transport/safe`. A `recover()`
added anywhere else — especially in `internal/core` — is a finding. Note the Go rule that
makes this subtle: `recover()` returns non-nil **only when called directly by a deferred
function**, so hoisting a recover body into a helper that a closure calls silently returns
nil and lets the panic unwind.

**Concurrent map read/write in Go is a fatal runtime throw, not a panic** — `recover()`
cannot catch it, and it terminates the process. Any design that mutates a shared map after
publication has an uncontainable failure mode. Snapshots are built to completion and
published with one atomic pointer store.

**The three cache invariants.**
- `CACHE-1` — an evaluation pins the snapshot pointer **once at entry** and uses that same
  pointer for every flag in the request. Per-flag loading lets a config swap land
  mid-request, so flag A answers from generation N and flag B from N+1.
- `CACHE-2` — build then swap. Never mutate a published snapshot.
- `CACHE-3` — the read path performs **no I/O**: no network, no disk, no lock, no failable
  allocation. An unknown flag is an *answer*, not a cache miss.

**Absent is not false, and absent is not zero.** A missing context attribute makes a
condition false **before** negation is applied. Otherwise `country != "IN"` matches every
user on the planet the moment an upstream geo lookup fails. Wrong-type attributes are never
coerced.

**Bucketing inputs are a wire format, not an implementation detail.** The hash function, the
key composition, and the bucket mapping are pinned by golden vectors. Changing any of them
re-buckets every user in every active rollout simultaneously — at a 10% rollout roughly 18%
of users flip state while every dashboard stays green. `hash/maphash` is specifically
disqualified: its per-process seed reshuffles every rollout on every deploy.

**Metric cardinality is a hard boundary.** `flag`, `env`, `reason`, and closed Go enums are
acceptable labels. `user_id` is **never** a label — a billion series destroys the metrics
backend before the flag service notices. `rule_id`, `trace_id`, `session_id`, and bucket
values are response and log fields only, never labels. No `flag` label on a latency
histogram.

**Logs carry no PII and no secrets.** Attribute **keys** only, never attribute values.
Subjects appear as a short hash. Panic values are truncated; stacks appear as a digest. No
raw stack trace ever reaches a caller.

**Ambiguous config is rejected, never defaulted.** A flag carrying both rules and a rollout
with no explicit `evaluation_order` is refused at config time. Shipping a default there
decides a design question by accident and makes every later change a silent behavioural
migration across every flag.

**Fail-open is the design, and silence is its failure mode.** A degraded flag service makes
every flag read as its default; nothing errors and nobody is paged. Any code path that
swallows an error without incrementing a counter is a finding, because it removes the only
signal that the system is lying.

## Output contract

Write your findings to the JSON path given in your dimension prompt. Emit **only** valid
JSON — no prose, no markdown fence:

```json
{
  "dimension": "<your dimension slug>",
  "findings": [
    {
      "file": "internal/core/evalctx.go",
      "line": 30,
      "severity": "blocking",
      "category": "correctness",
      "short_summary": "null attribute reports present, defeating absent-guard",
      "summary": "One sentence stating the defect.",
      "failure_scenario": "Given X, the code returns Y; the contract requires Z.",
      "verdict": "CONFIRMED"
    }
  ]
}
```

An empty `findings` array is a valid and often correct result. Write the file even when you
find nothing — the aggregator distinguishes "clean" from "the job failed".

# Architecture Decision Records

An ADR records a decision that is **expensive to reverse** — one where the cost of
changing your mind later is paid in migrations, incidents, or re-bucketed users
rather than in a refactor. Decisions that are cheap to revisit do not need one;
they need a code review.

Every ADR here uses the same four sections: **Context** (the forces, with numbers),
**Decision** (what we are doing), **Consequences** (what it costs, including the
parts that hurt), and **Alternatives considered** (what was rejected and why —
written so that someone re-proposing the alternative in a year gets an answer
rather than a re-run of the argument).

## Index

| # | Decision | Status | Closes |
|---|---|---|---|
| [0001](0001-bucketing-key.md) | Bucketing key is a configurable namespace defaulting to the flag key | Accepted | **O1** |
| [0002](0002-rule-list-merge.md) | Rule lists merge by replace-or-append, never deep or key merge | Accepted | B1 |
| [0003](0003-absent-attribute-is-false.md) | An absent attribute makes a condition false, before negation | Accepted | C1 |
| [0004](0004-no-regex-operator.md) | No `REGEX` operator in the targeting rule set | Accepted | — |
| [0005](0005-xxhash-and-bucket-space.md) | xxhash64 is a wire format; the bucket space is 0..9999 | Accepted | C2 |
| [0006](0006-client-cached-snapshot.md) | The client caches a resolved snapshot and evaluates locally | Accepted | **O5** |
| [0007](0007-stdlib-only-metrics.md) | A `Metrics` interface with an expvar default, not a Prometheus dependency | Accepted | — |
| [0008](0008-evaluation-order.md) | Rules first, rollout on fallthrough — as an explicit required field | Accepted | **O2** |

O3 (misconfiguration handling: reject at config time **and** fail safe at
evaluation time, with per-flag quarantine) and O4 (propagation: push plus a 500 ms
heartbeat plus a 30 s reconcile poll) are closed in [`PLAN.md`](../../PLAN.md) and
designed in [`docs/02-hld.md`](../02-hld.md) §B and §D. They are candidates for
ADRs of their own if either is challenged.

## The three that are effectively immutable

These change what existing, unmodified config *means*. All three re-interpret
production behaviour with no diff to review, and none of them has a natural
detection signal — which is why each ships with a manufactured one.

| ADR | What silently changes | Manufactured detector |
|---|---|---|
| [0001](0001-bucketing-key.md) | Every user's bucket in every active rollout. At 10%, ~18% of users flip while the aggregate stays 10% | `bucketing_scheme_hash` gauge, paged on change; 1,000-subject golden vectors that fail the build **and refuse to boot** |
| [0005](0005-xxhash-and-bucket-space.md) | Same, via the hash function or the bucket space | Same golden vectors; the dependency is version-pinned with a reason |
| [0008](0008-evaluation-order.md) | Which of rules or rollout decides, for every flag carrying both | The field is required and explicit, so there is nothing to change silently |

**Before touching any of them, read the ADR and then talk to someone.** Reverting
the code restores the assignments; it does not restore the data written while the
change was live.

## Writing a new one

1. Copy the header table and the four sections from any existing ADR.
2. Number it sequentially. Numbers are never reused, and files are never deleted.
3. A superseded ADR keeps its file and its content; mark its status
   `Superseded by ADR-00XX` and link both ways. The record of a decision that was
   later reversed is more useful than the decision that replaced it.
4. Put numbers in the Context. "It is faster" is not a context; "208 ms p99 at
   F=100 against a sub-millisecond budget" is.

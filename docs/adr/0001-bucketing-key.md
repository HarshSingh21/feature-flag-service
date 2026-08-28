# ADR-0001: Bucketing key is a configurable namespace that defaults to the flag key

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-28 |
| **Decision id** | **O1** in [`PLAN.md`](../../PLAN.md) |
| **Source analysis** | [`docs/02-hld.md`](../02-hld.md) §C.4 · [`docs/03-lld.md`](../03-lld.md) §5 |
| **Implements** | `core.Rollout.BucketNamespace`, `core.BucketKeyStrategy` |
| **Related** | [ADR-0005](0005-xxhash-and-bucket-space.md) (the hash), [ADR-0008](0008-evaluation-order.md) (when the rollout runs at all) |

## Context

A percentage rollout must be **sticky**: the same subject must land in the same
bucket on every evaluation, across processes, across restarts, and across the
whole fleet. Stickiness is what makes "10% rollout" mean *a stable cohort of 10%
of users* rather than *a 10% chance per request*. Without it a user watches a
feature appear and disappear between page loads, and any metric computed over
the exposure window is meaningless because the exposed population changed
underneath it.

Stickiness is produced entirely by what goes into the hash. So the question "what
is the bucketing key?" is really the question "which users are correlated with
which other users, on which flags?"

The brief states the requirement precisely: the same user must get the same
bucket across different flags **only if the flag is configured to share the
bucketing key**. That word *only* rules out two of the four candidates outright.

| # | Strategy | Key bytes | Correlation across flags | Opt-in sharing |
|---|---|---|---|---|
| A | User ID only | `user_id` | Total — every flag at 10% hits the **same** 10% of users | Impossible to opt out |
| B | Flag key + user ID | `flag_key + 0x1F + user_id` | None — independent per flag | Inexpressible |
| C | Configurable salt defaulting to the flag key | `namespace + 0x1F + subject`, `namespace = rollout.bucket_namespace ?? flag.key` | Independent by default, correlated on request | **Yes, explicit and per flag** |
| D | C, plus a configurable subject attribute | `namespace + 0x1F + ctx[bucket_by]` | As C, orthogonally to the subject | Yes, plus tenant-level stickiness |

## Decision

**Adopt C, extended with D's configurable subject.** Concretely, on
`core.Rollout`:

- `bucket_namespace` — the hash salt. **Empty means "use the flag key."** Setting
  the same literal on two flags puts them in one bucket space deliberately. This
  is the brief's opt-in sharing, expressed as one config field.
- `bucket_by` — names the context attribute used as the bucketing subject.
  Empty means `user_id`. This is what makes a rollout expressible as *10% of
  tenants* rather than *10% of users* — for a B2B corpus those are wildly
  different, since 10% of tenants can be 60% of traffic.

Key composition is `namespace + 0x1F + subject`, hashed with the fixed hasher of
ADR-0005. `0x1F` (ASCII unit separator) is a delimiter that cannot occur in a
flag key or a user id, so `("ab", "c")` and `("a", "bc")` cannot collide.

**A missing bucketing subject is deterministic and never a random bucket.** The
alternatives all lose: random assignment destroys stickiness for exactly the
population that has none; hashing the empty string puts *all* anonymous traffic
in one bucket, so the rollout is 0% or 100% of it — a cliff, not a ramp, and
which one flips arbitrarily as the namespace changes; treating it as *in* fail-opens
a canary, inverting the safety property a canary exists to provide.

> **Unreconciled between documents.** `02-hld.md` §C.4 states a missing subject
> yields `ROLLOUT_OUT` with `Detail = no_bucketing_subject` — i.e. the rollout's
> **off value**. The shipped code takes the other branch: `internal/core` defines
> a distinct `ReasonMissingSubject`, classified as a **fallback** reason, and
> `Evaluator.Evaluate` returns the **caller's default** for it. Both are
> deterministic, so stickiness is unaffected either way, but they differ in what
> the caller receives and in whether the event feeds the fail-open alarm (hazard
> H1) — `Reason.IsFallback()` currently says it does, which means routine
> anonymous traffic inflates the signal that is supposed to detect a degraded
> client. **This must be closed before the first rollout ships.**

## Consequences

### Positive

- The brief's requirement is satisfied literally, by one field, with no code change.
- Independent-by-default is the safe default: one unlucky cohort does not absorb
  every canary of every flag in the company simultaneously.
- Correlation becomes a deliberate, reviewable, diffable act.

### Negative — the immutability trap, which is the reason this ADR exists

**`bucket_namespace` and `bucket_by` are immutable once a rollout has run in
production.** Not "discouraged" — immutable.

Changing either re-derives the key for every subject, so every subject is
independently reassigned. At a rollout of `p`, the fraction of users who cross
the boundary is `2p(1-p)`:

| Rollout | Users who flip state |
|---|---|
| 1% | 2.0% |
| 10% | **18.0%** |
| 50% | 50.0% |

**And the aggregate rollout percentage does not change.** It was 10% before and
it is 10% after. Every dashboard, every rollout gauge, every exposure count reads
exactly as it did. There is no organic signal. You learn about it days later from
user reports, and by then anything stateful the flag gated — a schema migration,
a UI opt-in, a partially written record — is inconsistent with the user's current
assignment.

Reverting the change restores assignments exactly, because bucketing is
deterministic. **It does not restore the data written during the window.** Budget
for the reconciliation, not just the revert.

### Detection must be manufactured

Because there is no natural signal, three mechanisms are mandatory, not optional
hardening:

1. **`bucketing_scheme_hash` gauge** — a hash of `{hash function, function
   version, namespace, key-composition rule, normalisation rule}`. **Alert on any
   change and page immediately.** It also appears in every `config.apply` log line
   alongside `prev_bucketing_scheme_hash`.
2. **Golden-vector build gate** — a fixed corpus of ~1,000 synthetic subject ids
   with their expected buckets, checked into the repo. A unit test asserts the
   mapping is unchanged, **and a startup self-check asserts the same**. Any diff
   fails the build and refuses to boot. See the checkbox in the PR template.
3. **Config-time rejection** — a rollout that has served traffic and whose
   namespace or subject changed is rejected by the validator, not warned about.

### The migration path, when one is genuinely needed

Treat the bucketing scheme as a **versioned, append-only, immutable contract**. A
new scheme gets a new scheme id and is opted into per flag. There is never a
global swap, and the namespace of a live flag is never edited in place.

## Alternatives considered

**A — user id only.** Rejected twice over. It cannot express opt-in sharing at
all (sharing is the only mode), and it is an operational flaw beyond the
experiment-design one: the same unlucky cohort is the guinea pig for every risky
change in the company, and their bad days compound.

**B — flag key + user id, hard-coded.** Fixes correlation but makes the stated
requirement inexpressible. Migrating B → C later is a reshuffle of every live
rollout, i.e. exactly the incident described above, so choosing B now buys
nothing and costs the expensive migration.

**D as a separate strategy rather than a field.** D is a superset of C rather
than a rival, so it is folded in as `bucket_by` rather than shipped as a second
strategy. The one thing D forces into the open is semantic: the config must make
the *unit* of rollout explicit so nobody reads "10%" as users when it is tenants.

**A strategy registry keyed by name** (`Rollout.StrategyName` resolving to a
registered `BucketKeyStrategy`), as sketched in the HLD. Retained as the
interface shape — `core.BucketKeyStrategy` is the plug point — but the shipped
default is C/D expressed as config fields, because a name that resolves to a
different implementation is precisely the silent-reshuffle failure mode with an
extra layer of indirection in front of it. An unknown strategy name is a
snapshot-build rejection, never a silent fallback.

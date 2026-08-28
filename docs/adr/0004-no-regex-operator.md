# ADR-0004: No REGEX operator in the targeting rule set

| | |
|---|---|
| **Status** | Accepted — revisit on the trigger below |
| **Date** | 2026-08-28 |
| **Source analysis** | [`docs/02-hld.md`](../02-hld.md) §C.3 |
| **Implements** | the `core.Operator` set in `internal/core/model.go` |

## Context

The shipped operator set is `EQUALS`, `IN`, `EXISTS`, `CONTAINS`, the ordered
numeric comparisons, and the semver comparisons. `REGEX` is the operator every
flag system eventually adds, and it is requested early because it appears to make
the operator set open-ended: any string predicate becomes expressible without a
schema change.

The usual argument against it — catastrophic backtracking, ReDoS — **does not
apply here, and this ADR concedes that up front.** Go's `regexp` is RE2. It has
no backtracking, and its match time is linear in the input. An adversarial
pattern cannot produce exponential blowup. Anyone rejecting regex in a Go codebase
on ReDoS grounds has not checked which engine they are using.

The objections that survive are different, and they are decisive.

## Decision

**Do not ship a `REGEX` operator.** The operator set stays small enough that a
config-time linter can type-check every operand against the attribute's declared
type, and small enough that a product manager can read a targeting rule.

## The three objections that stand

### 1. Compile lifecycle poisons snapshot builds

A regex cannot be compiled on the read path. `regexp.MustCompile` costs roughly
10 µs and allocates — against a whole-evaluation budget of ~0.3 µs, that is 30x
the budget for one condition, on every evaluation.

So it must be compiled at **snapshot build time**. Which means a malformed
pattern is no longer a bad rule that matches nothing — it is a **snapshot build
failure**. One flag with a bad regex fails the build for the entire environment,
and by [ADR-0006](0006-client-cached-snapshot.md) a failed build means the
previous generation keeps serving, so an urgent unrelated kill-switch change is
now blocked behind someone else's typo. Containing that requires per-flag
isolation in the builder — a whole quarantine mechanism, built to support one
operator.

### 2. Match cost is linear in an attacker-influenced length

RE2 being linear in input size is a guarantee about the *shape* of the cost
curve, not a bound on the cost. Targeting attributes include user agent, referrer,
and path — all attacker-influenced, and none of them length-bounded by default.

```
64 KB attribute, linear match   ~100 µs
whole evaluation budget            0.3 µs typical, 1 ms per request
```

That is ~30x the per-evaluation budget from a single condition, chosen by the
caller, at 2.4M evaluations/sec. Containing it requires a hard attribute-length
cap enforced at the transport boundary — another rule, remembered by nobody,
whose absence is invisible until it is an incident.

### 3. It is unreviewable in a config diff

`^(?:a|b)+c$` in a pull request gets rubber-stamped. `country IN ["IN", "US"]`
does not. Targeting rules are read and approved by people who are not the author,
often under time pressure, and a targeting rule is a production behaviour change
with the review rigour of a config edit. An operator whose semantics cannot be
verified by reading is an operator that will eventually ship the wrong cohort.

### And a fourth, which is really about product

Every regex in a real flag config is a `STARTS_WITH`, an `ENDS_WITH`, an `IN`, or
a genuinely missing operator that should be added explicitly. `REGEX` hides that
demand signal. Without it, the third team to need prefix matching asks for
`STARTS_WITH` and everyone gets a readable, type-checkable, constant-cost
operator. With it, they write a pattern and nobody ever learns what was needed.

## Consequences

### Positive

- Every operator has a bounded, predictable cost that does not depend on
  attacker-controlled input length.
- Snapshot builds cannot fail because of a *pattern*, only because of a *value*.
- Config diffs remain reviewable by non-authors.
- The operator set stays lint-checkable against declared attribute types.

### Negative

- Some genuinely irregular rule will be inexpressible, and someone will have to
  wait for an operator to be added rather than writing it themselves. That is the
  trade, stated plainly.
- `CONTAINS` ships as the pressure valve and is itself a compromise: it is a
  substring match that users mistake for a semantic one — `plan CONTAINS "pro"`
  also matches `"unprofitable"`. It ships with a config-time **lint warning**,
  not a rejection.

## Revisit trigger

Add `REGEX` when **three or more concrete rules** have been named that cannot be
expressed with the shipped operator set — concrete meaning a real config someone
wanted to write, not a hypothetical. Prefer adding the specific missing operator
first; `REGEX` is the answer only when the third one arrives and they have nothing
in common.

If it is added, it ships with all of: compiled at snapshot build with per-flag
build isolation, a hard input-length cap (1 KB) enforced before matching, no
`Longest` mode, and a per-pattern cost budget in the linter.

## Alternatives considered

**Ship `REGEX` with a documented "keep it simple" convention.** Rejected —
conventions are not enforcement, and the failure mode is invisible until it is
production.

**Ship a restricted glob syntax (`*`, `?`) instead.** Genuinely tempting: bounded
cost, readable, no compile failure class worth naming. Rejected for now only
because no rule has yet been named that `CONTAINS` plus an explicit prefix or
suffix operator would not cover more legibly, and a general string matcher with
overlapping semantics is a choice authors have to make rather than a capability
they gain. This is the first thing to reconsider if the revisit trigger fires.

> **Unreconciled between documents.** `02-hld.md` §C.3 ships `STARTS_WITH` and
> `ENDS_WITH` as part of the operator set, and splits ordered comparison into
> `NUM_*` and `SEMVER_*` families with `GTE`/`LTE` on both. The operator set
> actually declared in `internal/core/model.go` has neither prefix operator, and
> has `semver_eq` / `semver_gt` / `semver_lt` without the `-or-equal` variants.
> Nothing in this ADR turns on the difference, but the two should be reconciled
> before the operator set is treated as frozen.

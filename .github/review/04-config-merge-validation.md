# Dimension: config layering, merge, and validation

Slug: `config-merge-validation`. Write findings to `findings/config-merge-validation.json`.

Read `.github/review/_house-rules.md` first.

## The model being enforced

Config resolves through layers: a caller-supplied default in the calling binary, a base
layer (total record), a per-environment overlay (sparse patch), and a TTL-bound ops override
(whitelisted fields only). Layers merge into an immutable per-environment snapshot at
**write** time, because post-merge validation is only decidable after merging.

## What you are looking for

**Merge policy per field kind.** Each kind has one correct policy, and using another produces
a specific incident:
- *Identity* (name, type) — base only, immutable. An overlay declaring a type is rejected
  **even when it matches**.
- *Scalars* — higher layer wins when present; explicit null is rejected.
- *The rollout block* — **deep merge, never whole-block replace.** Replace lets
  `rollout: {percentage: 25}` blank the bucketing key, which reshuffles every user's bucket
  and flips enrolled users off during a routine percentage bump. This is a merge rule that
  produces an incident.
- *Maps* — per-key merge; per-key null deletes that key.
- *Ordered rule lists* — **whole-list replace or whole-list append, never element-wise.**
  With first-match-wins, the *order* is the semantics. Index merge silently re-pairs every
  overlay patch when a base rule is inserted at position 0; keyed merge defines content but
  not order, so you would have to simulate the merge to know production behaviour. Prepend
  is also excluded: a prepended overlay rule shadows every base rule, which is a replace
  wearing a disguise.

**Aliasing between layers or environments.** Deep copy must be unconditional. Two
environments must never share a backing array, slice, or map even when the resolved content
is byte-identical — interning saves single-digit megabytes and creates a class of bug where a
future in-place optimisation corrupts production from dev. A finding here is any shared
reference surviving the merge.

**Ambiguity defaulted instead of rejected.** A flag carrying both rules and a rollout with no
explicit evaluation order must be **refused**. Shipping a default decides a design question
by accident and makes any later change a silent behavioural migration across every flag.
Check the predicate used: a presence check (`Rollout != nil`) and an admits-anyone check
(`BasisPoints > 0`) are different questions, and using the latter for validation lets a
zero-percent rollout escape the ambiguity rule.

**Rejection is a no-op, not a flush.** An invalid new version must leave the previously
serving snapshot exactly as it was. Check that a validation failure cannot partially apply,
clear a cache, or advance a generation counter.

**Blast radius of a rejection.** Severity must be scoped: a base-layer failure blocks
everything, an environment failure keeps that environment on last-known-good while others
publish, and a single bad flag is quarantined rather than freezing its environment. Global
atomicity is explicitly *not* wanted — it buys "all environments agree", which is worthless
since environments are meant to differ, and costs "a production typo blocks an urgent dev fix".

**Silent rejection.** Every apply must return a report naming what was rejected and why. A
path that drops a flag without recording it is a finding.

**Provenance.** Which layer won each field must be recoverable for incident forensics. The
append operator is the one path by which a base change alters production behaviour without a
production edit — that needs to be visible.

# ADR-0008: Rules first, rollout on fallthrough — written as an explicit required field

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-28 |
| **Decision id** | **O2** in [`PLAN.md`](../../PLAN.md) |
| **Source analysis** | [`docs/02-hld.md`](../02-hld.md) §C.5 |
| **Implements** | `core.EvaluationOrder`, `core.Flag.EvaluationOrder` |
| **Related** | [ADR-0001](0001-bucketing-key.md) — closed together, see below |

## Context

A flag can carry both targeting rules and a percentage rollout. Something has to
decide which runs first, and the orderings are not variations on a theme — they
are different products:

| Ordering | Meaning | Reads as |
|---|---|---|
| **Rules first** | First matching rule returns immediately. The rollout runs only for subjects that fell through *every* rule | "these cohorts always get X; everyone else is in a ramp" |
| **Rollout gates** | The rollout decides enrolment first; rules only apply to enrolled subjects | "ramp the feature, and within the ramp target these cohorts" |
| **Rollout nested in a rule** | Each rule may carry its own rollout | "ramp separately within each cohort" |

The problem is not choosing between them. The problem is that **the same config
document is valid under all three and means something different under each.** A
flag with two rules and a 10% rollout is byte-identical JSON whichever ordering
the engine implements.

## Decision

**Two decisions, and the second matters more than the first.**

**1. The ordering is rules-first.** The first matching rule returns and stops; the
rollout runs only for subjects that fell through every rule. This matches how
people describe flags out loud ("internal users always on, then ramp everyone
else"), it keeps a rule's effect independent of the rollout percentage, and it
keeps the rollout's cohort stable when a rule is added.

**2. `evaluation_order` is an explicit, required field on any flag that has both
rules and a rollout. It has no default. A flag carrying both with the field absent
is REJECTED at config time.**

`core.EvaluationOrder` has exactly two values today: `OrderUnspecified` (the zero
value, legal **only** when the flag does not have both rules and a rollout) and
`OrderRulesFirst`. One legal value, and it is still written down.

## Why an explicit field rather than a default — this is the whole ADR

A default would work perfectly, today, for every flag, and cost nothing. It is
still wrong, for one reason:

**The orderings accept byte-identical config.** So if the ordering is implicit,
then changing it later — or shipping a second ordering because one team needs
rollout-gating — is not a feature addition. It is a **silent behavioural migration
across every flag that has both rules and a rollout, simultaneously, with no diff
to review.** Every affected config file is unchanged. Every dashboard aggregate is
plausible. The rules did not change, the percentage did not change, and the
resolved values did.

With the field required from day one:

- Every existing flag already carries `evaluation_order: rules_first`. A new
  ordering is opt-in per flag, in a diff, reviewed.
- The migration is a config change with an author and an approver, not a release
  note.
- A config file states its own semantics. You do not need to know the engine
  version to read it.

This is the same principle as [ADR-0001](0001-bucketing-key.md)'s scheme id and
[ADR-0005](0005-xxhash-and-bucket-space.md)'s wire-format treatment of the hash:
**anything that reinterprets existing config without changing it must be
versioned in the config, not in the code.**

It generalises to a rule worth stating: **ambiguous config is rejected, never
defaulted.** Shipping a default here would have decided O2 by accident, and
"decided by accident" is how a system acquires semantics nobody can defend.

### Why O1 and O2 were closed together

They interact. The rollout-nested-in-rules ordering raises the question of whether
two rollouts on one flag share a bucket space — which is an ADR-0001 question. Under
rules-first that question does not arise: **a rule and the rollout are mutually
exclusive for any given subject**, because the rollout only runs on fallthrough.
Rules-first is the ordering that keeps the two decisions independent.

## Consequences

### Positive

- The reason codes are unambiguous and directly readable:
  `RULE_MATCH` → a rule decided; `ROLLOUT_IN` / `ROLLOUT_OUT` → nothing matched and
  the ramp decided; `FALLTHROUGH` → nothing matched and there is no ramp. No
  composite state, and `core.Reason` stays low-cardinality and safe as a metric label.
- A rule's effect does not move when the rollout percentage changes, and the
  rollout's cohort does not move when a rule is added. Under rollout-gating, both
  are false — adding a rule changes who the rollout applies to.
- Ordering is one swappable stage behind an interface, so a second ordering can be
  added without restructuring the evaluator. Reversibility is *not* uniform,
  though: swapping rules-first for rollout-gating reinterprets identical config,
  which is exactly what the explicit field exists to prevent doing silently.

### Negative

- One more required field, and a rejection class that will confuse the first
  author who hits it. Mitigated by the rejection message naming the field, the two
  legal values, and this ADR.
- "Ramp the feature, and within the ramp target these cohorts" is not expressible
  today. It is a real use case, and it is deferred rather than denied — when it
  arrives, it arrives as a second `evaluation_order` value, per flag, in a diff.
- `OrderUnspecified` being the Go zero value means a `Flag` constructed in code
  rather than through the validator can carry an ordering that would have been
  rejected at config time. The validator, not the type system, is the enforcement
  point; that must stay covered by test.

## Alternatives considered

**Rollout gates, then rules.** Coherent and preferred by teams who think of a flag
as an experiment first and a targeting surface second. Rejected as the default
because it makes rule behaviour depend on the rollout percentage — an internal-users
rule stops working when someone lowers the ramp to 5%, which is astonishing.

**Rollout nested inside each rule.** The most expressive option, and the one that
drags O1 back open by asking whether sibling rollouts share a bucket space.
Deferred, not rejected on merit: it is a superset of rules-first and can be added
later as a per-rule optional rollout, once the flat case is proven.

**Ship all three implementations behind a registry and let config name one.** The
HLD's original shape (`rulesFirstResolver`, `rolloutGateResolver`,
`nestedRolloutResolver`, all conformance-tested). Narrowed here to one shipped
implementation with the *field* retained, because three live orderings triple the
behaviour matrix that every reviewer has to hold in their head, to serve use cases
that have not yet been named. The field is the part that had to exist on day one;
the implementations can follow whenever a real one is requested.

**Default to rules-first and document it.** The tempting option. Rejected for the
reason this ADR exists: the config would not say what it means, and any later
change to the default would rewrite the behaviour of every flag in the corpus
without a single line of diff.

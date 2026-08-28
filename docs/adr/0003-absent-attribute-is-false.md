# ADR-0003: An absent attribute makes a condition false, evaluated before negation

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-28 |
| **Source analysis** | [`docs/02-hld.md`](../02-hld.md) §C · [`PLAN.md`](../../PLAN.md) call C1 |
| **Implements** | `core.EvalContext.Attribute`, `core.Condition.Negate`, `core.OpExists` |

## Context

A targeting rule matches on request attributes — `country`, `plan`,
`app_version`. Those attributes come from upstream systems that fail: a geo
lookup times out, an entitlements call is degraded, a new caller simply does not
populate the field yet. **An attribute being absent is a normal condition, not an
exception**, and it happens most often precisely when the system is already
unhealthy.

So the evaluator needs a defined answer for `attribute is absent`, and — this is
the part that is usually missed — a defined *interaction with negation*.

## Decision

Two rules, in this order:

1. **If the attribute is absent from the evaluation context, the condition is
   `false`.** No coercion, no default value, no "treat as empty string".
2. **Negation is applied afterwards, to the result of the whole comparison** —
   and an absent attribute short-circuits to `false` **before** negation, so a
   negated condition on an absent attribute is also `false`.

Consequently `Negate` is a boolean field on `core.Condition`, not a family of
`NOT_EQUALS` / `NOT_IN` operators.

`core.EvalContext.Attribute` returns `(Value, bool)` where the second return
distinguishes **absent** from **present-but-zero**. `Value{}` (the zero value) is
`TypeUnknown`, not `""`, so a present-but-empty string is genuinely different
from a missing attribute — and both are different from a present integer zero.

`OpExists` is the only correct way to write "attribute is present", and it exists
because rule 2 makes `!=` unable to mean it.

## The worked example — why this is not a style preference

A rule that should target everyone outside India:

```yaml
- id: r-not-india
  conditions:
    - attribute: country
      op: eq
      values: ["IN"]
      negate: true          # country != "IN"
  value: true
```

The geo lookup fails. `country` is absent. Under the two obvious alternatives:

| Semantics | `country != "IN"` with `country` absent | Blast radius |
|---|---|---|
| Absent is empty string | `"" != "IN"` → **true** | Rule matches **every user on the planet** |
| Negate the "attribute missing" outcome | `NOT(false)` → **true** | Same |
| **This ADR** | absent → `false`, negate does not run → **false** | Rule matches nobody; evaluation falls through to the next rule |

The first two ship a feature to 100% of traffic because a geo lookup was slow.
Nothing errors, no exception is thrown, no alert fires — the flag *worked*, it
just answered a different question than the author asked. This is the single most
common way a targeting system causes an incident.

The chosen semantics fail in the safe direction: a rule whose input is missing
does not match, so the flag falls through to its next rule, its rollout, or its
own default. A rollout is an *addition* of new behaviour; declining to add it for
a subject you cannot evaluate is the conservative choice.

## Consequences

### Positive

- The absent-attribute rule is written **exactly once**, at a single point in the
  condition evaluator. It cannot drift between an operator and its negated twin.
- The operator table stays half the size, and so does its truth-table test matrix.
- The failure direction is safe and, more importantly, *the same in every case*.

### Negative

- "Everyone except X" is not expressible as a single negated condition when the
  attribute may be absent. It requires two conditions: `country EXISTS` AND
  `country != "IN"`. That is more verbose — and it is honest, because it forces
  the author to state what should happen when `country` is unknown.
- Rule authors coming from systems with the other semantics will write the
  one-condition version and get a rule that quietly matches nobody. Mitigation:
  the config-time linter warns on a negated condition with no sibling `EXISTS`
  guard, and a `flag_eval_attribute_absent_total{flag_key,attribute}` counter
  makes the case visible rather than silent.

### Neutral

- A **present but wrong-typed** attribute is a separate case with the same
  answer: the condition is false, no coercion is attempted, and
  `flag_eval_attribute_type_mismatch_total{flag_key,attribute}` is incremented.
  `String("true")` is not a bool and `String("1")` is not an int — enforced in
  `core.Value` and covered by `TestValueNeverCoercesAcrossTypes`.

## Alternatives considered

**Absent coerces to the type's zero value.** Rejected: it makes `plan == ""`
match every request with a missing plan, and it is indistinguishable in the
config from an author who genuinely meant the empty string.

**Absent makes the condition `unknown`, with three-valued logic.** Defensible and
rejected. Kleene logic answers the negation question correctly, but it forces
every combiner (`AND`, `OR`), every rule outcome, and every reason code to carry
a third state, and it makes flag behaviour something a PM can no longer read off
the config. The cost lands on every rule to fix a case that the `EXISTS` operator
already handles explicitly.

**Absent raises an error and returns the caller default.** Rejected for the same
reason as the missing-bucketing-subject case in [ADR-0001](0001-bucketing-key.md):
this is a *routine* condition on any public endpoint. Turning it into an error
destroys the usefulness of the error rate as a signal, which costs more than it
buys.

**`NOT_EQUALS` and `NOT_IN` as first-class operators.** Rejected. It doubles the
operator table and, worse, hides the absent-attribute question inside each
operator's implementation — where it will be answered one way in `NOT_EQUALS`,
another way in `NOT_IN`, and a third way in whichever operator is added next.

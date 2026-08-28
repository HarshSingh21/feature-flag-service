# ADR-0002: Rule lists merge by replace-or-append, never by deep merge or key merge

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-28 |
| **Source analysis** | [`docs/02-hld.md`](../02-hld.md) §B · [`PLAN.md`](../../PLAN.md) call B1 |
| **Implements** | `core.Rule`, the `internal/config` merge engine |
| **Related** | [ADR-0008](0008-evaluation-order.md) — first-match-wins is what makes order semantic |

## Context

Configuration is layered Helm-style: a base layer, a per-environment overlay, and
a narrow ops override. Every field kind needs a defined merge rule. Scalars are
last-writer-wins and maps deep-merge; both are uncontroversial.

Rule lists are not, because **rules are evaluated in slice order and the first
match wins**. The order of the slice is not presentation. It is the semantics. A
merge rule for rule lists is therefore a rule about how production behaviour is
computed, not about how two documents are combined.

Four candidate strategies exist:

| Strategy | What it defines | What it leaves undefined |
|---|---|---|
| **Replace** | Content and order | nothing |
| **Append** | Content and order | nothing |
| Index merge (patch element *i*) | Content per position | nothing — but positions are unstable |
| Key merge (patch by `rule.id`) | Content per rule | **order** |

## Decision

**A rule list in an overlay carries an explicit operator: `replace` or `append`.
Nothing else is legal. `prepend` is not offered.**

- `replace` — the overlay's list *is* the resolved list. The base's rules are gone.
- `append` — the resolved list is base rules in base order, then overlay rules in
  overlay order.

Deep merge, index merge, and key merge are rejected outright. There is no
"default" merge for a rule list: an overlay that supplies rules without an
operator is a config-time rejection.

## Consequences

### Positive

- **The resolved rule order is readable from the two documents without running
  the merge engine.** This is the entire point. The question "what will prod
  actually do?" is answered by reading, not by simulating.
- Both operators are total: given base and overlay you can write down the result
  by hand, every time, with no knowledge of the merge implementation.
- `append` gives the common case — prod adds a targeting rule on top of the
  shared base — without duplicating the base list into every overlay.

### Negative

- Adding one rule to a `replace` overlay means restating the base rules. That
  duplication is real, and it is the price of the resolved list being explicit in
  the document you are reading.
- `append` is the one path by which a change to the **base** alters **prod**
  behaviour without any prod edit. This is intended semantics — it is why `append`
  exists — but it must be visible: the resolved snapshot carries a per-rule
  provenance table (which layer contributed this rule), and base changes are
  change-reviewed on that basis. Recorded as hazard H7 in `PLAN.md`.

### Neutral

- `prepend` was deliberately not shipped even though it is expressible. Once both
  `append` and `prepend` exist, an overlay can straddle the base and the resolved
  order stops being reconstructible by reading top to bottom.

## Alternatives considered

**Index merge — overlay element *i* patches base element *i*.** Fails on the most
routine base edit there is. Insert a rule at position 0 of the base and every
overlay patch silently re-pairs with a different rule. Nothing errors; the config
is still valid; every environment now targets differently. The failure is
invisible in both diffs, because neither document changed in a way that looks
related to the other.

**Key merge — overlay patches base rules by `rule.id`.** This is the sophisticated
option, and it is the trap. It defines *content* completely and *order* not at
all. Where does a rule that exists only in the overlay go — before the base rules,
after them, at the position of the id it most resembles? Every answer is
arbitrary, and the consequence is that **you have to run the merge to know what
production will do**. That converts a reviewable diff into a simulation exercise,
during an incident, at 3am. First-match-wins plus undefined ordering is not a
merge strategy; it is a coin flip with a schema.

**Deep merge of the list as if it were a map.** Same objection as key merge, with
the additional problem that a partially-patched rule (`conditions` from base,
`value` from overlay) is a rule no author ever wrote and no reviewer ever read.

**Defining "most specific rule wins" and dropping order entirely.** Specificity is
undefinable across heterogeneous operators — is `country IN [IN, US]` more or less
specific than `plan == "pro"`? Any definition is a second ordering source that
will eventually disagree with the array, and then there are two answers.

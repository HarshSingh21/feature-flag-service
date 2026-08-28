# Dimension: correctness and contracts

Slug: `correctness-contracts`. Write findings to `findings/correctness-contracts.json`.

Read `.github/review/_house-rules.md` first. It defines severity, the output schema, and the
standards this repo commits to.

## What you are looking for

**Never-throw violations.** Any path in the diff that can return an error to a caller,
panic outside the two sanctioned `recover()` sites, or fail to produce a value. The contract
is that a caller always receives *something* — the configured value, or its own default.
Trace the failure path, not the happy path.

**Absent versus present-but-zero.** The single highest-value bug class in this codebase.
- A missing attribute must make a condition false **before** negation applies.
- An explicit JSON `null` must not read as "present".
- An empty string in a subject field must not read the same as a populated one *in one
  spelling and differently in another*. If a context offers two ways to express the same
  thing (a struct field and a map entry), they must agree — check that they do.

**Type safety with no coercion.** `"true"` is not `true`. `"1"` is not `1`. A wrong-type
attribute makes a condition false and increments a counter; it never converts.

**Tri-state presence.** Config overlays distinguish *absent* (inherit), *explicit null*
(unset), and *explicit value* (override). A pointer field collapses the first two and is a
finding. Check that a sparse patch type is genuinely sparse and a total record is genuinely
total — reusing one struct for both is named in the design as the most common way this class
of system rots.

**Enum round-trips.** For every enum touched by the diff: is every constant present in both
`String()` and the corresponding `Parse*`? A constant added to the `const` block but not to
both functions serialises as `"unknown"` and parses as a zero value — silently, forever.
A hand-written table test does not catch the *next* addition; only an exhaustive loop over
the enum does. If the diff adds a constant without an exhaustive test, that is a finding.

**Sentinels versus zero values.** A sentinel like `NoBucket = -1` is inverted against its
type's zero value: an unset field reads as bucket 0, which is a real bucket inside every
non-empty rollout. Any struct field whose zero value is a *valid but wrong* answer is a
finding when nothing forces construction through a helper.

**Doc comments that overclaim.** A comment asserting a guarantee the code does not make is
a `minor` finding at least — it stops the next reviewer from checking. Specifically: a
comment naming a test that does not exist, or claiming an invariant is "enforced by test"
when no such test is present.

## What not to report

Missing features scheduled for a later phase. Style. Naming, unless the name actively
misleads about behaviour. Error-message wording, unless it tells the reader something false.

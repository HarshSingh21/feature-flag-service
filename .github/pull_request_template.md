## What this changes

<!-- One paragraph. What behaviour is different after this merges? -->

## Why

<!-- The problem, not the patch. Link the phase in PLAN.md or the ADR this implements. -->

## How it was verified

<!-- Which test proves it? "I ran it locally" is not a verification. -->

---

## Merge gates

Every box is a gate, not a formality. An unchecked box means this is a draft.

- [ ] `make ci` passes locally (`fmt-check`, `vet`, `build`, `test`)
- [ ] `go test -race ./...` is clean — no data races, no skipped race-sensitive tests
- [ ] `gofmt -s -l .` prints nothing
- [ ] New behaviour has a test that fails without the change
- [ ] Error paths are tested, not just the happy path
- [ ] No new dependency, or a new dependency is justified in the description

## Design gates

- [ ] An ADR was added under `docs/adr/` for any architectural decision, or this
      change makes no architectural decision
- [ ] The change respects the dependency rings — `internal/core` imports nothing
      that performs I/O (no logger, no clock, no `net/*`, no `os`)
- [ ] No `recover()` was added inside `internal/core`; panic containment stays at
      the two sites in `internal/transport/safe`
- [ ] Exported symbols document their behaviour on nil, missing attribute, and
      wrong type

## Bucketing — read this one

Changing any bucketing input silently re-buckets **every user in every active
rollout**. At a 10% rollout roughly 18% of users flip state while the aggregate
percentage — and therefore every dashboard — stays exactly the same. There is no
organic detection signal. See [ADR-0001](docs/adr/0001-bucketing-key.md) and
[ADR-0005](docs/adr/0005-xxhash-and-bucket-space.md).

- [ ] **The bucketing golden vectors were NOT changed.** No edit to the checked-in
      `(key, expected_bucket)` fixtures, the hash function, the key composition,
      the bucket space, or the string normalisation applied before hashing.

<!--
If you genuinely need to change them, this PR is not the place. A bucketing
scheme change requires: a new scheme id (never an in-place edit), per-flag opt-in,
an ADR, and a named owner for reconciling any state written while the old scheme
was live. Reverting the code restores assignments; it does not restore the data.
-->

## Operability

- [ ] New failure modes are observable — a metric, a log line, or a reason code
- [ ] No user id, tenant id, or rule id was added as a metric label (cardinality)
- [ ] No secrets or PII in logs

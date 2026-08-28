# Dimension: bucketing and compatibility surfaces

Slug: `bucketing-compatibility`. Write findings to `findings/bucketing-compatibility.json`.

Read `.github/review/_house-rules.md` first.

## Why this dimension exists

Percentage rollouts are sticky: the same subject must get the same answer forever. That makes
**the hash function, the key composition, and the bucket mapping a wire format** — a
compatibility surface with users, not an implementation detail.

Changing any of them re-buckets every user in every active rollout at once. At a 10% rollout
roughly `2p(1-p)` = **18% of users flip state**, while the aggregate percentage stays at 10%
and every dashboard stays green. There is no error, no alert from the numbers themselves, and
no way to undo it for users who already saw the other variant.

## What you are looking for

**Any change to a bucketing input.** The hash algorithm, its seed or salt, the order or
separator of concatenated key parts, the normalisation applied to the subject, the bucket
space size, the comparison direction. Every one of these is `blocking` unless the diff also
carries updated golden vectors *and* an explicit acknowledgement field or migration.

**Non-deterministic hashing.** `hash/maphash` is specifically disqualified — its per-process
random seed reshuffles every rollout on every deploy. Also: Go map iteration order anywhere
in key construction, `time`-dependent input, pointer values, or anything not derived purely
from the subject and the configured namespace.

**Monotonicity.** Raising a rollout percentage must only ever *add* users. Any scheme that
mixes the percentage into the hash (`hash(key + percentage)`) destroys this: every ramp
reshuffles the entire population, so users who had the feature lose it. Check the threshold
comparison, not just the hash.

**Bucket space and boundaries.** Basis points give 0..9999. Check: is the comparison
`bucket < threshold` (correct) or `<=` (off by one basis point)? Is a 0% rollout guaranteed
to admit nobody and 100% to admit everybody? Is the mapping from a 64-bit hash to the bucket
space done by multiply-shift rather than modulo, and if modulo, is the bias argued?

**Missing subject.** A rollout with no bucketing subject in the context must have one defined
behaviour. Hashing the empty string is a finding: it is deterministic *and* arbitrary, so
every anonymous request lands in one bucket, making the rollout 0% or 100% for all of them
and flipping the moment the namespace changes.

**Namespace semantics.** An empty namespace means "independent per flag"; a shared literal
means "deliberately correlated". Check the default cannot silently correlate two flags that
were meant to be independent, and that the resolved namespace is what gets hashed.

**Golden vectors.** Does the diff change behaviour that the committed vectors pin? If vectors
are absent for a bucketing change, say so — the design commits to 500 key/bucket pairs
including the *composed key*, plus a 1000-ID stability test that fails the build and refuses
boot on a diff.

## Severity guidance

Treat any unacknowledged change to a bucketing input as `blocking` even when the code is
"more correct" than before. Correctness does not help a user who already saw the other
variant.

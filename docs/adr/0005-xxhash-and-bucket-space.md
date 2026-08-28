# ADR-0005: xxhash64 is a wire format; the bucket space is 0..9999

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-28 |
| **Source analysis** | [`docs/02-hld.md`](../02-hld.md) §C.4 · [`docs/03-lld.md`](../03-lld.md) §5.1 |
| **Implements** | `core.Hasher`, `core.BucketSpace`, `github.com/cespare/xxhash/v2` |
| **Related** | [ADR-0001](0001-bucketing-key.md) — what gets hashed |

## Context

Sticky bucketing turns a key into a bucket. Two things must be chosen: the hash
function and the size of the bucket space. Both look like implementation details.
Neither is.

**The hash output is a persisted semantic.** A 10% rollout means *this specific
10% of users*. If the hash output changes for any reason — different function,
different library, different Go version, different architecture, different
process — the membership of that 10% changes, and:

- users in the experiment silently leave it, invalidating every metric computed
  over the exposure window;
- users lose a feature mid-session — a bug report for a UI feature, potential
  data corruption for a migration flag, where a user writes the new format on one
  request and the old format on the next;
- a canary that looked healthy at 1% is now a *different* 1%, so it proved nothing.

## Decision

**Hash: xxhash64, via `github.com/cespare/xxhash/v2`, treated as a wire format
rather than an implementation detail.**

**Bucket space: 10,000 — `core.BucketSpace`. Buckets are `0..9999`, giving
basis-point (0.01%) granularity.** Rollout size is expressed in basis points
(`core.Rollout.BasisPoints`), not whole percent.

**Mapping: multiply-shift (Lemire), consuming the high 32 bits.**

```go
const BucketSpace = 10_000

func bucketOf(h uint64) uint32 {
    hi := h >> 32                          // < 2^32
    return uint32((hi * BucketSpace) >> 32) // in [0, 10000)
}

// Strictly less-than. This is the monotonicity guarantee.
func inRollout(bucket, basisPoints uint32) bool { return bucket < basisPoints }
```

**Inclusion is strictly `bucket < basisPoints`, and the bucket does not depend on
the percentage.** This is what makes a ramp monotone: raising a rollout from 10%
to 20% only ever *adds* users and never removes one. Any scheme of the form
`hash(key + percentage)` destroys that property — every ramp becomes a complete
reshuffle, and users lose the feature they had five minutes ago.

## Alternatives considered

| Candidate | Speed, short key | Uniformity | Cross-version stability | Verdict |
|---|---|---|---|---|
| **xxhash64** | ~1 ns/byte; ~12–15 ns for a 40-byte key, zero alloc | Excellent avalanche across all 64 bits; passes SMHasher | Published spec, fixed constants, output is a function of bytes only. Stable across Go versions, architectures, and the library's major version | **chosen** |
| `hash/maphash` | fast | excellent | **randomly seeded per process** | **rejected — and named as the trap** |
| FNV-1a (`hash/fnv`) | ~2–3 ns/byte | **weak low-bit diffusion** | stable | rejected |
| MD5 / SHA-1 | ~200–400 ns, allocates | excellent | stable | rejected |
| CRC32 | very fast | 32-bit output, visible clustering on structured keys | stable | rejected |

### `hash/maphash` — the disqualification worth spelling out

**A reviewer will suggest it, and they will be right about everything except the
one thing that matters.** It is stdlib, it is fast, it has excellent distribution,
and it removes a dependency. It is also **seeded with a per-process random value**.

That means the bucket for a given user is different in every process, and
different again after every restart. Concretely:

- Two pods in the same deployment disagree about whether a user is in a rollout,
  so the user's experience depends on which pod the load balancer picked — the
  feature flickers on refresh.
- **Every deploy reshuffles every rollout in the entire system**, silently, with
  the aggregate percentages unchanged. This is the ADR-0001 re-bucketing incident
  fired automatically on every release.

`maphash` is designed to be unpredictable, specifically to prevent hash-flooding
attacks on Go maps. That design goal is the exact opposite of what bucketing
needs. It is not a close call; it is a category error.

### FNV-1a — rejected on the exact bits we read

FNV-1a is byte-wise XOR-then-multiply, which propagates changes *upward*. Its low
bits are close to an XOR of the input's low bits — and `h % 10000` reads precisely
those bits. At 1B users and 100,000 users per basis point, sampling error is
irrelevant; **the risk at this scale is hash quality, not sample size**, so a
function with a known weakness in the range we consume is the wrong trade for
saving a dependency. It is fixable by xor-folding the high half down, but that is
a hand-rolled step that will be dropped by the first person who "simplifies" it.

### MD5 / SHA-1 — rejected on cost for a property not needed

15–30x the cost of the entire rest of an evaluation. Bucketing is not a security
boundary; nobody gains anything by predicting their own bucket.

### CRC32 — rejected on output width

32 bits with visible clustering on structured keys, against a 10,000-bucket space
built from structured keys (`namespace + 0x1F + user_id`).

### Bucket space alternatives

| Space | Granularity | Verdict |
|---|---|---|
| 100 | 1% | Rejected. A 0.5% canary is inexpressible, and widening the space later is a full reshuffle |
| **10,000** | **0.01% (1 basis point)** | **chosen** — 100,000 users per bucket at 1B users |
| 1,000,000 | 0.0001% | Rejected. No named use case, and 1,000 users per bucket starts making distribution noise visible |

10,000 is the smallest space in which a fractional-percent rollout is expressible
without a later breaking change, which matters because **changing the bucket
space after launch re-buckets everyone** — the same incident as changing the key.

`h % 10000` would also be acceptable in isolation (modulo bias at 2^64 is ~5e-16
relative), but multiply-shift avoids the division and consumes the high bits,
where xxhash's avalanche is strongest.

## Consequences

### Positive

- Deterministic and identical across processes, machines, architectures, Go
  versions, and both binaries — the server and the client link the *same*
  `internal/core`, so there is no second implementation to diverge.
- Zero-allocation on the hot path: the key is composed into a stack scratch
  buffer and hashed, ~15 ns for a typical key.
- Monotone ramps, so raising a percentage never removes a user.

### Negative — the obligations this creates

Treating the hash as a wire format means it comes with compatibility obligations,
not just a `go.mod` line:

1. **Golden test vectors are checked in** — `(key, expected_bucket)` pairs, plus
   the ~1,000-subject assignment-stability corpus of ADR-0001. Any change to the
   function, the key composition, the bucket space, or the string normalisation
   fails the build **and refuses to boot**.
2. **The dependency is version-pinned with a comment naming the reason**, and
   covered by a `go.mod` review rule. `go get -u` is not a routine operation on
   this dependency.
3. **A change requires a versioned scheme id and a per-flag migration**, never a
   patch release. See ADR-0001's migration path.
4. One external dependency in a codebase that otherwise uses only the stdlib.
   Accepted deliberately: the stdlib's two candidates are disqualified on
   correctness (`maphash`) and on distribution in the bits we consume (`fnv`).

# `test/load` — load and throughput benchmark suite

Specification: [`docs/05-consistency-and-e2e.md` §3](../../docs/05-consistency-and-e2e.md).
Claims under test: [`docs/03-lld.md` §2 and §4](../../docs/03-lld.md).

This suite exists to substantiate the design's performance claims **or falsify them
honestly**. Nothing in it is tuned to pass. `TestPassCriteria` fails the build when a
criterion is missed, because §3.3 says a missed criterion is a finding about the
design, not a test to relax.

---

## How to run it

```sh
# The whole thing: every scenario, every criterion, one PASS/FAIL table.
go test ./test/load/ -run TestPassCriteria -v

# Longer runs = tighter tails. Use this before quoting a p999 anywhere.
go test ./test/load/ -run TestPassCriteria -v -load.duration=10s

# Just the memory curve.
go test ./test/load/ -run TestL6SnapshotMemory -v

# The testing.B view: ns/op, allocs/op, B/op, evals/sec, ns/eval.
go test ./test/load/ -run '^$' -bench . -benchtime 2s

# One scenario.
go test ./test/load/ -run '^$' -bench L2 -benchtime 5s -count 10 | tee l2.txt

# Correctness of the harness itself. These two DO run under -race.
go test -race ./test/load/ -run 'TestCorpusResolves|TestHarnessUnderChurn' -v
```

`TestCorpusResolves` and `TestHarnessUnderChurn` are the guard on everything else. A
batch of 100 keys that are all missing from the snapshot returns 100 `FLAG_NOT_FOUND`
results very fast indeed, and every number in this suite would then be a number about
a map miss. They assert that every flag resolves to a configured value, that the
worst-case flag falls through all 20 rules to its rollout rather than matching early,
and that a batch reads exactly one generation while a writer swaps underneath it. They
assert on behaviour rather than timing, so they are the only part of the suite that
gets stricter under `-race` instead of meaningless.

Flags:

| flag | default | meaning |
|---|---|---|
| `-load.duration` | `2s` | wall-clock duration of each percentile scenario (L2–L5) |
| `-load.churn` | `100ms` | L4's config swap interval |
| `-short` | off | 250 ms scenarios, L6 skips the 20k point. Keeps `go test ./...` quick |

**`-race` is refused, deliberately.** Every number here is a latency, a throughput or
an allocation count, and a race-instrumented binary reports ThreadSanitizer's numbers
rather than the design's. `TestPassCriteria`, `TestL6SnapshotMemory` and the parallel
benchmarks skip with an explanatory message, so `go test -race ./...` stays green and
stays honest.

---

## What each scenario proves

| # | Scenario | The question it answers |
|---|---|---|
| **L1** | one `BoolValue` on a cached snapshot, no contention | What does an evaluation cost at the floor, and does the read path allocate? |
| **L2** | batch of 100 flags, one snapshot pin | **The headline.** Is a p99 request at S7's 100-flag shape inside the 1 ms budget? |
| **L3** | L2 across `GOMAXPROCS` goroutines | Does the read path contend? What does one box actually achieve? |
| **L4** | L3 with a writer swapping the snapshot every 100 ms | **The load-bearing one.** Is an atomic swap invisible to readers, and is the garbage from discarded generations affordable? |
| **L5** | 20 rules × 4 conditions, batch of 100 | The pathological corner. Does the ~8-core figure hold? |
| **L6** | snapshot resident memory at 1k / 5k / 20k flags | Does the ~6 MB at 5k claim hold, and what is the per-pod ceiling? |

### How the corpus is built

L1–L4 run against a 1,000-flag environment shaped like a real one: **20% percentage
rollouts, 20% two-rule targeting, 60% plain enabled flags**. Same mix as
`pkg/client/bench_test.go` so the two suites are comparable. A corpus of only plain
flags measures a map lookup and calls it an evaluation.

L5's flag is the true worst case, not merely a big one. Under an `AND` combiner the
first false condition short-circuits, so a rule whose *first* condition fails costs a
quarter of what it looks like it costs. Conditions 1–3 are built to match and
condition 4 to fail, so all 80 conditions are actually evaluated; `IN` puts the
matching value last in an 8-element list; the flag then falls through to a rollout.

---

## How to read the output

```
L2  batch of 100, single goroutine, no churn
  workers          1
  duration         5.001s
  iterations       630944 ops (630944 samples, 100 evaluations/op)
  latency  p50     7.50 µs
           p99     15.17 µs
  throughput       126.2k ops/sec = 12.62M evaluations/sec
  per evaluation   79 ns (mean)
  gc               NumGC=198 totalPause=7.265ms maxPause=296µs
```

- **`ops` are requests, `evaluations` are flags.** `docs/03-lld.md` §1: "the unit of
  load is evaluations, not requests — this is the number most capacity plans get
  wrong." Both are reported so neither can be quoted as the other.
- **Percentiles come from a recorded-latency harness, not from `testing.B`.**
  `testing.B` reports a *mean*; P1 and P3 are p99 criteria and a mean cannot be turned
  into one. The cost is two `time.Now()` calls inside the timed region (~14 ns on this
  box, reported at the top of every run). It is **not subtracted** — subtracting a
  constant you estimated is how a benchmark starts measuring its own assumptions.
- **`max` is not a design number on a shared machine.** On a laptop the maximum is an
  OS scheduling event or a stop-the-world pause, not the read path. Read p99 and p999.
- **L6 measures retained heap** (`MemStats.HeapAlloc` delta after forced GC), **not
  RSS.** Runtime overhead, fragmentation, stacks and unreturned spans are excluded. It
  is the number to check a "~6 MB snapshot" claim against, not the number to size a
  container limit from.
- Every benchmark feeds its result into a package-level sink. A benchmark whose result
  is unused can be optimised away entirely and will report an impossibly fast number.

---

## Results measured on this machine

```
cpu           Apple M5
NumCPU        10   (GOMAXPROCS 10)
go            go1.26.3
platform      darwin/arm64
race detector false
```

**These are single-machine numbers.** Fleet figures below are *extrapolated* by linear
scaling. Nothing here measures a fleet, and linear scaling is an assumption, not a
result: it ignores per-pod `GOMAXPROCS` limits, noisy neighbours, and the fact that a
real pod spends most of its cores on the application, not on flag evaluation.

### Pass criteria

`-load.duration=5s`; 630,944 samples at L2, 3,406,752 at L3, 3,319,680 at L4.

| # | Criterion | Target | Measured | |
|---|---|---|---|---|
| **P1** | L2 p99, batch of 100 | < 1.000 ms | **15.17 µs** — 66× margin | ✅ PASS |
| **P2** | L1 allocations/op, plain flag | = 0 | **0 allocs/op, 0 B/op** | ✅ PASS |
| **P3** | L4 p99 vs L3 p99 | ≤ +20% | **49.62 µs vs 52.71 µs (−5.9%)** | ✅ PASS |
| **P4** | L3 achieved evaluations/sec | ≥ 2.4M | **67.98M/sec on 10 cores** — 28× | ✅ PASS |
| **P5** | L6 snapshot heap at 5,000 flags | < 10.00 MB | **4.26 MB** (service) / 1.47 MB (client) | ✅ PASS |

### Latency and throughput

| # | Scenario | p50 | p95 | p99 | p999 | evaluations/sec | ns/eval |
|---|---|---|---|---|---|---|---|
| L2 | batch of 100, 1 goroutine | 7.50 µs | 8.33 µs | **15.17 µs** | 43.38 µs | 12.62M | 79 |
| L3 | batch of 100, 10 goroutines | 9.08 µs | 19.67 µs | 52.71 µs | 516.75 µs | **67.98M** | 146 |
| L4 | L3 + swap every 100 ms | 9.33 µs | 19.29 µs | 49.62 µs | 604.04 µs | 66.28M | 150 |
| L5 | batch of 100 worst-case flags | 217.00 µs | 244.92 µs | 326.92 µs | 533.33 µs | 0.45M | 2224 |

### `testing.B` view (`-benchtime 2s`)

| benchmark | ns/op | ns/eval | B/op | allocs/op |
|---|---|---|---|---|
| `L1BoolValuePlain` | 47.88 | 47.88 | 0 | **0** |
| `L1BoolValueRules` | 85.51 | 85.51 | 0 | **0** |
| `L1BoolValueRollout` | 105.2 | 105.2 | 64 | **1** |
| `L1EnginePlain` (engine only, no client) | 24.69 | 24.69 | 0 | 0 |
| `L1Unbatched100` (100 separate calls) | 7856 | 78.56 | 1280 | 20 |
| `L2Batch100` (allocating result slice) | 8660 | 86.60 | 9472 | 21 |
| `L2BatchAppend100` (pooled slice) | 7487 | 74.87 | 1280 | 20 |
| `L3Batch100Parallel` | 2024 | 20.24 | 1280 | 20 |
| `L4Batch100ParallelWithChurn` | 1912 | 19.12 | 1288 | 20 |
| `L5Batch100Pathological` | 232909 | 2329 | 6400 | 100 |
| `L5SingleWorstFlag` | 2240 | 2240 | 64 | 1 |
| `L6BuildMemSnapshot5k` | 257002 | — | 858512 | 5019 |
| `L6BuildResolvedSnapshot5k` | 3307418 | — | 6767331 | 51101 |

### L6 — snapshot memory

| flags | client `MemSnapshot` | per flag | service `ResolvedSnapshot` | per flag |
|---|---|---|---|---|
| 1,000 | 0.30 MB | 319 B | 0.88 MB | 924 B |
| 5,000 | **1.47 MB** | 309 B | **4.26 MB** | 892 B |
| 20,000 | 5.90 MB | 309 B | 17.02 MB | 892 B |

Both representations are measured because they cost very different amounts and
picking one would be choosing the number that flatters the claim. The service-side
snapshot carries a per-flag provenance map (`config.FlagProvenance`), which is most of
the 2.9× difference — and it is the thing that answers "what did the base say versus
the prod overlay" at 3am, so it is worth its weight.

The service-side snapshot is isolated by differencing: `Store.Set` clones the layer it
accepts, so a naive delta around `Set()` charges the snapshot for a layer clone it does
not own. The suite measures a store with one environment and a store with none, holding
the identical layer, and subtracts.

---

## Findings

### 1. The sub-millisecond claim at F=100 holds, with 66× of margin

L2 p99 is **15.17 µs** against a 1 ms budget. The LLD predicted "~30 µs p50, ~340 µs
p99" for local evaluation at F=100. Measured p50 is **7.50 µs** (4× better) and p99 is
**15.17 µs** (22× better). The p99 prediction was pessimistic by more than an order of
magnitude, which is the right direction for a design document to be wrong in, but it is
wrong: the real risk at F=100 is not latency.

### 2. The read path is zero-allocation — except for rollouts, and that is 20% of the corpus

- plain flag: **0 allocs/op**
- rule-matched flag: **0 allocs/op**
- **rollout flag: 1 alloc/op, 64 B**

`internal/core/bucket.go`, `NamespaceStrategy.Key`, builds the bucket key
(`<len>:<namespace>:<subject>`) with a `strings.Builder`. Its own comment says "One
allocation" — so this is known, not hidden. But `docs/05-consistency-and-e2e.md` §3.2
states the requirement as "**allocations per operation (must be 0 on the read path — a
single allocation per evaluation is 2.4M allocs/sec at peak and turns the GC into the
bottleneck)**", and P2 is written against the floor case only. Measured consequence, at
20% rollout flags:

- one 100-flag batch: **20 allocations, 1,280 B**
- at the design's 2.4M evaluations/sec peak: **480k allocs/sec, 29 MB/sec of garbage**
- at L3's measured saturation on this box: 13.6M allocs/sec, 830 MB/sec, and **204
  garbage collections in 5 seconds** on a path documented as allocation-free

The GC kept up here — L3's total pause was 57.85 ms out of 5.011 s of load, and it did
not break P3 — so this is not a fire. It is a discrepancy between what the design says
the read path does and what it does, and it is exactly the sort of thing that stops
being free on a pod with a large application heap. The fix is small and local: the
bucket key does not need to be a `string`. Hashing namespace and subject incrementally,
or writing into a stack buffer, removes the allocation without touching the wire
format — the length-prefix framing that makes the key injective is preserved either way.

### 3. L4: the atomic swap is not a latency event, and it *reduced* GC pressure

L4 p99 came in **5.9% below** L3's, with 50 successful swaps and the client ending at
generation 51. Across every run the sign was the same: swapping the snapshot 10 times a
second while 10 goroutines saturate the read path costs readers nothing measurable. The
claim in §3.3 P3 is not merely met, it is met with the wrong sign.

The GC numbers make the reason visible, and it is worth stating because it is
counterintuitive:

| | L3 (no churn) | L4 (swap every 100 ms) |
|---|---|---|
| NumGC in ~5 s | 204 | **90** |
| total pause | 57.85 ms | 21.12 ms |
| max pause | 12.08 ms | 0.76 ms |

L4 collects *less*. Retaining a fresh 1,000-flag generation raises the live heap, which
raises the `GOGC` trigger, which lowers the collection frequency — and the read path's
own rollout garbage is what is actually driving collections in both. The discarded
generations are not the GC pressure in this design; **the read path is**. That inverts
the question L4 was written to ask, and it is only visible because L4 measured both.

Caveat: this is 1,000 flags at 10 swaps/sec on a 10-core box. A 20k-flag corpus
churning at the same rate retains 17 MB per live generation, and the client holds a
generation per apply until the last reader releases it. Worth re-running L4 at 20k
before promising anything about a large corpus under rapid config change.

### 4. Per-evaluation cost beats the design in both directions

| | design | measured | |
|---|---|---|---|
| typical flag | ~0.3 µs | **0.079 µs** | **3.8× better** |
| worst realistic flag | ~3.4 µs | **2.224 µs** | 1.5× better |
| single plain `BoolValue` | ~0.3 µs | **0.048 µs** | 6.3× better |
| engine alone, no client | — | **0.025 µs** | — |

Recomputing §4.1's capacity table with the measured numbers:

```
typical steady state    360,000/s x 0.079 µs = 0.028 cores   (design said 0.11)
peak realistic        2,400,000/s x 0.079 µs = 0.189 cores   (design said 0.72)
peak pathological     2,400,000/s x 2.224 µs = 5.338 cores   (design said 8.16)
```

**The design's conclusion survives its own arithmetic being wrong.** The pathological
corner is 5.3 cores rather than 8.2 — still "a line item rather than a rounding error",
still affordable against 40+ pods, and the two consequences the LLD draws from it
(rule-count-per-flag is a budgeted resource; lint above ~10 rules) both stand.

On the "~3.4 µs worst realistic" figure being conservative: how conservative depends
entirely on where the failing condition sits. This suite's L5 flag fails on the
**fourth** condition of every rule, so all 80 conditions are evaluated — 2.22 µs. A
rule set that fails on its *first* condition short-circuits and costs a small fraction
of that. A measurement of "20 rules × 4 conditions" that does not say which condition
fails is not reproducible, and two such measurements can differ by an order of
magnitude without either being wrong.

### 5. The batch API is mandatory for correctness, not for cost — the LLD says otherwise

`docs/03-lld.md` §4.1, consequence 1: *"The batch API is mandatory, not a convenience.
100 individual client calls per request means 100 × per-call overhead."*

Measured, same corpus, same 100 flags:

| | ns/op | allocs/op |
|---|---|---|
| 100 separate `BoolValue` calls | 7,856 | 20 |
| one `Batch` (allocates the result slice) | 8,660 | 21 |
| one `BatchAppend` (pooled slice) | 7,487 | 20 |

Batching saves **5%**, and the allocating `Batch` form is *slower* than 100 individual
calls. There is no 100× per-call overhead to amortise, because `BatchAppend` still
establishes a `recover` frame per element in `evalGuarded` — by design, so that one
pathological flag cannot convert the other 99 to defaults. The batch amortises one
atomic load and one outer `recover` frame across 100 flags, and that is all.

This does not weaken the case for `Batch`; it relocates it. The real argument is the
one the same file makes two paragraphs later and the one §1.1 G1 turns into a
guarantee: **the snapshot is pinned once, so a swap landing mid-batch cannot return
flag A from generation N and flag B from N+1.** That is a correctness property no
amount of per-call speed can buy. The cost sentence should be struck; the CACHE-1
sentence should be the whole justification.

### 6. Snapshot memory: the claim is right, and conservative

`~6 MB at 5,000 flags` versus **4.26 MB** measured for the service-side
`ResolvedSnapshot` and **1.47 MB** for what a client actually holds. §4.2's "steady
state per pod ~10 to 18 MB" for three retained generations is comfortable: three
client-side generations is ~4.4 MB. The per-pod ceiling is not a constraint at 5k
flags, and even 20k flags is 5.9 MB per client generation.

`config.ResolvedSnapshot` costs 2.9× a `client.MemSnapshot` at every size, all of it
provenance and quarantine bookkeeping. That is the service's own cost, not a per-pod
one, and it buys the debug endpoint.

---

## What this suite does not measure

- **A fleet.** Every fleet number is one box multiplied by a rate. Linear scaling across
  40 pods is an assumption stated in the output, not a result.
- **The transport.** L1–L5 measure the client and the engine against an in-process
  snapshot. HTTP framing, JSON decoding and the eval endpoint's own overhead are not in
  any number here.
- **Convergence.** Propagation, staleness and the 5 s budget are §2's E2E scenario, not
  §3's load benchmark.
- **RSS.** L6 is retained Go heap. A container limit needs more than this number.
- **A cold snapshot.** Every scenario runs against a warm map. A pod's first requests
  after an apply will take page faults this suite has already paid.
- **Contention on the write side.** L4 runs one writer. Two writers racing `Set()` is
  §2.3's "two writers" variant.

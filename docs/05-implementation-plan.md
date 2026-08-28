# 05 — Implementation Plan (Phase 0 → Phase 14)

**Status:** DRAFT for approval · **Date:** 2026-08-28 · **Owner:** harsh

> **Relationship to `PLAN.md`.** `PLAN.md` is the *programme* plan: it decides WHAT the
> system is and closes the ambiguous decisions O1–O5. This document is the *execution*
> plan: it decomposes that into buildable phases with named files, named types, and exit
> gates that are executable commands rather than prose. `PLAN.md` phase numbers and this
> document's phase numbers are **not** the same axis — §0.3 maps them.

---

## 0.1 Why this plan opens with a reconciliation phase

A full inventory of `docs/02-hld.md`, `docs/03-lld.md`, `docs/diagrams/*` and `PLAN.md`
found that the design corpus is not yet a single specification. It contains:

| Class | Count | Consequence if we build anyway |
|---|---|---|
| Direct contradictions between sections | **16** | Each one is resolved *silently* by whoever types first. That is the exact failure mode ADR-B8 ("ambiguous config is rejected, never defaulted") exists to prevent — applied to config, but not to our own spec |
| Go identifiers referenced but never defined | **15** | `CompiledRule`, `TypedValue`, `LayerID`, `Severity`, `AttributeDecl`, `LayerRevisions`, `HashFnID`, `EvalContext`, `Snapshot`, `FlagConfig`, `ValueType`, the `Operator` const set, `acknowledge_reshuffle`, `rollout.anonymous_policy`, `Rollout.StrategyName` |
| ADRs the docs themselves mandate, unwritten | **2** | The joint O1+O2 ADR (demanded in five separate places) and "bucketing is a wire format" (`ADR-00x`, literal placeholder) |
| Open decisions still blocking phases | **3** | S-O2 (L2 disk cache in/out), S-O3 (max staleness under outage), S-O4 (tenant label cardinality) |
| Named work with no owner | **2** | `flagctl lint`; the "call-site default must be the pre-flag behaviour" review rule |

**This is already costing us.** Code committed and in flight has picked sides:

| Conflict | Code picked | Doc that disagrees |
|---|---|---|
| Missing bucketing subject | `ReasonMissingSubject.IsFallback() == true` → **caller default** (`internal/core/reason.go:88`) | HLD §C.4: missing subject ⇒ deterministic **`ROLLOUT_OUT`**, never the caller default |
| Rollout threshold | `BasisPoints int32`, space 10 000 (`internal/core/model.go:133`) | HLD §B.4.3: `Threshold uint32 = pct/100 × 2³²` |
| Package spine | `internal/core` (LLD §3.3 vocabulary) | HLD §A.3 layout: `internal/flag` + `internal/eval/{resolve,rules,bucket,compiled}` |
| Condition shape | `Values []Value` + `Negate` + `Combiner LogicOp` | §C.3 ships **no boolean combiner** in Phase 1; `diagrams/lld.md` L3 adds one |
| Flag value model | direct `DefaultValue`/`OffValue`/`Rule.Value` | §C.2: `Variations map[string]Value` + `DefaultVar`/`OffVar`/`Rule.VariationKey` |
| ADR numbering | `docs/adr/0001–0004` (bucketing-key, rule-list-merge, absent-attribute, no-regex) | HLD inline `ADR-B1`/`ADR-B2` — a **third** numbering scheme |

Every one of these is defensible as a choice. None of them has been *made* — they have been
typed. Phase 0 turns the typing into decisions.

## 0.2 How to read this plan

Each phase carries:

- **Objective** — one sentence.
- **Entry** — what must be true to start.
- **Deliverables** — files and exported symbols, not prose.
- **Satisfies** — the specific ADRs, invariants, hazards, validation rules, metrics and
  brief requirements this phase closes. Appendix A is the inverse index; nothing may be
  absent from it.
- **Exit gate** — commands that pass, or assertions a test makes. Not "reviewed and looks
  right".

A phase is done when its gate is green **and** every Appendix-A row pointing at it is ticked.

## 0.3 Phase-number mapping

| This doc | `PLAN.md` | Note |
|---|---|---|
| P0 Contract freeze | completes Phase 2's unmet exit criteria | `PLAN.md` Phase 2 exit says "every exported symbol has a stated contract" — not yet true |
| P1 Ring-0 domain | Phase 2 (types) | largely built |
| P2 Targeting rules | Phase 5 | built, untested |
| P3 Bucketing & rollout | Phase 6 | built, untested |
| P4 Evaluator & never-throw | Phase 4 | built, untested |
| P5 Config layers & merge | Phase 3 (first half) | in flight, does not compile |
| P6 Validation | Phase 3 (second half) | not started |
| P7 Snapshot, build pipeline, publish | Phase 3 / 7 (swap) | not started |
| P8 Store & admin ingress | Phase 3 (`ConfigStore`) | not started |
| P9 Transport & health | Phase 8 (first half) | not started |
| P10 Propagation | Phase 7 | not started |
| P11 Thin client | Phase 8 (second half) | partially built |
| P12 Observability | Phase 9 | partially built |
| P13 Test suite & benchmarks | Phase 10 | ~2% |
| P14 Go-live | Phase 11 | not started |

Build order is **not** `PLAN.md` order. `PLAN.md` sequences by *argument* (decide targeting
before rollout because O1 and O2 interact). Implementation sequences by *dependency ring*:
ring 0 → ring 1 → ring 2 → ring 3. The reordering is deliberate and is the reason this
document exists separately.

---

# P0 — Contract Freeze

**Objective.** Turn the design corpus into one normative specification with zero
contradictions and zero undefined identifiers, so that no later phase decides a design
question by accident.

**Entry.** None. This phase blocks all others.

**Deliverables.**

| # | Item | Output |
|---|---|---|
| P0.1 | **Package spine.** Ratify `internal/core` (LLD §3.3, and what is built) or HLD §A.3's `internal/flag` + `internal/eval/**`. The single-shared-evaluator-package commitment is load-bearing — it is the entire answer to §A.5.3's divergence objection — but the package carrying it has two names | `docs/00-contract.md` §1 + the losing layout marked SUPERSEDED in place |
| P0.2 | **`Snapshot` ≡ `ResolvedSnapshot`.** One name. `PLAN.md` escalation #7 names this explicitly and it is still open | contract §2 |
| P0.3 | **Flag / Rule / Condition shape.** §B direct-values vs §C variations-map. Decide `KillSwitch` (§C only), `Combiner LogicOp` (diagrams only, contradicts §C.3's "no boolean tree in Phase 1"), `Values []Value` vs `Value` singular | contract §3 + ADR |
| P0.4 | **Threshold representation.** Basis points ∈ [0,10000] (§C.4, built) vs `uint32 = pct/100 × 2³²` (§B.4.3). Arithmetically incompatible; both claim to be the hot-path compare | contract §4 + golden vectors keyed to the winner |
| P0.5 | **Client API surface.** `BoolValue(ctx, key, ec, def) bool` (§C.6) vs `BoolValue(name, def, ctx) (bool, Detail)` (§D.5). `New()` never blocks (§A.5.2) vs `NewClient()` blocks 3 s (§D.5). `EvaluateAll` vs `EvaluateBatch` | contract §5 |
| P0.6 | **Reason enum.** Union the ~12 candidates. **Decide `MISSING_SUBJECT` first**: caller-default (built, `diagrams/lld.md`) vs deterministic `ROLLOUT_OUT` (§C.4). Include or exclude `BREAKER_OPEN`, `CLIENT_UNINITIALIZED`, `TIME_PREDICATE_SKEW_SUSPECT`. Confirm `CACHED`/`STALE` stay excluded (§C.2) | contract §6 |
| P0.7 | **Metric namespace.** Five prefixes in play: `flagconfig_`, `flagsvc_`, `flagclient_`, `flag_client_`, `flag_eval_`. Several metrics appear twice under two names | contract §7 |
| P0.8 | **Client state machine.** `{Uninitialized, Fresh, Resyncing, Disconnected, Stale}` (§D.5) vs `{UNINITIALIZED, HEALTHY, DEGRADED_STALE}` (LLD §6.2). `/ready` is specified against the LLD names | contract §8 |
| P0.9 | **Environment enum.** 3 (`numEnvs`, §B) vs 5 typical / 20 stress (§D) vs `+ __system` reserved (§D.7). `Environment uint8` is a dense array index, so this is a data-structure decision | contract §9 |
| P0.10 | **Log event name and schema.** `flag_eval_fault` 20 fields (§C.7) vs `flag.evaluation.error` 12 fields (§D.6) | contract §10 |
| P0.11 | **`pending_changes_seconds` threshold.** Page > 60 s (§B.7.3) vs alert > 5 s (LLD §7). H5 calls this "the load-bearing alert" | contract §11 |
| P0.12 | **Validation ID ranges.** §B.4.4 step 2 says B01–B13 / O01–O09; the tables define B01–B16 / O01–O10. Also `diagrams/lld.md` L4 names a post-merge check ("rollout without on and off values") absent from M01–M17 | contract §12 |
| P0.13 | **Write the two mandated ADRs.** (a) joint O1+O2 — demanded in §C.9, §C.5, §D.8, HLD front matter and `PLAN.md` escalation #2, *and it must record the bucketing scheme hash*; (b) "bucketing hash and key construction are a wire format" (`ADR-00x`). **Reconcile with the existing `docs/adr/0001–0004`, which is a third numbering** | `docs/adr/` |
| P0.14 | **Define or delete the 15 undefined identifiers.** Each becomes a Go declaration in P1 or is struck from the contract | contract appendix |
| P0.15 | **Close S-O2, S-O3, S-O4.** S-O2 is de facto closed IN — `pkg/client/cache.go` already ships `FileStore`, `JSONCodec`, `SnapshotStore`. Ratify or delete the code. S-O3 (max staleness under total outage) changes LLD §6.3. S-O4 gates whether `tenant_id` may ever be a metric label | contract §13 |
| P0.16 | **Assign the unowned work.** `flagctl lint` (cross-references Go call sites against declared flag types — §C assumes it exists); the review rule "the call-site default must be the value that was correct before the flag existed" (§D.5, the entire safety argument for fail-open) | owners named, or the dependent claims softened in writing |
| P0.17 | **Discovery traceability.** `docs/01-discovery.md` is titled *Discovery — Rate Limiting*, status BLOCKED, and describes a different system. There is no discovery doc for the flag service. Either write one or delete the traceability claim | decision recorded |

**Satisfies.** `PLAN.md` Phase 2 exit criteria · escalations #2, #3, #6, #7 · ADR-00x · the
joint O1/O2 ADR · R3 (bucketing immutability needs a recorded scheme hash to be enforceable).

**Exit gate.**
- `docs/00-contract.md` exists and is the single normative source. Every superseded HLD/LLD
  section carries an in-place SUPERSEDED marker (particularly §A.5.3, which still rejects
  client-side caching in prose while §D.5 and LLD §2 mandate it).
- Zero identifiers in the contract are undefined.
- `grep -c` of the conflict register returns 16 resolved, 0 open.
- Both mandated ADRs are committed, and ADR numbering is single-valued.

> **GATE 0** — no further implementation is approved until this passes. Code already written
> stays on disk; it is re-validated against the contract in P1–P4 rather than deleted.

---

# P1 — Ring 0: Domain Types

**Objective.** A dependency-free vocabulary that the service binary and the client library
both compile against, so there is no second implementation to diverge (LLD §3.3).

**Entry.** GATE 0.

**Deliverables.** `internal/core/` — `value.go`, `model.go`, `evalctx.go`, `reason.go`,
`result.go`, `contract.go`. Built. Remaining work is conformance to the frozen contract:

| # | Item |
|---|---|
| P1.1 | Apply every P0 decision to the built types (Reason set, threshold repr, condition shape, flag value model) |
| P1.2 | Declare the identifiers P0.14 retained |
| P1.3 | **Write `TestNoIOImports`.** `value.go:7` claims the no-I/O constraint "is enforced by `TestNoIOImports`". That test does not exist. A doc comment asserting a guarantee nobody makes is worse than no comment |
| P1.4 | Enum round-trip exhaustiveness tests: adding an `Operator` without adding it to `String()` **and** `ParseOperator` must fail the build |
| P1.5 | `Value` JSON round-trip property test incl. `int64` bounds, `TypeUnknown → null`, fractional rejection |
| P1.6 | `Value.Equal` reflexivity/symmetry across all type pairs |
| P1.7 | `EvalContext.Attribute` absent-vs-present-but-zero, incl. the asymmetry that an empty `UserID` reports ABSENT while an explicit empty-string entry in `Attributes` reports PRESENT |
| P1.8 | Zero-allocation assertion on `Value` construction and comparison (`testing.AllocsPerRun`) |

**Satisfies.** ADR-B4 (tri-state, no pointer fields) · C1 (absent ⇒ false before negation) ·
C4 (typed accessors) · brief: flag types bool/string/int.

**Exit gate.** `go test -race ./internal/core/...` green · `TestNoIOImports` passes and fails
when `os` is added · enum exhaustiveness test fails on a deliberately-omitted constant ·
`AllocsPerRun == 0` on the `Value` paths.

---

# P2 — Ring 1a: Targeting Rules & Matcher

**Objective.** Attribute matching with defined, tested behaviour on the two cases real
systems get wrong: absent attribute and wrong-type attribute.

**Entry.** P1.

**Deliverables.** `internal/core/matcher.go`, `semver.go` (both built, untested beyond semver).

| # | Item |
|---|---|
| P2.1 | Operator set per contract. §C.3 ships: `EQUALS`, `IN`, `EXISTS`, `SEMVER_{EQ,GT,GTE,LT,LTE}`, `NUM_{GT,GTE,LT,LTE}`, `STARTS_WITH`, `ENDS_WITH`, `CONTAINS` (with lint warning). **`REGEX` does not ship.** Built code is missing `STARTS_WITH`/`ENDS_WITH` and has no `GTE`-vs-`NUM_GTE` distinction — reconcile |
| P2.2 | `IN` compiled to `map[Value]struct{}` at snapshot build when `len(Values) ≥ 8`; linear scan below |
| P2.3 | Absent attribute ⇒ condition false **before** negation. `EXISTS` + `Negate` is the only operator true on absence |
| P2.4 | Wrong-type attribute ⇒ false, no coercion, `flag_eval_attribute_type_mismatch_total{flag_key,attribute}` incremented |
| P2.5 | First-match-wins; `RuleID` carried into `Result`, never into a metric label |
| P2.6 | `Tri` undecidable outcomes routed to `ConditionObserver` (built) and wired to the metric in P12 |
| P2.7 | Fuzz target for the matcher against corrupted rule trees |

**Satisfies.** ADR-0003 / C1 · brief: targeting rules · `PLAN.md` Phase 5 exit · validation
B06, B07, B14, B15, M06, M07 (config-side halves land in P6).

**Exit gate.** `country == "IN"` returns true for IN, fallthrough otherwise, and the
absent-country case matches the documented behaviour · negation table green across all
operators · fuzz target runs 60 s with no panic · no allocation in `MatchRule`.

---

# P3 — Ring 1b: Bucketing & Percentage Rollout

**Objective.** Implement O1 and O2 as decided, with the hash pinned as a wire format.

**Entry.** P1, and ADR-00x from P0.13.

**Deliverables.** `internal/core/bucket.go` (built: `XXHasher`, `NamespaceStrategy`).

| # | Item |
|---|---|
| P3.1 | xxhash64 via `cespare/xxhash/v2`. `hash/maphash` is disqualified — its per-process seed reshuffles every rollout on every deploy |
| P3.2 | Multiply-shift to bucket space, no modulo: `hi := h >> 32; uint32((hi * BucketSpace) >> 32)`. Avoids a 20–40-cycle division; modulo bias at 2⁶⁴ is ~5×10⁻¹⁶ |
| P3.3 | `BucketKeyStrategy` with a `[64]byte` caller-supplied stack scratch buffer ⇒ zero allocation. Built signature is `Key(flag, ctx) (string, bool)` — **returns a string, so it allocates.** Reconcile against §C.4's `Key(flag, ctx, dst []byte) ([]byte, bool)` |
| P3.4 | `bucket_namespace` empty ⇒ flag key (independent); shared literal ⇒ deliberately shared. This is O1 |
| P3.5 | Rules-first ordering as an explicit `evaluation_order` field, never an implicit default. This is O2 |
| P3.6 | Missing subject behaviour **per the P0.6 decision** — the single highest-value thing P0 unblocks |
| P3.7 | **500-pair bucketing golden vectors**, including the *composed key*, not just the hash |
| P3.8 | **1 000-ID assignment-stability golden test** that fails the build **and refuses boot on diff** (§D.8 row 9) |
| P3.9 | `bucketing_scheme_hash` = hash of {function name, version, salt, key-composition rule, normalisation rule} |
| P3.10 | Monotone-ramp property test: raising a percentage only ever adds users |
| P3.11 | Distribution test across a large synthetic population, within tolerance |
| P3.12 | Purity test: each strategy called 1 000× on one context returns identical bytes |
| P3.13 | Explicit non-normalisation of the subject: `"User1"` and `"user1"` bucket differently, by decision (§C.10). Lint at the SDK boundary |

**Satisfies.** O1, O2, ADR-00x, ADR-0001 · C2 (xxhash pinned), C3 (monotone ramp) · H3
(silent re-bucketing) · R3 · brief: percentage rollout, sticky bucketing, independent vs
shared buckets.

**Exit gate.** Stickiness over repeated calls · independence across two flags at equal
percentage · sharing when both opt into one namespace · distribution within tolerance ·
golden vectors green · boot self-check refuses to start on a vector diff.

---

# P4 — Ring 1c: Evaluator Pipeline & Never-Throw

**Objective.** The pipeline spine, and the never-throw guarantee made enforceable rather than
aspirational.

**Entry.** P2, P3.

**Deliverables.** `internal/core/evaluator.go` (built).

| # | Item |
|---|---|
| P4.1 | Stages: pin snapshot → flag lookup → enabled/kill-switch → type check → rules → rollout → assemble `Result` |
| P4.2 | **CACHE-1 / SNAP-1: pin once per request, not once per flag.** These are the same invariant under two IDs (P0 collapses them). Enforced by an **architecture test asserting `store.Load()` appears at exactly one call site** — a second is a build failure |
| P4.3 | **CACHE-3: the read path performs no I/O.** No network, no disk, no lock, no failable allocation. An unknown flag is an *answer*, not a cache miss |
| P4.4 | Typed accessors `BoolValue`/`StringValue`/`IntValue`; caller default required and positional |
| P4.5 | `recover()` at **exactly two sites**, both in the transport/safe boundary, on the calling goroutine. A `recover()` in core is a review reject. Pattern: assign the default to the named return **before** anything can break |
| P4.6 | **Never-panics reflection test** — enumerate every exported method via reflection, run each against a deliberately corrupted snapshot (nil maps, nil `*Flag`, variation key pointing nowhere, nil condition slice, `basis_points` of 4 billion). A new exported method with no boundary fails CI |
| P4.7 | **No goroutines in the eval package** — `forbidigo`/`semgrep` rule failing CI on `go ` and `errgroup` |
| P4.8 | Zero-allocation assertion on the happy path via `testing.AllocsPerRun` |
| P4.9 | Result carries `Reason`, `RuleID`, `Bucket`, `Generation`, `UsedDefault` |

**Satisfies.** A2, A3, A4 · C5 (immutable snapshot: concurrent map read/write in Go is a
*fatal throw* `recover` cannot catch) · CACHE-1/3, SNAP-1 · L7 never-throw · brief: sync
low-latency API that never throws.

**Exit gate.** Corrupted snapshot returns the caller default on every path and logs
structured errors · fault-injected panic inside the engine still returns a value to the
caller · `AllocsPerRun == 0` · arch test green · `go test -race` green.

---

# P5 — Ring 2a: Config Layers & Merge

**Objective.** Helm-style base + overlay + ops layering with merge semantics defined per
field kind.

**Entry.** GATE 0 (the layer vocabulary is one of the frozen items), P1.

**Deliverables.** `internal/config/` — `opt.go`, `layer.go`, `merge.go`. **In flight;
currently does not compile (`layer.go:381: undefined: sortStrings`).**

| # | Item |
|---|---|
| P5.1 | `Opt[T]{Present, Null, Value}` tri-state. Pointers collapse absent and explicit-null into `nil` — ADR-B4 |
| P5.2 | Base and overlay are **different Go types** — total record vs sparse patch. Reusing one struct is named as the single most common way this class of system rots |
| P5.3 | Four layers: L0 caller default (lives in the *caller binary*, participates in resolution but **not** in the merge), L1 base, L2 env overlay, L3 ops override |
| P5.4 | L3 whitelisted to `{enabled, value}` + mandatory `expires_at`, `reason`, `owner`; 30-day hard cap, 72 h warn |
| P5.5 | Merge per field kind: identity **base-only immutable** · scalar higher-layer-wins, explicit null **rejected** · `rollout` **recursive deep merge** · `tags` per-key merge with per-key null delete · `targeting_rules` **whole-list replace or append, never element-wise, never prepend** |
| P5.6 | `targeting_rules` and `targeting_rules_append` mutually exclusive |
| P5.7 | **Deep copy is unconditional.** Snapshots for different environments never share a backing array, slice or map, even when byte-identical |
| P5.8 | Provenance: field → winning `LayerID`, retained for incident forensics (ADR-B5) |

**Satisfies.** ADR-B1, ADR-B4, ADR-B5 · B1–B5 · H7 (`append` lets base edits reach prod) ·
brief: environments.

**Exit gate.** `go build ./...` green · a prod overlay that raises a percentage **and** adds a
rule while inheriting the base resolves correctly, proven by test · `rollout: {percentage: 25}`
does **not** blank `bucket_by` (ADR-B2's named incident) · replace-vs-append order table green ·
env snapshots share no backing memory, proven by pointer-identity assertion.

---

# P6 — Ring 2b: Validation

**Objective.** Reject at config time and fail safe at evaluation time — decision O3, both.

**Entry.** P5.

**Deliverables.** `internal/config/validate/`.

| # | Item |
|---|---|
| P6.1 | **B01–B16** base self-validation, all reject-global except B13–B16 warn |
| P6.2 | **O01–O10** overlay self-validation, reject-flag except O10 warn |
| P6.3 | **M01–M17** post-merge — the set that makes eager resolution mandatory, because these are only decidable after merging |
| P6.4 | Four-level severity: reject-global / reject-env / **reject-flag (quarantine)** / warn |
| P6.5 | **M15 safety valve:** quarantined > `max(20, 5%)` escalates to reject-env. Per-flag quarantine is what stops one bad flag freezing an environment (O3) |
| P6.6 | **B10 / M09:** rules + rollout with no explicit `evaluation_order` ⇒ **reject**, never defaulted (ADR-B8). Note `Flag.HasRollout()` returns false for `BasisPoints == 0` — confirm the validator uses a *presence* predicate, not `HasRollout`, or a `rollout{basis_points:0}` escapes M09 |
| P6.7 | **B09:** rollout present ⇒ `bucket_by` non-empty. **No default bucketing key** |
| P6.8 | Lint warnings: `CONTAINS` usage · `Negate` with no sibling `EXISTS` · > 10 rules per flag · **M10** (replace-mode overlay sharing an id with base but differing in content — the primary early warning for ADR-B1's accepted cost) |
| P6.9 | `BuildReport{PerEnv}` returned on **every** commit. No silent rejection, ever |
| P6.10 | Fuzz target for the validator |
| P6.11 | `acknowledge_reshuffle: true` required to change `bucket_strategy` on a flag with a live rollout |

**Satisfies.** O3 · ADR-B8 · B8 (ambiguous config rejected) · M13/M14 memory guards ·
H3 · brief: misconfiguration handling.

**Exit gate.** Every B/O/M rule has a test that fires it and one that does not · an invalid
overlay is rejected with the previous snapshot still serving · **a rejected config leaves the
cache byte-identical** (requires deterministic encoding — see P7.6) · fuzz 60 s clean.

---

# P7 — Ring 2c: Snapshot, Build Pipeline, Atomic Publish

**Objective.** Build to completion, validate, then a single atomic swap — all cost on the
write path.

**Entry.** P5, P6.

**Deliverables.** `internal/config/snapshot/`, `internal/build/`.

| # | Item |
|---|---|
| P7.1 | Six-stage pipeline: stage (**~250 ms debounce**) → layer self-validation → per-env merge → post-merge validation → **freeze** → publish |
| P7.2 | Freeze precomputes: threshold, `HashFn` → function *index* (no string compare on the hot path), `bucket_namespace` default → flag key, flag map **at exact capacity**, provenance attached |
| P7.3 | **CACHE-2: build then swap.** `atomic.Pointer`, single writer goroutine, zero mutexes on the data path. An `RWMutex` config write stalls ~5 000 in-flight evaluations per change (ADR-D2) |
| P7.4 | **Per-environment transactionality, not global** (ADR-B7). A prod typo must not block an urgent dev fix |
| P7.5 | Rollback ring, N = 3. `Rollback(env)` is a single pointer swap requiring no source layers |
| P7.6 | **Deterministic snapshot encoding.** `pkg/client/cache.go:378` iterates a map, so L2 output byte-order is nondeterministic today — which makes P6's "byte-identical" exit criterion untestable and golden-vector comparison of persisted snapshots impossible. Sort by flag key |
| P7.7 | `SnapshotID = (instance_id ULID, generation uint64 from 1)` + `config_version = sha256(canonical_json)[:12]`. Both are required: generation alone cannot prove a rollback restored prior state; `config_version` alone cannot order two snapshots (ADR-D3) |
| P7.8 | Structural sharing: a single-flag `set()` allocates ~205 KB, not a 2 MB full rebuild |

**Satisfies.** ADR-B2 (eager resolution), ADR-B6, ADR-B7, ADR-D2, ADR-D3 · CACHE-2 · A4 ·
B6, B7 · brief: live updates without restart.

**Exit gate.** **No reader observes a partially-built snapshot during a swap under concurrent
load**, proven under `-race` · rejected config is a no-op on the cache, not a flush · rollback
reproduces the prior `config_version` at a new generation · build p99 < 100 ms.

---

# P8 — Ring 3a: ConfigStore & Admin Ingress

**Objective.** The brief's `set(flag_config)` / `get(flag_name, env)`, plus the operator path.

**Entry.** P7.

**Deliverables.** `internal/store/`, `internal/admin/`, `cmd/flagctl/`.

| # | Item |
|---|---|
| P8.1 | Store holds **raw unmerged layers** plus the version counter and change notification (ADR-B5). Note the docs define two different `Get`s — one over raw layers, one over resolved snapshots. P0.2 resolves |
| P8.2 | Admin ingress on **port 9090**, separate `http.Server` so a config-push storm cannot starve evaluation on 8080 |
| P8.3 | Different authn per listener: mTLS/SPIFFE peer identity for eval, signed operator token for admin |
| P8.4 | `set()` returns **200 on local swap** (~t+15 ms) with `{generation, config_version, subscribers_notified}`; does not wait for client acks. Validation reject ⇒ 4xx with a `BuildReport`, live snapshot untouched |
| P8.5 | `GET /convergence?generation=N` ⇒ `{acked, total, laggards[]}` |
| P8.6 | `Rollback(env)` endpoint |
| P8.7 | Provenance debug endpoint |
| P8.8 | **Config-apply audit log shipped off-box and replayable.** This is the *de facto* recovery source for R1/H4 — in-memory means a restart loses the corpus while clients hold snapshots with no source of truth |
| P8.9 | `flagctl lint` — cross-reference Go call sites against declared flag types (owner assigned in P0.16) |
| P8.10 | Replica-to-replica fan-out: `admin -.-> PEERS`, versioned, at-least-once, idempotent by `(flag, env, version)`, divergence alarm on version skew. **§A.1.4 defers this to §D and §D never covers it — this is a genuine design gap, not just an implementation task** |

**Satisfies.** A1 (two deployables, admin on a second listener) · ADR-B5 · R1, H4 · brief:
config store set/get.

**Exit gate.** `set` → `get` round trip per env · a rejected push returns a `BuildReport` and
leaves the snapshot serving · audit log replay reconstructs the corpus from empty · admin
traffic on 9090 does not measurably affect 8080 eval latency under a push storm.

---

# P9 — Ring 3b: Transport & Health

**Objective.** Expose evaluation over the wire with the two recover boundaries and the health
semantics the failure model requires.

**Entry.** P4, P7.

**Deliverables.** `api/flag/v1/`, `api/gen/`, `internal/transport/{grpc,http,safe}/`, `cmd/flagd/`.

| # | Item |
|---|---|
| P9.1 | Protobuf contract in `api/flag/v1/`, generated stubs checked in. **`EvaluateRequest` / `EvaluateBatchRequest` have names but no field list anywhere in the docs — this is new design work, not transcription** |
| P9.2 | **Batch is mandatory, not a convenience.** Per-RPC overhead is ~30 µs, ~80× evaluation cost; 10 flags unbatched is 4.3 ms vs 0.45 ms batched. Little's Law: 320 goroutines blocked vs 40 during a 20 ms stall |
| P9.3 | Response `Detail{value, reason, variant, snapshot{instance_id, generation, config_version}, bucketing_scheme_hash, evaluated_at}` — **always, not behind a debug flag** |
| P9.4 | **Both** recover sites live here: per-request handler + snapshot-compile goroutine. Nowhere else in the codebase |
| P9.5 | `/ready` — NOT READY until every environment has published generation ≥ 1 (H6: cold start is the only case with no last-known-good) |
| P9.6 | `/live` — stays healthy when config is broken. The process is fine; restarting will not help |
| P9.7 | `/health` — path is committed, semantics are **not specified anywhere**. Define in P0 or here |
| P9.8 | Error envelope `{error_code, message, trace_id, span_id, timestamp}` per `AGENTS.md`. **Scope note the docs never state: the eval path never returns errors (it returns `Reason`), so the envelope applies only to the admin 4xx path** |
| P9.9 | No raw stack traces to callers, ever |
| P9.10 | Trace id through every hop — HTTP **and** message headers — into every log line |
| P9.11 | Server-side admission control: accept-rate limit with `503` + `Retry-After` (H2) |
| P9.12 | `deploy/` network policy separating 8080 from 9090 |

**Satisfies.** A1 · H2, H6 · `AGENTS.md` house standards · brief: evaluation API.

**Exit gate.** Contract test over the generated stubs · batch and single agree exactly ·
panic injected in a handler returns an error envelope, not a stack trace, and the process
survives · `/ready` is red until generation ≥ 1 in every env.

---

# P10 — Live Update Propagation

**Objective.** Meet the sub-5 s budget with a measured number, not an assertion.

**Entry.** P7, P9.

**Deliverables.** `internal/transport/grpc/` streaming, subscriber hub.

| # | Item |
|---|---|
| P10.1 | **Layer 1 — push.** gRPC server-stream `Subscribe`; the snapshot event carries shared pre-serialised bytes. 90 ms p99 |
| P10.2 | **Layer 2 — heartbeat.** `{instance_id, generation, server_time}` every **500 ms** on the same stream, ~30 bytes. Doubles as LB keepalive |
| P10.3 | **Layer 3 — dead-stream detection.** 3 missed heartbeats = **1 500 ms** ⇒ reconnect with jitter, fetch full snapshot. **This path is the binding 2 040 ms constraint** |
| P10.4 | **Layer 4 — reconcile poll.** Unary `GetSnapshot` every **30 s** with `(instance_id, generation)`. Exists for **defects, not events** — if it ever fires with a real diff that is a bug report, and it is alerted as one |
| P10.5 | **Snapshots are absolute state, not deltas.** Therefore: subscriber channel depth 1 with drain-and-replace, non-blocking send, no gap-fill, no replay log, no sequence reassembly. Coalescing 20 changes in 200 ms into one frame is a feature |
| P10.6 | `instance_id` differs ⇒ **resync unconditionally, ignore generation entirely**. Prevents a client at gen 900 meeting a restarted server at gen 3 and concluding it is ahead |
| P10.7 | Reconnect backoff: first retry `uniform(0, 250 ms)`, then exponential full-jitter to a 10 s cap. The 250 ms window is chosen to stay inside 2 040 ms |
| P10.8 | Generation-conditional reconnect: converts a 10 GB thundering-herd burst into ~150 KB |
| P10.9 | `grpc.InitialWindowSize` tuned 64 KB → 16 KB above ~1 000 subscribers. **320 MB of HTTP/2 stream buffers at 5 000 subscribers is what sizes the pod, not the ~6 MB of config** |
| P10.10 | `sync.RWMutex` on the **subscriber registry** is fine and intended — the D.3 mutex ban applies to the eval hot path only |
| P10.11 | Continuous canary-flag propagation prober every 60 s in a reserved `__system` environment (ADR-D6) |

**Satisfies.** O4, ADR-D1, ADR-D3, ADR-D5, ADR-D6 · D1, D3 · H5 · L8, brief: live updates
under 5 s.

**Exit gate.** **Measured** end-to-end propagation p99 well inside 5 s, with the dead-stream
path measured at ≤ 2 040 ms · no torn reads during a swap under concurrent load · a killed
stream reconverges within the budget · thundering-herd test at 1 000 simulated clients.

---

# P11 — Thin Client

**Objective.** Give the app backend a client that cannot become the outage.

**Entry.** P4, P9, P10.

**Deliverables.** `pkg/flagclient/` + `breaker/`, `fake/`, `otel/`. **Partially built as
`pkg/client/` — `doc.go`, `state.go`, `cache.go`.**

| # | Item |
|---|---|
| P11.1 | **Three-tier cache.** L0 call-site default (compile time, cannot fail) · L1 full resolved snapshot, immutable, process lifetime, the only thing the hot path reads · L2 last-known-good on local disk, pod lifetime |
| P11.2 | Cache filled **by the write path, post-merge**. Never lazy, never fill-on-miss: at 600 k evals/s a cold-key stampede is an outage, not a blip |
| P11.3 | **No hard expiry.** Expiring converts a freshness problem into a fleet-wide availability outage. Staleness is reported, never enforced. `max_stale` 300 s **warns and keeps serving** |
| P11.4 | L2 disk write is **async, after** the L1 swap; failure degrades cold-start recovery and does not fail the apply |
| P11.5 | **Startup must not block on the flag service.** Otherwise the flag service becomes a hard dependency of every deploy, fleet-wide. Per P0.5, reconcile `New()` never-blocks against `NewClient()` blocks-3 s |
| P11.6 | Client state machine per P0.8. `/ready` gates on `!= UNINITIALIZED` — a `DEGRADED_STALE` pod is a working pod |
| P11.7 | Circuit breaker: ~10 consecutive failures or > 50 % over 10 s, 5 s half-open probe, pure atomics. Breaker-open path costs ~20 ns |
| P11.8 | **No retries on timeout.** Retry only on connection-level `UNAVAILABLE` before any bytes are sent, once, against the same shared deadline |
| P11.9 | Client timeout 20 ms (~10× p99). Hard timeout + breaker treating **slow as failed** + in-flight semaphore — H2: a fast shed beats a successful 20 ms response |
| P11.10 | `pkg/flagclient/fake` — the honest answer to "let me evaluate locally in tests" |
| P11.11 | `pkg/flagclient` must not import `internal/*`. Enforce with a lint rule, not just Go visibility |
| P11.12 | Fix, from the current `pkg/client/cache.go`: nondeterministic map iteration in `JSONCodec.Encode` (P7.6) · `asyncWriter.drain`'s bare `recover()` swallows a store panic **and** skips `onWrite(err)`, so the L2-failure signal never fires — H1's exact shape · `FileStore.path` maps every non-alphanumeric rune to `_`, so `prod!` and `prod?` collide onto one file and one env's last-known-good overwrites another's |
| P11.13 | Snapshot format is versioned; a client too old to parse a snapshot **refuses it and stays on the previous generation, loudly** |

**Satisfies.** O5, ADR-D3 · L12 (evaluation survives a total flag-service outage) · H1, H2,
H6 · R2 (killswitch cannot propagate during a total outage — documented, not fixed) ·
brief: never-throw, low latency.

**Exit gate.** **Killing the flag service leaves the app backend serving defaults within its
stated timeout, with no thrown error and no latency cliff** · a cold pod during a total outage
serves compiled-in defaults and says so · L2 hydration survives a restart mid-outage ·
`AllocsPerRun == 0` on the cached read path.

---

# P12 — Observability

**Objective.** Make a 3 am debug possible, and make silence detectable.

**Entry.** P4 onward (wired incrementally, gated here).

**Deliverables.** `internal/obs/` (partially built: `log.go`, `trace.go`), `pkg/flagclient/otel/`.

| # | Item |
|---|---|
| P12.1 | Metrics per the P0.7 namespace: config/build (`pending_changes_seconds`, `quarantined_flags`, `build_failures_total`, `base_revision`, `ops_overrides_active`) · service (`config_apply_total`, `snapshot_generation`, `bucketing_scheme_hash`, `subscribers`, `propagation_lag_seconds`, `subscriber_drops_total`) · client (`connected`, `staleness_seconds`, `resync_total`, `uninitialized_evaluations_total`, **`fallback_total`**) · eval (`attribute_type_mismatch_total`, `type_mismatch_total`, `no_bucket_subject_total`, `PanicTotal`) |
| P12.2 | **Cardinality guard.** `flag` and `env` and `reason` are labels. `user_id` **never** — 1 B series destroys the metrics backend before the flag service notices. `rule_id`, `variation_key`, `trace_id`, `bucket_value` are response and log fields only. **No `flag` label on latency histograms** — 12 buckets × 500 flags = 6 000 series for a number nobody segments; per-flag latency is a trace |
| P12.3 | **Enforced by types:** the metrics package exports only typed label structs with enum-typed fields. No `map[string]string` or variadic `...string` label API is exported |
| P12.4 | `tenant_id` as a label is **blocked on S-O4** |
| P12.5 | Structured log schemas per P0.10 — evaluation fault and config apply |
| P12.6 | **`diff_summary` on config-apply** — named the highest-value field in the system. Scalars carry from/to; **rule lists carry a count delta and a content hash**, so one large rule edit cannot produce a megabyte log line |
| P12.7 | **Log rate limiting is a correctness requirement, not hygiene.** Per `(flag_key, reason)` token bucket at 1/s burst 5, plus `sampled_of` so true volume is reconstructible. Without it one misconfigured flag at 1 M evals/s writes 1 M lines/s — a second-order outage worse than the flag bug |
| P12.8 | No PII in logs: attribute **keys** only, never values; `subject_hash` = first 8 hex of `xxhash(subject)`; `panic_value` truncated to 256 B; `stack_digest` = SHA-256 prefix of the normalised stack |
| P12.9 | The 3 am field set present on every relevant line: `trace_id`, `config_version`, `generation`, `reason`, `flags_changed`, `diff_summary`, `instance_id`, `bucketing_scheme_hash` |

**Satisfies.** H1, H3, H5 · L7 (structured error log on every internal error) · `AGENTS.md`
tracing and PII rules · brief: structured error logs.

**Exit gate.** For any past evaluation you can answer **which config version served it and
which rule or bucket decided it** · a cardinality test asserts the label set is closed ·
the rate limiter is proven under a 1 M-line/s synthetic storm · **`fallback_total` is wired,
and the alert fires when it is unexpectedly LOW as well as high** — a metric that is always
zero is usually a metric that is not wired up.

---

# P13 — Test Suite, Golden Vectors & Benchmarks

**Objective.** Prove the brief's five mandated areas plus every failure path, and lock the
performance claims to measured baselines.

**Entry.** P11, P12.

**Deliverables.** `test/golden/`, `test/conformance/`, `test/fuzz/`.

| Area | Content |
|---|---|
| Brief area 1 — flag types | bool, string, int across every path |
| Brief area 2 — targeting rules | match, no-match, missing attribute, wrong type |
| Brief area 3 — percentage stickiness | repeat, independence, opt-in sharing, distribution |
| Brief area 4 — environment isolation | a prod change does not leak to dev; snapshots share no memory |
| Brief area 5 — default on error | corrupt config, panic injection, unknown flag, type mismatch |
| **Conformance** | The golden fixture corpus run through **both** linkages — service binary and client library — asserting **byte-identical `Result` including `Reason`, `RuleID` and bucket**. This is the structural answer to §A.5.3's divergence objection; without it the "one implementation" claim is unbacked |
| **Golden vectors** | 500 bucketing pairs (composed key, not just hash) · 1 000-ID assignment stability, failing the build **and refusing boot** |
| **Fuzz** | matcher and config validator corpora |
| **Architecture tests** | `store.Load()` at exactly one call site · no goroutines in eval · `pkg/` does not import `internal/` · no `recover()` outside the two sites · `TestNoIOImports` |
| **Allocation** | `AllocsPerRun == 0` on the eval hot path, asserted in CI |
| **Benchmarks** | evaluation p99 single flag (< 50 µs) and batch F=100 (< 500 µs) · propagation latency · build duration · recorded as the baseline |
| **Race** | `go test -race ./...` |

**Exit gate.** `make ci` green · every benchmark recorded as a committed baseline · every
Appendix-A row ticked.

---

# P14 — Go-Live

**Objective.** Hand it to on-call without a knowledge-transfer meeting.

**Entry.** P13.

**Deliverables.** `docs/04-runbook.md`, dashboards, alert rules.

| # | Item |
|---|---|
| P14.1 | Rollout and rollback procedures; migration ordering; feature-flag-for-the-feature-flag-service posture |
| P14.2 | **Alert-to-action map** — every alert in P12 maps to one runbook section |
| P14.3 | Kill switch: how to disable a flag under incident conditions and how fast it takes effect. **R2 stated plainly: this does not work during a total flag-service outage.** Nobody plans an incident response around a flag flip that depends on a service that may be part of the incident |
| P14.4 | Re-bucketing warning: changing the bucket strategy post-launch silently re-buckets every user mid-rollout. Change-controlled operation requiring `acknowledge_reshuffle` |
| P14.5 | **Product owners must be told that "flag service degraded" means "everything reads as its default"** — a fail-open default is a *behavioural* change during an outage, not a no-op |
| P14.6 | Known limits stated: in-memory store, single region, config lost on restart (R1), staleness unbounded during a total outage (S-O3) |
| P14.7 | SLO and alert definitions loaded: propagation p99 ≤ 1 s / max 5 s hard · convergence < 99 % for 60 s pages · fallback rate > 0.1 % for 5 m pages · uninitialized evaluations > 0 for 2 m pages · staleness max > 60 s for 2 m pages · connectivity < 90 % for 3 m pages · `bucketing_scheme_hash` changes > 0 pages immediately · any `rejected_internal` pages · **`rejected_validation` never alerts** |

**Exit gate.** A person who did not build this can, from the runbook alone, disable a flag,
roll back a config version, and explain a fallback spike.

> **GATE 14 — go-live sign-off.**

---

# Appendix A — Coverage matrix

Every commitment extracted from `PLAN.md`, `docs/02-hld.md`, `docs/03-lld.md` and
`docs/diagrams/*`, mapped to the phase that discharges it. **A commitment with no phase is a
plan defect.**

## A.1 Brief requirements

| Requirement | Phase |
|---|---|
| Flag types bool/string/int | P1, P13 |
| Targeting rules | P2, P13 |
| Percentage rollout + sticky bucketing | P3, P13 |
| Independent vs shared buckets (O1) | P0.13, P3.4, P13 |
| Environments | P5, P7.4, P13 |
| Live updates < 5 s | P10 |
| Sync, low-latency, never throws | P4, P9, P11, P13 |
| Config store `set`/`get` | P8 |
| Unit tests, five areas | P13 |
| O1 bucketing key | P0 decide → P3 build |
| O2 precedence | P0 decide → P2, P3 build |
| O3 misconfiguration | P0 decide → P6 build |
| O4 pull vs push | P0 decide → P10 build |
| O5 client caching | closed → P11 build |

## A.2 Architecture calls

| Call | Phase | | Call | Phase |
|---|---|---|---|---|
| A1 two deployables | P8.2, P9.12 | | B8 ambiguous rejected | P6.6 |
| A2 concentric rings | P0.1, P13 arch tests | | C1 absent ⇒ false | P2.3 |
| A3 recover at two sites | P4.5, P9.4 | | C2 xxhash pinned | P3.1, P3.7 |
| A4 cost on write path | P7 | | C3 monotone ramp | P3.10 |
| A5 batching is the design | P9.2 | | C4 typed accessors | P4.4 |
| B1 rule lists replace/append | P5.5 | | C5 immutable snapshot | P4.3, P7.3 |
| B2 rollout deep-merges | P5.5 | | D1 hybrid propagation | P10 |
| B3 four layers | P5.3 | | D2 atomic.Pointer | P7.3 |
| B4 tri-state `Opt[T]` | P5.1 | | D3 SnapshotID | P7.7, P10.6 |
| B5 raw unmerged layers | P8.1 | | | |
| B6 eager resolution | P7 | | | |
| B7 per-env transactionality | P7.4 | | | |

## A.3 Hazards and accepted risks

| # | Detection lands in |
|---|---|
| H1 silent fail-open | P12.1 `fallback_total`, **alerted low** |
| H2 slow-not-down | P9.11, P11.9 |
| H3 silent re-bucketing | P3.8, P3.9, P12.1 |
| H4 restart loses corpus | P8.8 |
| H5 stale config is silent | P12.1 `pending_changes_seconds`, threshold from P0.11 |
| H6 cold start | P9.5, P11.5 |
| H7 append reaches prod | P5.8 provenance, P6.8 M10 |
| R1 total config loss | P8.8 + P14.6 |
| R2 killswitch during outage | P14.3, documented not fixed |
| R3 bucketing immutable | P0.13 ADR, P3.9 |
| §C.10 no subject normalisation | P3.13 |
| §D.5 call-site default = pre-flag behaviour | P0.16 owner, P14.5 |

## A.4 Invariants

| ID | Enforced in |
|---|---|
| CACHE-1 / SNAP-1 (same invariant, two IDs — P0.2) | P4.2 arch test |
| CACHE-2 build-then-swap | P7.3, P7 gate |
| CACHE-3 no I/O on read path | P4.3 |
| Zero allocation on happy path | P1.8, P4.8, P11 gate, P13 |
| Never-panics on every exported method | P4.6 reflection test |
| No goroutines in eval | P4.7 |
| Bucketing golden vectors (500) | P3.7 |
| Assignment stability (1 000, refuses boot) | P3.8 |
| Strategy purity (1 000×) | P3.12 |
| Cross-linkage conformance | P13 |
| Versioned snapshot format | P11.13 |
| Typed metric labels only | P12.3 |
| `go test -race` green | P13 |

## A.5 Validation rules

| Range | Phase |
|---|---|
| B01–B16 base | P6.1 |
| O01–O10 overlay | P6.2 |
| M01–M17 post-merge | P6.3 |
| M10 lint backstop | P6.8 |
| M13/M14 memory guards | P6.3 |
| M15 quarantine valve | P6.5 |
| §C.6 build-time checks | P6, P7.2 |

## A.6 Deferred — tracked, not lost

| Item | Disposition |
|---|---|
| Time-based targeting (`BEFORE`/`AFTER`) | Out of Phase 1. A scheduled rollout is a cron calling `set()`, where the clock lives on one machine |
| `REGEX` | Out. Revisit at ≥ 3 concrete inexpressible rules, then with a compiled-at-build regexp and a hard 1 KB cap |
| Nested boolean trees | Out. Note `diagrams/lld.md` L3's `LogicOp Combiner` contradicts this — P0.3 |
| Float attributes, array attributes | Out |
| Flag prerequisites | Out. Lands as a new reason between S5 and S6, not a `DISABLED` sub-detail |
| `CACHED` / `STALE` reasons | Deliberately excluded — a boolean staleness flag invites branching on it |
| Persistence, admin UI, multi-region | Out of scope by the brief |
| Snapshot sharding | Only above 100 MB |
| Rollback history > 3 | Belongs in a source of truth Phase 1 does not have |
| Peer replica convergence protocol | **Named in §A.1.2, deferred to §D, never designed there. P8.10 is a design task, not an implementation task** |

---

# Appendix B — Conflict register (resolve in P0)

| # | Conflict | Sides |
|---|---|---|
| 1 | `Snapshot` vs `ResolvedSnapshot` | §D vs §B |
| 2 | `internal/core` vs `internal/flag` + `internal/eval` | LLD §3.3 vs HLD §A.3 |
| 3 | Flag/Rule/Condition shape | §B direct values vs §C variations map |
| 4 | Threshold representation | basis points vs `pct/100 × 2³²` |
| 5 | `BoolValue` signature | §C.6 4-arg-1-return vs §D.5 3-arg-2-return |
| 6 | Construction blocking | `New()` §A.5.2 vs `NewClient()` §D.5 |
| 7 | `EvaluateAll` vs `EvaluateBatch` | §A.5.1 vs `diagrams/lld.md` L6 |
| 8 | Reason enum, esp. `MISSING_SUBJECT` | caller default vs deterministic `ROLLOUT_OUT` |
| 9 | `pending_changes_seconds` | > 60 s page vs > 5 s alert |
| 10 | Metric prefixes | five in play |
| 11 | Client state machine | 5 states vs 3 |
| 12 | Environment count | 3 vs 5/20 vs `+ __system` |
| 13 | Log event name and field set | `flag_eval_fault` vs `flag.evaluation.error` |
| 14 | Client-side caching | §A.5.3 rejects in prose; §D.5 + LLD §2 mandate. §A.5.3 is stale |
| 15 | Validation ID ranges | B01–B13/O01–O09 vs B01–B16/O01–O10 |
| 16 | `diagrams/lld.md` extras | a post-merge check absent from M01–M17; `LogicOp Combiner` ruled out by §C.3 |

# Appendix C — Undefined identifiers (define in P0.14, declare in P1)

`AttributeDecl` · `CompiledRule` · `LayerID` · `LayerRevisions` · `Severity` · `TypedValue` ·
`HashFnID` · `EvalContext` (sketched in a diagram only) · `Snapshot` · `FlagConfig` ·
`ValueType` · the `Operator` const set · `acknowledge_reshuffle` · `rollout.anonymous_policy` ·
`Rollout.StrategyName`

# Appendix D — Current state

| Component | Files | Status |
|---|---|---|
| `internal/core` ring 0 | value, model, contract, reason, result, evalctx | committed `b877940`, tests green, **needs `TestNoIOImports`** |
| `internal/core` ring 1 | evaluator, matcher, bucket, semver | written, only semver tested |
| `internal/config` | opt, layer, merge | **does not compile** |
| `internal/obs` | log, trace | written, zero tests |
| `pkg/client` | doc, state, cache | written, zero tests, three defects in P11.12 |
| `cmd/flagd`, `cmd/flagctl` | — | empty |
| `api/` | — | absent |
| `test/` | — | absent |
| `docs/adr/` | 0001–0004 | third ADR numbering; the two mandated ADRs unwritten |

Build order is ring 0 → 1 → 2 → 3. Current construction is **broad and out of order** —
P2, P3, P4, P5, P11 and P12 all partially written simultaneously, ahead of the gates. That is
not automatically wrong, but it means nothing is *finished*, and the first thing every phase
above does is re-validate existing code against the frozen contract rather than start clean.

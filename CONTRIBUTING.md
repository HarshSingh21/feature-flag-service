# Contributing

Short version: `make ci` must pass, every architectural decision gets an ADR, and
**you do not touch the bucketing golden vectors.**

## Before you start

Read [`PLAN.md`](PLAN.md) for the phase you are working in and
[`docs/adr/`](docs/adr/README.md) for the decisions that are already closed. Most
"why is it done this way?" questions have an ADR, and re-litigating a closed
decision in a PR review is the most expensive way to discover it.

## Merge gates

Every one of these runs in CI and can be run locally with `make ci`:

| Gate | Command | Blocking |
|---|---|---|
| Formatting | `gofmt -s -l .` must print **nothing** | yes |
| Vet | `go vet ./...` | yes |
| Build | `go build ./...` | yes |
| Tests, race-enabled | `go test -race ./...` | yes |
| Lint | `golangci-lint run` | **not yet** — advisory while the tree is being written |

Two notes on that table.

**`gofmt` exits 0 whether or not it found anything.** It reports by printing
filenames. A CI step that runs `gofmt -l .` and trusts the exit code is a
formatting gate that never fails. Both `make fmt-check` and the CI job check for
non-empty output explicitly; keep it that way.

**`-race` is not optional.** The propagation design rests on an atomic pointer
swap under concurrent readers, and a concurrent map read/write in Go is a *fatal
runtime error* that `recover()` cannot catch. A race here is not a flaky test, it
is an uncontainable production failure mode. Do not skip race-sensitive tests to
get a build green.

The lint job is `continue-on-error: true` today, deliberately: introducing a
linter as a hard gate against a half-written tree means the first person to hit a
false positive disables the whole job. Once the backlog is at zero, flip it in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml) — and prefer removing a
noisy linter from [`.golangci.yml`](.golangci.yml) over sprinkling `//nolint`
through the source.

## The bucketing golden vectors — read this before you edit anything under `internal/core`

**Changing any bucketing input silently re-buckets every user in every active
rollout.**

The inputs are: the hash function, the library version, the key composition, the
`bucket_namespace` salt, the `bucket_by` subject, the bucket space, and any string
normalisation applied before hashing.

At a rollout of `p`, the fraction of users who cross the boundary is `2p(1-p)`. At
a 10% rollout that is **18% of users** — and the aggregate rollout percentage is
still exactly 10%, so **every dashboard stays green.** There is no organic
detection signal. You find out days later, from user reports, after anything
stateful the flag gated has already been written inconsistently.

Reverting the change restores the assignments exactly, because bucketing is
deterministic. **It does not restore the data.**

So:

- The checked-in `(key, expected_bucket)` vectors and the ~1,000-subject
  assignment-stability corpus are a **contract**, not fixtures. A failing golden
  test means you broke the contract, never that the fixture is stale.
- The service asserts the same vectors at startup and **refuses to boot** on a
  diff. Do not "fix" that check.
- If a bucketing change is genuinely required, it is not a PR — it is a new scheme
  id, opted into per flag, with an ADR, a migration, and a named owner for
  reconciling state written under the old scheme. See
  [ADR-0001](docs/adr/0001-bucketing-key.md) and
  [ADR-0005](docs/adr/0005-xxhash-and-bucket-space.md).

The PR template has a checkbox for this. It is not a formality.

## Architectural decisions need an ADR

If your change decides something that is expensive to reverse — a wire format, a
merge semantic, a default that reinterprets existing config, a dependency that
consumers inherit — add a file under [`docs/adr/`](docs/adr/README.md) in the same
PR. See that README for the template and the numbering rule.

Rule of thumb: **if changing it later would require a migration, an incident
review, or an apology, write the ADR now.** Reviewers are expected to ask for one.

## Code standards

- **`internal/core` imports nothing that performs I/O.** No logger, no clock, no
  `net/*`, no `os`, no metrics registry. Errors are returned as data
  (`Result.Reason`), never logged or panicked from there. This is what makes the
  never-throw contract fuzzable rather than aspirational.
- **Imports point inward.** Ring N may import ring < N; nothing imports outward.
- **`recover()` lives at exactly two sites**, both in `internal/transport/safe`. A
  `recover()` in `internal/core` is a review reject — the core has nothing to
  recover from.
- **No coercion between value types.** `String("true")` is not a bool.
- **Cardinality:** flag key, environment and reason may be metric labels. User id,
  tenant id and rule id may not. They belong in a structured log line.
- Exported symbols document their behaviour on nil, on a missing attribute, and on
  a wrong type. That is the exit criterion from Phase 2, not a nicety.

## Tests

- A new behaviour needs a test that **fails without the change**.
- Error paths are tested, not just the happy path.
- Keep tests `t.Parallel()` where they can be, and keep them free of sleeps.
- `make test-short` is the fast loop; `make test` is the gate.

## Commit messages

Conventional Commits, imperative mood, wrapped at 72 columns:

```
<type>(<scope>): <what changed, imperative>

<why it changed, and what it costs. link the ADR or PLAN.md phase.>
```

`type` is one of `feat`, `fix`, `perf`, `refactor`, `test`, `docs`, `build`, `ci`,
`chore`. `scope` is the package: `core`, `config`, `transport`, `obs`, `client`,
`adr`.

```
feat(core): add explicit evaluation_order to Flag

Rules-first and rollout-gating accept byte-identical config, so an
implicit default would make any later change a silent behavioural
migration across every flag. See ADR-0008.
```

A breaking change to a wire format, a config schema, or anything covered by an
ADR gets a `BREAKING CHANGE:` footer naming the migration.

## Pull requests

Fill in the template — it is the gate list, not paperwork. Keep PRs to one
reviewable idea; a formatting sweep and a semantic change in one diff means
neither gets reviewed.

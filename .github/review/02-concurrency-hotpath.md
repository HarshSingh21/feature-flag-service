# Dimension: concurrency and hot path

Slug: `concurrency-hotpath`. Write findings to `findings/concurrency-hotpath.json`.

Read `.github/review/_house-rules.md` first.

## Why this dimension is severe here

The service answers ~2.4 million evaluations per second at peak from an in-memory snapshot
that is replaced while readers are running. Two Go facts make mistakes here unrecoverable
rather than merely buggy:

- **A concurrent map read/write is a fatal runtime throw.** `recover()` cannot catch it and
  the process dies. Any design that mutates a shared map after publication has an
  uncontainable failure mode, and no amount of careful-looking locking fixes it.
- **An `RWMutex` on the read path serialises against the writer.** A config swap under an
  `RWMutex` stalls every in-flight evaluation. The design uses an atomic pointer swap
  specifically to avoid this.

## What you are looking for

**Mutation after publication.** Anything that writes to a map, slice, or struct reachable
from a published snapshot pointer. Returning a `*Flag` into caller code is allowed only
because callers treat it as read-only — check whether the diff introduces a caller that
does not.

**Pin-once violations (CACHE-1).** The snapshot pointer must be loaded once per request and
threaded down, not loaded per flag. A per-flag load lets a swap land mid-request, so flag A
answers from generation N and flag B from N+1 — a cross-flag inconsistency that is invisible
in tests that evaluate one flag at a time.

**I/O on the read path (CACHE-3).** No network, no disk, no lock, no channel send, no
failable allocation, no `time.Now()` if it can be hoisted. An unknown flag is an *answer*,
not a cache miss — a fill-on-miss path is a finding, because concurrent cold-key requests
stampede the origin and at this request rate that is an outage rather than a blip.

**Allocation on the read path.** The design claims zero allocations per evaluation. Look for:
interface boxing of scalars, `[]byte`/`string` conversions, closures capturing per-call
state, `fmt.Sprintf`, appending to a slice without a preallocated buffer, a map built per
call. If the diff adds one, the finding is that the claim is now false and nothing tests it.

**Goroutine lifecycle.** A goroutine started per request or per apply, with no bound and no
shutdown path, is a leak. Check that every spawned goroutine has a termination condition and
that `close()`/`Wait()` cannot deadlock or double-close. A single-slot mailbox that drops the
newest value under load is a correctness bug, not a performance one.

**Swallowed panics.** A bare `_ = recover()` that discards the value without logging or
incrementing a counter converts a crash into silence. That is worse than the crash: the
system keeps serving wrong answers and no signal fires.

**Shutdown ordering.** A flush-on-close that races the writer, or a `Wait()` after the
channel it drains is already closed.

## Evidence bar

For a race, state the two goroutines and the interleaving. For an allocation claim, name the
line and why it escapes. "Add a mutex here" without a demonstrated interleaving is not a
finding.

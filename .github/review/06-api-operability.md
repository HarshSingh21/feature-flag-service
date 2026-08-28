# Dimension: API contract and operability

Slug: `api-operability`. Write findings to `findings/api-operability.json`.

Read `.github/review/_house-rules.md` first.

## The question this dimension asks

Can this change be deployed safely, observed, alerted on, rolled back, and debugged at 3am by
someone who did not build it?

## What you are looking for

**Timeouts on every external call.** No unbounded `Dial`, `Do`, `Read`, or channel receive.
A missing timeout is how "slow, not down" takes out the caller: the real failure mode is not
a dead dependency but a slow one, and a fast shed beats a successful response that arrives
too late. Check that the timeout is enforced on the whole call including connection setup,
and that a breaker treats *slow* as failed rather than only counting errors.

**Retry posture.** A retry on timeout multiplies load against a struggling dependency.
Retrying only on a connection-level failure before any bytes were sent, once, against the
same shared deadline, is the defensible position. A retry that resets the deadline is a
finding.

**Startup must not become a deploy-time dependency.** If construction blocks on a remote
fetch and fails hard, the flag service becomes a hard dependency of every deploy fleet-wide.
Construction must return a usable object even when the remote is unreachable, and say so
through its state and a counter.

**Cold start.** The one case with no last-known-good. A fresh process during an outage serves
compiled-in defaults; readiness must gate on having real config, and the fact must be
observable rather than silent.

**Health endpoint semantics.** Liveness and readiness answer different questions and must not
be aliased. A process with broken config is *live* — restarting it will not help — but not
*ready*. Readiness that ignores whether config was ever loaded lets a pod serve defaults to
the whole fleet while reporting green.

**Idempotency.** Every mutation and every consumer must tolerate redelivery. Check the
idempotency key actually covers what makes two applies "the same".

**Trace propagation.** A trace id through every hop — HTTP headers and message headers — and
into every log line. A hop that starts a fresh trace breaks the only tool that makes a
distributed 3am debug tractable.

**Versioning and ordering.** A bare monotonic counter is not enough to compare state across a
restart: a client at generation 900 meeting a restarted server at generation 3 concludes it
is ahead and serves stale config indefinitely with every health signal green. Identity needs
a process identity alongside the counter, and content identity needs to be derived from the
content so a rollback is recognisable.

**Rollback.** Can this change be reverted without a data migration? Is the previous state
still available, and is reverting a single operation rather than a rebuild from sources that
may no longer exist?

**Alert coverage for the new failure mode.** If the diff introduces a way to fail, there must
be a signal for it. Ask specifically: if this broke in production at 3am, what fires? If the
answer is "nothing", that is the finding — and it is `major` at minimum, because a silent
failure mode in a fail-open system is indistinguishable from normal operation.

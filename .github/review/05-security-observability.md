# Dimension: security and observability

Slug: `security-observability`. Write findings to `findings/security-observability.json`.

Read `.github/review/_house-rules.md` first.

## What you are looking for

**Log injection.** Any attacker-influenced string reaching a log line without control
characters stripped or escaped. A value containing `\n` forges an entire additional log
record; a very large value floods the pipeline. Check flag keys, attribute keys, error
strings, panic values, and stack traces. The spec requires a stack **digest**, not a raw
stack, and a panic value truncated to 256 bytes.

**PII and secrets in logs.** Attribute **keys** may be logged; attribute **values** may not.
A bucketing subject appears as a short hash, never in the clear. No API key, token,
credential, or connection string in any log line, error message, or metric label. No raw
stack trace ever reaches a caller.

**Metric cardinality.** This is a hard boundary, not a guideline. `user_id` as a label is a
billion series and destroys the metrics backend before the flag service notices. `rule_id`,
`trace_id`, `session_id`, and bucket values are response and log fields only. A `flag` label
on a *latency histogram* multiplies buckets by flag count for a number nobody segments — if
per-flag latency is genuinely needed, that is a trace. Also check the label API itself: an
exported `map[string]string` or variadic label surface makes the boundary unenforceable, so
labels should be typed structs with enum-typed fields.

**Unbounded log volume.** At a million evaluations per second, one misconfigured flag writes
a million lines per second and takes down the logging pipeline — a second-order outage
substantially worse than the flag bug that caused it. Any error log on an evaluation path
needs rate limiting per `(flag, reason)` plus a sampled-count field so true volume stays
reconstructible. An unrate-limited log on the hot path is a `blocking` finding.

**Swallowed errors that remove a signal.** Fail-open is the design: a degraded flag service
makes every flag read as its default, nothing errors, and nobody is paged. That makes the
fallback counter the only evidence the system is lying. Any path that discards an error
without incrementing a counter is a finding, and any counter that can only ever read zero is
a finding too — a metric that is always zero is usually a metric that is not wired up.

**Filesystem and path handling.** Directory permissions no wider than 0750 for cached
config. Any path built from configuration must be cleaned and confined to its intended root.
A sanitiser that maps distinct inputs to the same output is a collision bug: two environments
whose names normalise identically will overwrite each other's last-known-good.

**Write durability.** A cache write that truncates the previous good file before the new one
is complete loses the last-known-good on a crash or a full disk. Write to a temporary file,
sync, then rename.

**Authn and surface separation.** The evaluation listener and the admin listener have
different trust levels and must not share authentication. An admin mutation reachable from
the evaluation surface is `blocking`.

**Error envelopes.** A caller gets a structured envelope with a trace id, not an internal
error string and never a stack trace.

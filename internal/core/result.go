package core

// Result is the outcome of one flag evaluation.
//
// Evaluation never returns an error. Failure is data: it arrives as a Reason plus
// the caller's default in Value. This is what allows the core to import nothing
// that performs I/O while still reporting faults faithfully.
type Result struct {
	// Value is what the caller gets. Always populated, on every path.
	Value Value

	// Reason explains which path produced Value. Never ReasonUnknown on a
	// completed evaluation.
	Reason Reason

	// RuleID names the matched rule when Reason is ReasonRuleMatch.
	// High cardinality: log it, never use it as a metric label.
	RuleID string

	// Bucket is the computed bucket in 0..9999 when a rollout was consulted,
	// and -1 otherwise. Present so an operator can answer "why was this user in?"
	// without re-deriving the hash by hand during an incident.
	Bucket int32

	// Generation is the snapshot generation that served this evaluation. It is
	// what makes "which config answered this request?" answerable after the fact.
	Generation int64
}

// NoBucket is Result.Bucket when no rollout was consulted.
const NoBucket int32 = -1

// IsFallback reports whether the caller's default was returned.
func (r Result) IsFallback() bool { return r.Reason.IsFallback() }

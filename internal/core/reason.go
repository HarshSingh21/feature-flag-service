package core

// Reason explains why an evaluation returned the value it did.
//
// Every Result carries one. This is the field that makes a flag debuggable at 3am:
// "the flag returned false" is not actionable, "the flag returned false because
// rule r-country-in did not match and the user bucketed at 7431 against a threshold
// of 2000" is.
//
// Reason is low cardinality by construction and is safe to use as a metric label.
// RuleID is NOT -- it belongs in the Result and in logs, never in a label.
type Reason uint8

const (
	// ReasonUnknown is the zero value and must never be returned by a completed
	// evaluation. Seeing it in production means a code path forgot to set a reason.
	ReasonUnknown Reason = iota

	// ReasonRuleMatch: a targeting rule matched. Result.RuleID names it.
	ReasonRuleMatch

	// ReasonRolloutIn: no rule matched, and the subject bucketed inside the rollout.
	ReasonRolloutIn

	// ReasonRolloutOut: no rule matched, and the subject bucketed outside the rollout.
	ReasonRolloutOut

	// ReasonFallthrough: no rule matched and no rollout is configured.
	ReasonFallthrough

	// ReasonDisabled: the flag exists but is switched off. Returns the off value.
	ReasonDisabled

	// ReasonFlagNotFound: no such flag in this environment's snapshot.
	//
	// Because the read path never consults the config source (invariant CACHE-3),
	// a flag that was never merged into a snapshot is indistinguishable from one
	// that does not exist. A rising rate here is how a forgotten config push
	// announces itself, so it must be monitored rather than silently defaulted.
	ReasonFlagNotFound

	// ReasonTypeMismatch: the resolved value's type is not the flag's declared
	// type, or the caller asked for a type the flag is not. Returns the caller default.
	ReasonTypeMismatch

	// ReasonMissingSubject: a rollout is configured but the bucketing subject is
	// absent from the evaluation context.
	ReasonMissingSubject

	// ReasonError: an internal fault or a recovered panic. Returns the caller default.
	ReasonError
)

func (r Reason) String() string {
	switch r {
	case ReasonRuleMatch:
		return "RULE_MATCH"
	case ReasonRolloutIn:
		return "ROLLOUT_IN"
	case ReasonRolloutOut:
		return "ROLLOUT_OUT"
	case ReasonFallthrough:
		return "FALLTHROUGH"
	case ReasonDisabled:
		return "DISABLED"
	case ReasonFlagNotFound:
		return "FLAG_NOT_FOUND"
	case ReasonTypeMismatch:
		return "TYPE_MISMATCH"
	case ReasonMissingSubject:
		return "MISSING_SUBJECT"
	case ReasonError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// IsFallback reports whether this reason means the caller-supplied default was
// returned rather than a configured value.
//
// The sum of these is the signal behind hazard H1, silent fail-open: a degraded
// system returns defaults for everything and nothing errors. Alert when this rate
// is unexpectedly LOW as well as high -- a metric that is always zero is usually
// a metric that is not wired up.
func (r Reason) IsFallback() bool {
	switch r {
	case ReasonFlagNotFound, ReasonTypeMismatch, ReasonMissingSubject, ReasonError:
		return true
	default:
		return false
	}
}

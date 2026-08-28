package httpx

import (
	"strings"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/internal/obs"
)

// EvalContextWire is the JSON form of core.EvalContext.
//
// It is a separate type from the domain struct on purpose. The wire shape is a
// compatibility contract with every client version in the fleet; the domain struct is
// free to change. Collapsing them makes any domain refactor a breaking API change.
type EvalContextWire struct {
	UserID     string                `json:"user_id,omitempty"`
	TenantID   string                `json:"tenant_id,omitempty"`
	Attributes map[string]core.Value `json:"attributes,omitempty"`
}

func (c EvalContextWire) toCore() core.EvalContext {
	return core.EvalContext{UserID: c.UserID, TenantID: c.TenantID, Attributes: c.Attributes}
}

// keys returns the NAMES of the attributes present, for logging. Never the values --
// the attribute map is arbitrary caller data and will contain PII.
func (c EvalContextWire) keys() []string {
	ks := obs.MapKeys(c.Attributes)
	if c.UserID != "" {
		ks = append(ks, "user_id")
	}
	if c.TenantID != "" {
		ks = append(ks, "tenant_id")
	}
	return ks
}

// EvaluateRequest is the single-flag evaluation payload.
type EvaluateRequest struct {
	Environment string `json:"environment"`
	Flag        string `json:"flag"`
	// Default is the caller's fallback, returned whenever evaluation cannot produce
	// a configured value. It is required: without it a miss has nothing safe to
	// return, and "the service picked something" is how a kill switch fails open by
	// accident.
	Default core.Value      `json:"default"`
	Context EvalContextWire `json:"context"`
}

// EvaluateResponse is one evaluation outcome.
type EvaluateResponse struct {
	Flag        string     `json:"flag"`
	Environment string     `json:"environment"`
	Value       core.Value `json:"value"`
	Reason      string     `json:"reason"`
	RuleID      string     `json:"rule_id,omitempty"`
	// Bucket is the computed rollout bucket in 0..9999, or -1 when no rollout was
	// consulted. Present so an operator can answer "why was this user in?" during an
	// incident without re-deriving the hash by hand.
	Bucket int32 `json:"bucket"`
	// Generation is the snapshot generation that served this evaluation. It is what
	// makes "which config answered this request?" answerable after the fact.
	Generation int64  `json:"generation"`
	Fallback   bool   `json:"fallback"`
	TraceID    string `json:"trace_id,omitempty"`
}

// BatchFlag is one entry in a batch request.
type BatchFlag struct {
	Flag    string     `json:"flag"`
	Default core.Value `json:"default"`
}

// EvaluateBatchRequest evaluates many flags against ONE pinned snapshot.
//
// The batch endpoint is not a convenience. A request that needs 100 flags costs 100x
// the per-call overhead if it makes 100 calls -- at a p99 of ~2 ms per hop that is
// 200 ms of pure transport on a sub-millisecond evaluation budget. It is also the
// only shape that can offer cross-flag consistency, because one request means one
// pinned snapshot.
type EvaluateBatchRequest struct {
	Environment string          `json:"environment"`
	Context     EvalContextWire `json:"context"`
	Flags       []BatchFlag     `json:"flags"`
}

// EvaluateBatchResponse carries the pinned generation at the TOP level, once.
//
// That placement is the contract: every result in Results came from this generation.
// A per-result generation would be the shape you would use if the handler pinned per
// flag, which is exactly the bug this endpoint exists to make impossible.
type EvaluateBatchResponse struct {
	Environment string             `json:"environment"`
	Generation  int64              `json:"generation"`
	TraceID     string             `json:"trace_id,omitempty"`
	Results     []EvaluateResponse `json:"results"`
}

// HealthResponse is the /health body.
type HealthResponse struct {
	Status       string      `json:"status"`
	Service      string      `json:"service"`
	InstanceID   string      `json:"instance_id,omitempty"`
	UptimeSecond float64     `json:"uptime_seconds"`
	Environments []EnvHealth `json:"environments"`
}

// EnvHealth is the per-environment health detail.
type EnvHealth struct {
	Env        string `json:"env"`
	Loaded     bool   `json:"loaded"`
	Generation int64  `json:"generation"`
	Flags      int    `json:"flags"`
	// AgeSeconds is how long since this environment last had config applied, or -1
	// when the source does not report freshness.
	AgeSeconds float64 `json:"age_seconds"`
	// Stale is advisory only. It never gates readiness.
	Stale bool `json:"stale"`
}

// ReadyResponse is the /ready body.
type ReadyResponse struct {
	Ready bool `json:"ready"`
	// Reason is a short machine-readable explanation, e.g. "no_snapshot".
	Reason string `json:"reason,omitempty"`
	// Degraded is true when the pod is ready but serving a stale snapshot. Ready and
	// degraded is a working pod; it keeps taking traffic.
	Degraded     bool        `json:"degraded"`
	Environments []EnvHealth `json:"environments"`
}

// SnapshotDebugResponse is the admin snapshot introspection body.
type SnapshotDebugResponse struct {
	Environment string     `json:"environment"`
	Generation  int64      `json:"generation"`
	Flags       int        `json:"flags"`
	Keys        []string   `json:"keys,omitempty"`
	Flag        *core.Flag `json:"flag,omitempty"`
}

// validEnv bounds the environment string.
//
// This is a cardinality control, not tidiness. `env` is a metric label; an
// unvalidated env is a caller-controlled label value, and a caller sending a fresh
// one per request mints a series per request. Validating at the edge is what makes
// the label safe to allow at all.
func validEnv(s string) bool { return validToken(s, 32) }

// validFlagKey bounds the flag key, for the same reason.
func validFlagKey(s string) bool { return validToken(s, 128) }

func validToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return !strings.Contains(s, "..")
}

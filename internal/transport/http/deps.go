// Package httpx is the HTTP surface of the flag service.
//
// The package lives in internal/transport/http but is named httpx so that every file
// in it can import net/http without an alias. Callers import it as:
//
//	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
//
// # Two listeners, one binary
//
// The evaluation API and the admin API are separate http.Server instances on separate
// ports. They are not separate deployables -- that would buy nothing and add a second
// thing to page on -- but they are separate listeners, which buys three things that
// matter operationally:
//
//  1. Each has its own accept queue and connection limits, so a config-push storm on
//     the admin port cannot starve evaluation by filling the eval listener's queue.
//  2. Network policy can expose the eval port to the service mesh while binding the
//     admin port to an ops-only path.
//  3. They can carry different authentication: peer identity for eval, a signed
//     operator token for admin.
//
// # Dependencies are interfaces declared here
//
// Every collaborator this package needs is declared in this file, at the point of
// consumption, rather than imported from the package that will implement it. That is
// idiomatic Go, and it is also what lets the whole HTTP surface be tested against
// fakes with no store, no compiler, and no config package in the build graph.
package httpx

import (
	"context"
	"errors"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// SnapshotSource hands out the live snapshot for an environment.
//
// Implementations must make Snapshot a lock-free pointer load. It is on the read
// path, which performs no I/O and acquires no lock (invariant CACHE-3).
//
// ok is false when NO snapshot has ever been published for env. That is not an error
// state -- a fresh replica legitimately has none, and every lookup against it returns
// the caller's default, which is the safe value by construction. It is, however, the
// state that must keep /ready false: a pod that has never loaded config would
// otherwise silently serve the whole fleet on compile-time defaults.
type SnapshotSource interface {
	Snapshot(env string) (core.Snapshot, bool)

	// Environments lists every environment that currently has a published snapshot.
	// Used by /health and by readiness when no required set is configured.
	Environments() []string
}

// FreshnessReporter is optionally implemented by a SnapshotSource.
//
// It exists to report staleness, NOT to gate readiness. A pod serving a
// stale-but-valid snapshot is a working pod; refusing its traffic during a
// control-plane incident converts that incident into a data-plane one. Staleness
// belongs on a gauge and in the /health body, never in the /ready decision.
type FreshnessReporter interface {
	LastAppliedAt(env string) (time.Time, bool)
}

// Evaluator evaluates one flag against an already-pinned snapshot.
//
// The signature is *core.Evaluator's, exactly, so the real engine satisfies this
// interface with no adapter. The compile-time assertion lives in cmd/flagd.
//
// The snapshot is a parameter, not something the evaluator fetches. That is what
// makes invariant CACHE-1 enforceable at the transport layer: the batch handler pins
// once and passes the same pointer for every flag in the request, so a config swap
// landing mid-batch cannot return flag A from generation N and flag B from N+1.
//
// want is the type the caller is asserting. Transport derives it from the type of the
// caller's default, which is the caller's type declaration already -- a caller that
// sends `"default": false` has said the flag is a bool, and asking for a second,
// separately-specified type field would create a way for the two to disagree.
//
// Evaluate must never return an error and must never panic. It returns faults as
// data in Result.Reason. The panic boundary around it is belt and braces, not the
// design.
type Evaluator interface {
	Evaluate(snap core.Snapshot, key string, ec core.EvalContext, want core.ValueType, def core.Value) core.Result
}

// KeyLister is optionally implemented by a core.Snapshot.
//
// The admin snapshot endpoint uses it to list flag keys when available. It is
// optional because core.Snapshot deliberately does not require key iteration: the
// read path never needs it, and an interface method that only debugging uses is an
// invitation to iterate a snapshot on the hot path.
type KeyLister interface {
	Keys() []string
}

// LayerApplier accepts a raw config layer on the admin path.
//
// The body is passed through as raw bytes because the layer document's schema is
// owned by the config package, not by transport. Transport's job is authentication,
// size limits, timeouts, and the error envelope -- not knowing what a layer looks
// like.
//
// Implementations must be idempotent: re-applying an identical layer must produce the
// same generation-consistent state, because the publisher retries.
type LayerApplier interface {
	ApplyLayer(ctx context.Context, body []byte) (ApplyResult, error)
}

// ErrValidation marks a config rejection as the caller's fault.
//
// An implementation signals it with fmt.Errorf("...: %w", httpx.ErrValidation) or by
// wrapping. It maps to 400 and, deliberately, never pages: the operator got a
// synchronous rejection, the live snapshot was never touched, and the system behaved
// exactly as designed. Paging a human for a user's typo is how you train people to
// ignore pages. Anything that is NOT ErrValidation maps to 500 and DOES page.
var ErrValidation = errors.New("config validation failed")

// AppliedEnv describes one environment affected by a config apply.
type AppliedEnv struct {
	Env string `json:"env"`
	// Generation is the generation the environment is on AFTER the apply.
	Generation int64 `json:"generation"`
	// Flags is the flag count in the resulting snapshot.
	Flags int `json:"flags"`
}

// ApplyResult is what a successful apply reports back to the publisher.
type ApplyResult struct {
	Applied []AppliedEnv `json:"applied"`
	// FlagsChanged names the flags whose resolved value changed. It is the single
	// highest-value field in an incident, because the first question is always
	// "what changed". Empty is legal and means a no-op apply.
	FlagsChanged []string `json:"flags_changed,omitempty"`
}

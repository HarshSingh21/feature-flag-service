package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/HarshSingh21/feature-flag-service/internal/config"
	"github.com/HarshSingh21/feature-flag-service/internal/core"
	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
)

// storeSource adapts *config.Store to httpx.SnapshotSource.
//
// The adapter exists only to widen the return type: Store.Snapshot returns a
// *config.ResolvedSnapshot, and the transport consumes the core.Snapshot interface.
// It cannot be elided, because a Go method set is not covariant in its results --
// and it must not be written as a direct type assertion either, since a typed-nil
// *ResolvedSnapshot inside a non-nil core.Snapshot interface would make /ready
// report healthy while every lookup returned the caller's default. That is hazard
// H6 in the plan: a deploy quietly serving the whole fleet on compiled-in defaults.
type storeSource struct{ st *config.Store }

func (s storeSource) Snapshot(env string) (core.Snapshot, bool) {
	snap, ok := s.st.Snapshot(env)
	if !ok || snap == nil {
		return nil, false
	}
	return snap, true
}

func (s storeSource) Environments() []string { return s.st.Environments() }

// layerApplier adapts *config.Store to httpx.LayerApplier.
type layerApplier struct{ st *config.Store }

// layerEnvelope is the admin wire format. The kind discriminator is explicit
// rather than inferred from which fields are present: a base layer and an overlay
// differ structurally, and guessing between them would mean a typo in one silently
// applying as the other.
type layerEnvelope struct {
	Kind string          `json:"kind"` // base | overlay | ops
	Body json.RawMessage `json:"body"`
}

func (a layerApplier) ApplyLayer(_ context.Context, body []byte) (httpx.ApplyResult, error) {
	var env layerEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return httpx.ApplyResult{}, fmt.Errorf("malformed layer envelope: %v: %w", err, httpx.ErrValidation)
	}

	var layer config.Layer
	switch strings.ToLower(strings.TrimSpace(env.Kind)) {
	case "base":
		var l config.BaseLayer
		if err := json.Unmarshal(env.Body, &l); err != nil {
			return httpx.ApplyResult{}, fmt.Errorf("malformed base layer: %v: %w", err, httpx.ErrValidation)
		}
		layer = &l
	case "overlay":
		var l config.OverlayLayer
		if err := json.Unmarshal(env.Body, &l); err != nil {
			return httpx.ApplyResult{}, fmt.Errorf("malformed overlay layer: %v: %w", err, httpx.ErrValidation)
		}
		layer = &l
	case "ops":
		var l config.OpsLayer
		if err := json.Unmarshal(env.Body, &l); err != nil {
			return httpx.ApplyResult{}, fmt.Errorf("malformed ops layer: %v: %w", err, httpx.ErrValidation)
		}
		layer = &l
	default:
		return httpx.ApplyResult{}, fmt.Errorf("unknown layer kind %q, want base or overlay or ops: %w",
			env.Kind, httpx.ErrValidation)
	}

	rep := a.st.Set(layer)

	// A rejection is the operator's mistake, not the service's. It maps to 400 and
	// must never page. Anything else would be a 500, which does.
	if err := rep.Err(); err != nil {
		return httpx.ApplyResult{}, fmt.Errorf("%v: %w", err, httpx.ErrValidation)
	}

	out := httpx.ApplyResult{}
	for _, e := range rep.PerEnv {
		if !e.Published {
			continue
		}
		flags := 0
		if snap, ok := a.st.Snapshot(e.Env); ok {
			flags = snap.Len()
		}
		out.Applied = append(out.Applied, httpx.AppliedEnv{
			Env:        e.Env,
			Generation: e.Generation,
			Flags:      flags,
		})
		// Only report a change when content a client evaluates actually moved.
		// Publishing without a content change is an accepted no-op build; listing
		// it as "changed" would make the first question in an incident -- what
		// changed? -- answer with noise.
		if e.ContentChanged {
			out.FlagsChanged = append(out.FlagsChanged, e.Env)
		}
	}
	return out, nil
}

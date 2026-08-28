// Package e2e is the two-client end-to-end suite specified by
// docs/05-consistency-and-e2e.md §2.
//
// It stands up the REAL service — internal/config.Store for the write path,
// internal/core.Evaluator for evaluation, and the real internal/transport/http
// surface behind httptest — and drives it with two clients that share nothing
// but that service:
//
//   - Client A is an operator: it PUSHes config with an HTTP POST to the admin
//     listener's /v1/config/layers and reads nothing else.
//   - Client B is an application: a pkg/client.Client whose Source speaks HTTP
//     to the admin listener, evaluating in a tight loop against its own L1
//     cache and recording every answer.
//
// Every assertion is made against the recorded observation log, never against a
// sleep. See observationLog.
package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
)

const (
	envProd = "prod"
	envDev  = "dev"

	// flagCount is the p99 request shape from docs/03-lld.md: 100 flags in one
	// batch, one pinned snapshot.
	flagCount = 100

	// The three tokens A1 distinguishes between. callerDefault is deliberately a
	// value the config can never produce, so "B returned its own default" is
	// visible in the log rather than indistinguishable from a configured value.
	valueOld      = "OLD"
	valueNew      = "NEW"
	callerDefault = "CALLER_DEFAULT"

	instanceID = "flagd-e2e-0"
)

// flagKeys returns the n keys the suite configures. They satisfy the store's
// key charset (^[a-z0-9][a-z0-9._-]{0,127}$).
func flagKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("checkout.flag-%03d", i)
	}
	return out
}

// ---------------------------------------------------------------------------
// Layer documents, authored as JSON because that is what crosses the wire.
//
// The `layer` discriminator is read by the test's LayerApplier (harness_test.go)
// and ignored by the config package's decoders, which skip unknown keys.
// ---------------------------------------------------------------------------

type wireBaseFlag struct {
	Key          string `json:"key"`
	Type         string `json:"type"`
	Owner        string `json:"owner"`
	Description  string `json:"description,omitempty"`
	Enabled      bool   `json:"enabled"`
	DefaultValue any    `json:"default_value"`
	OffValue     any    `json:"off_value"`
}

type wireBase struct {
	Layer         string         `json:"layer"`
	SchemaVersion int            `json:"schema_version"`
	Flags         []wireBaseFlag `json:"flags"`
}

type wireOverlayFlag struct {
	Key          string `json:"key"`
	DefaultValue any    `json:"default_value"`
}

type wireOverlay struct {
	Layer         string            `json:"layer"`
	SchemaVersion int               `json:"schema_version"`
	Environment   string            `json:"environment"`
	Flags         []wireOverlayFlag `json:"flags"`
}

// baseLayer builds a valid base layer where every one of the n flags resolves to
// value.
//
// Every flag carries the SAME token on purpose. It turns A3 from "all results
// share a generation" into the far stronger "all results share a generation AND
// a value": a batch torn across a config swap shows up as two different tokens
// in one result set, which is exactly the cross-flag inconsistency invariant
// CACHE-1 exists to make impossible.
func baseLayer(n int, value string) []byte {
	doc := wireBase{Layer: "base", SchemaVersion: 1}
	for _, k := range flagKeys(n) {
		doc.Flags = append(doc.Flags, wireBaseFlag{
			Key:          k,
			Type:         "string",
			Owner:        "team-checkout",
			Enabled:      true,
			DefaultValue: value,
			OffValue:     value,
		})
	}
	return mustJSON(doc)
}

// overlayLayer flips every flag in one environment to value. This is the write
// under test in the main scenario: it touches ONE environment, which is what
// makes the environment-isolation variant meaningful.
func overlayLayer(env string, n int, value string) []byte {
	doc := wireOverlay{Layer: "overlay", SchemaVersion: 1, Environment: env}
	for _, k := range flagKeys(n) {
		doc.Flags = append(doc.Flags, wireOverlayFlag{Key: k, DefaultValue: value})
	}
	return mustJSON(doc)
}

// invalidBaseLayer is a base document with three independent, individually
// diagnosable violations:
//
//	B02 unknown type, B03 default_value type disagreeing with the declared type,
//	B01 a key outside the permitted charset.
//
// Base findings are reject-global, so a correct implementation publishes
// NOTHING, in any environment, and every reader keeps its last-known-good.
func invalidBaseLayer() []byte {
	return []byte(`{
	  "layer": "base",
	  "schema_version": 1,
	  "flags": [
	    {"key": "checkout.flag-000", "type": "bool",   "owner": "team-checkout", "enabled": true, "default_value": "not-a-bool", "off_value": false},
	    {"key": "checkout.flag-001", "type": "sparkle","owner": "team-checkout", "enabled": true, "default_value": "x",          "off_value": "x"},
	    {"key": "NOT A VALID KEY",   "type": "string", "owner": "team-checkout", "enabled": true, "default_value": "x",          "off_value": "x"}
	  ]
	}`)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("e2e: marshal fixture: %v", err))
	}
	return b
}

// requireEqual is a tiny assertion helper; the suite deliberately has no
// third-party test dependency.
func requireEqual[T comparable](t *testing.T, got, want T, format string, args ...any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", fmt.Sprintf(format, args...), got, want)
	}
}

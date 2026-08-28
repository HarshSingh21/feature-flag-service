// Package demo is a single, readable, end-to-end walkthrough of what this service
// actually does.
//
// Run it and read the output:
//
//	go test -v -run TestWalkthrough ./test/demo/
//
// It is written to be read top to bottom like a story, not to maximise coverage.
// Every step prints what happened and why it matters. If you want to understand
// this system in ten minutes, this is the file.
package demo

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/HarshSingh21/feature-flag-service/internal/config"
	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// ---------------------------------------------------------------------------
// The config an operator would actually write.
// ---------------------------------------------------------------------------

// baseLayerJSON is the BASE layer: shared by every environment.
// Think of it as values.yaml in a Helm chart.
const baseLayerJSON = `{
  "schema_version": 1,
  "flags": [
    {
      "key": "new-checkout",
      "type": "bool",
      "owner": "payments",
      "description": "Rewritten checkout flow",
      "enabled": true,
      "default_value": false,
      "off_value": false,
      "evaluation_order": "rules_first",
      "rules": [
        {
          "id": "r-internal-staff",
          "conditions": [{"attribute": "email_domain", "op": "eq", "values": ["acme.internal"]}],
          "value": true
        },
        {
          "id": "r-blocked-region",
          "conditions": [{"attribute": "country", "op": "in", "values": ["KP", "IR"]}],
          "value": false
        }
      ],
      "rollout": {
        "basis_points": 0,
        "bucket_namespace": "",
        "bucket_by": "user_id",
        "on_value": true,
        "off_value": false
      }
    },
    {
      "key": "checkout-banner-text",
      "type": "string",
      "owner": "growth",
      "enabled": true,
      "default_value": "Complete your order"
    },
    {
      "key": "cart-max-items",
      "type": "int",
      "owner": "payments",
      "enabled": true,
      "default_value": 50
    }
  ]
}`

// prodOverlayJSON is the PROD overlay: only what differs from base.
// Think of it as values-prod.yaml. Everything unstated is INHERITED.
const prodOverlayJSON = `{
  "schema_version": 1,
  "environment": "prod",
  "flags": [
    {
      "key": "new-checkout",
      "rollout": {"basis_points": 1000}
    },
    {
      "key": "cart-max-items",
      "default_value": 20
    }
  ]
}`

// ---------------------------------------------------------------------------

func TestWalkthrough(t *testing.T) {
	step := stepper{t: t}
	store := config.New()
	eval := core.New()

	// -----------------------------------------------------------------------
	step.title("1. An operator pushes the BASE layer")
	// -----------------------------------------------------------------------
	base := decodeBase(t, baseLayerJSON)
	rep := store.Set(base)
	if err := rep.Err(); err != nil {
		t.Fatalf("base layer rejected: %v", err)
	}
	for _, env := range store.Environments() {
		snap, _ := store.Snapshot(env)
		step.logf("env %-8s generation=%d flags=%d", env, snap.Generation(), snap.Len())
	}
	step.note("One push, three environments built. Each gets its own immutable snapshot.")

	// -----------------------------------------------------------------------
	step.title("2. A PROD overlay changes two fields. Everything else is inherited")
	// -----------------------------------------------------------------------
	rep = store.Set(decodeOverlay(t, prodOverlayJSON))
	if err := rep.Err(); err != nil {
		t.Fatalf("prod overlay rejected: %v", err)
	}
	devSnap, _ := store.Snapshot("dev")
	prodSnap, _ := store.Snapshot("prod")

	devCart, _ := devSnap.Flag("cart-max-items")
	prodCart, _ := prodSnap.Flag("cart-max-items")
	step.logf("cart-max-items   dev=%v   prod=%v", devCart.DefaultValue, prodCart.DefaultValue)

	devCheckout, _ := devSnap.Flag("new-checkout")
	prodCheckout, _ := prodSnap.Flag("new-checkout")
	step.logf("new-checkout rollout   dev=%d bp   prod=%d bp",
		devCheckout.Rollout.BasisPoints, prodCheckout.Rollout.BasisPoints)
	step.logf("prod generation=%d, dev generation=%d (dev did NOT rebuild)",
		prodSnap.Generation(), devSnap.Generation())

	// The incident-shaped bug this design prevents: the overlay set ONLY
	// basis_points. If rollout replaced instead of deep-merging, bucket_by and
	// bucket_namespace would now be blank -- silently re-bucketing every user.
	if prodCheckout.Rollout.BucketBy != "user_id" {
		t.Fatalf("bucket_by was lost in the merge: %q", prodCheckout.Rollout.BucketBy)
	}
	step.note("rollout DEEP-MERGES. bucket_by survived a basis_points-only overlay.")
	step.note("A whole-block replace would have blanked it and re-bucketed every user.")

	// -----------------------------------------------------------------------
	step.title("3. Targeting rules: first match wins, and the rollout never runs")
	// -----------------------------------------------------------------------
	staff := core.EvalContext{
		UserID:     "u-1001",
		Attributes: map[string]core.Value{"email_domain": core.String("acme.internal")},
	}
	res := eval.Evaluate(prodSnap, "new-checkout", staff, core.TypeBool, core.Bool(false))
	step.logf("internal staff        -> %-5v  reason=%-12s rule=%s",
		res.Value, res.Reason, res.RuleID)

	blocked := core.EvalContext{
		UserID:     "u-1002",
		Attributes: map[string]core.Value{"country": core.String("KP")},
	}
	res = eval.Evaluate(prodSnap, "new-checkout", blocked, core.TypeBool, core.Bool(false))
	step.logf("blocked region        -> %-5v  reason=%-12s rule=%s",
		res.Value, res.Reason, res.RuleID)
	step.note("A matching rule returns immediately. The 10% rollout is never consulted.")

	// -----------------------------------------------------------------------
	step.title("4. The absent-attribute rule -- the one most flag systems get wrong")
	// -----------------------------------------------------------------------
	noGeo := core.EvalContext{UserID: "u-1003"} // country attribute MISSING
	res = eval.Evaluate(prodSnap, "new-checkout", noGeo, core.TypeBool, core.Bool(false))
	step.logf("no country attribute  -> %-5v  reason=%s", res.Value, res.Reason)
	if res.Reason == core.ReasonRuleMatch && res.RuleID == "r-blocked-region" {
		t.Fatal("absent country matched the blocked-region rule")
	}
	step.note("An ABSENT attribute makes a condition FALSE, before negation is applied.")
	step.note("Otherwise a failed geo lookup would silently match every user on Earth.")

	// -----------------------------------------------------------------------
	step.title("5. Percentage rollout: sticky, and independent per flag")
	// -----------------------------------------------------------------------
	sample := core.EvalContext{UserID: "u-777"}
	first := eval.Evaluate(prodSnap, "new-checkout", sample, core.TypeBool, core.Bool(false))
	step.logf("u-777 bucket=%d of 10000, threshold=%d -> %v (%s)",
		first.Bucket, prodCheckout.Rollout.BasisPoints, first.Value, first.Reason)

	for i := 0; i < 1000; i++ { // stickiness: same answer, every time
		again := eval.Evaluate(prodSnap, "new-checkout", sample, core.TypeBool, core.Bool(false))
		if again.Bucket != first.Bucket || !again.Value.Equal(first.Value) {
			t.Fatalf("not sticky: call %d gave bucket=%d value=%v", i, again.Bucket, again.Value)
		}
	}
	step.note("1000 calls, identical answer. Bucketing is pure computation -- nothing stored.")

	// Enrolment across a population should land near the configured 10%.
	enrolled := 0
	const population = 20000
	for i := 0; i < population; i++ {
		ctx := core.EvalContext{UserID: fmt.Sprintf("user-%d", i)}
		if r := eval.Evaluate(prodSnap, "new-checkout", ctx, core.TypeBool, core.Bool(false)); r.Reason == core.ReasonRolloutIn {
			enrolled++
		}
	}
	pct := float64(enrolled) / float64(population) * 100
	step.logf("%d of %d users enrolled = %.2f%% (configured 10.00%%)", enrolled, population, pct)
	if pct < 9 || pct > 11 {
		t.Fatalf("rollout distribution off target: %.2f%%", pct)
	}
	step.note("At 1 billion users this costs ZERO bytes. Storing assignments would be ~16 GB per flag.")

	// -----------------------------------------------------------------------
	step.title("6. Environment isolation is real, not a naming convention")
	// -----------------------------------------------------------------------
	devRes := eval.Evaluate(devSnap, "cart-max-items", sample, core.TypeInt, core.Int(0))
	prodRes := eval.Evaluate(prodSnap, "cart-max-items", sample, core.TypeInt, core.Int(0))
	step.logf("cart-max-items   dev=%v   prod=%v", devRes.Value, prodRes.Value)
	step.note("Same flag, same user, different answer per environment.")

	// -----------------------------------------------------------------------
	step.title("7. A BAD config push is rejected -- and changes nothing")
	// -----------------------------------------------------------------------
	before, _ := store.Snapshot("prod")
	fingerprintBefore := before.Fingerprint()

	bad := decodeOverlay(t, `{
	  "schema_version": 1,
	  "environment": "prod",
	  "flags": [
	    {"key": "cart-max-items", "default_value": "not-an-integer"},
	    {"key": "new-checkout", "rollout": {"basis_points": 99999}}
	  ]
	}`)
	rep = store.Set(bad)
	step.logf("push rejected: %v", rep.Err() != nil)
	for _, f := range rep.Findings().Rejections() {
		step.logf("  REJECT [%s] flag=%s field=%s: %s", f.RuleID, f.Flag, f.Field, f.Message)
	}

	after, _ := store.Snapshot("prod")
	step.logf("prod generation before=%d after=%d, content identical=%v",
		before.Generation(), after.Generation(), after.Fingerprint() == fingerprintBefore)
	if after.Fingerprint() != fingerprintBefore {
		t.Fatal("a rejected push mutated the serving snapshot")
	}
	step.note("Served content is untouched. Last-known-good keeps serving.")
	step.note("Every violation is reported at once -- not one per round trip.")

	// The generation advances because a build was accepted -- it is the operator's
	// audit counter, so "did my push land?" stays answerable. But subscribers are
	// only woken when content a CLIENT evaluates actually changed. Retrying the
	// same bad push therefore does not churn the fleet.
	repeat := store.Set(bad)
	step.logf("same bad push again: generation moves, content changed=%v",
		repeat.PerEnv["prod"].ContentChanged)
	step.note("Generation is an operator audit counter; the subscriber wake-up is separate.")
	step.note("A CI job retrying a bad push would otherwise re-swap every client, forever.")

	// -----------------------------------------------------------------------
	step.title("8. A good update lands, and the generation advances")
	// -----------------------------------------------------------------------
	rep = store.Set(decodeOverlay(t, `{
	  "schema_version": 1,
	  "environment": "prod",
	  "flags": [{"key": "new-checkout", "rollout": {"basis_points": 10000}}]
	}`))
	if err := rep.Err(); err != nil {
		t.Fatalf("ramp to 100%% rejected: %v", err)
	}
	rampSnap, _ := store.Snapshot("prod")
	res = eval.Evaluate(rampSnap, "new-checkout", sample, core.TypeBool, core.Bool(false))
	step.logf("after ramp to 100%%: u-777 -> %v (%s), generation %d -> %d",
		res.Value, res.Reason, after.Generation(), rampSnap.Generation())
	step.note("Raising a percentage only ever ADDS users. Nobody enrolled gets re-bucketed.")

	// -----------------------------------------------------------------------
	step.title("9. Evaluation NEVER throws -- even when everything is wrong")
	// -----------------------------------------------------------------------
	cases := []struct {
		what string
		run  func() core.Result
	}{
		{"flag does not exist", func() core.Result {
			return eval.Evaluate(rampSnap, "no-such-flag", sample, core.TypeBool, core.Bool(true))
		}},
		{"caller asks wrong type", func() core.Result {
			return eval.Evaluate(rampSnap, "cart-max-items", sample, core.TypeBool, core.Bool(true))
		}},
		{"nil snapshot", func() core.Result {
			return eval.Evaluate(nil, "new-checkout", sample, core.TypeBool, core.Bool(true))
		}},
		{"snapshot that panics", func() core.Result {
			return eval.Evaluate(panicSnapshot{}, "new-checkout", sample, core.TypeBool, core.Bool(true))
		}},
	}
	for _, c := range cases {
		r := c.run()
		step.logf("%-24s -> %-5v  reason=%s", c.what, r.Value, r.Reason)
		if !r.Value.Equal(core.Bool(true)) {
			t.Fatalf("%s did not return the caller default", c.what)
		}
	}
	step.note("Every failure returns the CALLER'S default. No error, no panic, ever.")
	step.note("That is why the default is a mandatory argument at the call site:")
	step.note("when config is unavailable there is nothing to read a default FROM.")

	step.title("Walkthrough complete")
}

// panicSnapshot is a deliberately hostile Snapshot: it detonates on use.
type panicSnapshot struct{}

func (panicSnapshot) Generation() int64 { return 0 }
func (panicSnapshot) Env() string       { return "prod" }
func (panicSnapshot) Len() int          { return 0 }
func (panicSnapshot) Flag(string) (*core.Flag, bool) {
	panic("simulated corruption inside the snapshot")
}

// ---------------------------------------------------------------------------
// Narration helpers -- these exist purely to make the output readable.
// ---------------------------------------------------------------------------

type stepper struct {
	t *testing.T
	n int
}

func (s *stepper) title(msg string) {
	s.t.Helper()
	s.n++
	s.t.Logf("\n%s\n%s\n%s", strings.Repeat("=", 74), msg, strings.Repeat("=", 74))
}

func (s *stepper) logf(format string, args ...any) {
	s.t.Helper()
	s.t.Logf("    "+format, args...)
}

func (s *stepper) note(msg string) {
	s.t.Helper()
	s.t.Logf("  > %s", msg)
}

func decodeBase(t *testing.T, s string) *config.BaseLayer {
	t.Helper()
	var l config.BaseLayer
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		t.Fatalf("decode base layer: %v", err)
	}
	return &l
}

func decodeOverlay(t *testing.T, s string) *config.OverlayLayer {
	t.Helper()
	var l config.OverlayLayer
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		t.Fatalf("decode overlay: %v", err)
	}
	return &l
}

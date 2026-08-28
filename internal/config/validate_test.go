package config

import (
	"strings"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

func assertHas(t *testing.T, fs Findings, ruleIDs ...string) {
	t.Helper()
	for _, id := range ruleIDs {
		if !fs.Has(id) {
			t.Fatalf("want finding %s, got %v\n%s", id, fs.RuleIDs(), fs.Error())
		}
	}
}

func assertNot(t *testing.T, fs Findings, ruleIDs ...string) {
	t.Helper()
	for _, id := range ruleIDs {
		if fs.Has(id) {
			t.Fatalf("unexpected finding %s\n%s", id, fs.Error())
		}
	}
}

// -----------------------------------------------------------------------------
// Pass 1 -- pre-merge
// -----------------------------------------------------------------------------

func TestValidateBaseAcceptsCanonicalLayer(t *testing.T) {
	t.Parallel()
	fs := ValidateBase(mustBase(t, canonicalBase))
	if fs.HasRejections() {
		t.Fatalf("canonical base rejected:\n%s", fs.Error())
	}
}

func TestValidateBaseAggregatesEveryViolation(t *testing.T) {
	t.Parallel()
	// One document, many independent problems. The validator must report all of
	// them: an operator fixing config one error per round trip is a bad experience.
	fs := ValidateBase(mustBase(t, `{"flags":[
      {"key":"Bad Key!","type":"nope","enabled":true,"default_value":1},
      {"key":"a.b","type":"bool","enabled":true,"default_value":"not-a-bool","off_value":3,
       "evaluation_order":"rollout_gates_rules",
       "rules":[
         {"id":"","conditions":[{"attribute":"x","op":"eq","values":["v"]}],"value":true},
         {"id":"dup","conditions":[{"attribute":"x","op":"weird","values":["v"]}],"value":true},
         {"id":"dup","conditions":[],"value":true},
         {"id":"arity","conditions":[{"attribute":"x","op":"in","values":[]}],"value":true}
       ],
       "rollout":{"basis_points":10001,"on_value":true,"off_value":false}},
      {"key":"a.b","type":"bool","enabled":true,"default_value":false}
    ]}`))

	assertHas(t, fs,
		"B01", // malformed key
		"B02", // unknown value type
		"B03", // default_value type mismatch
		"B04", // off_value type mismatch
		"B05", // empty rule id AND duplicate rule id
		"B06", // unknown operator
		"B07", // empty rule + operator arity
		"B08", // basis points out of range
		"B12", // duplicate flag key
		"B13", // owner missing (warn)
		"B18", // unknown evaluation_order
	)
	if len(fs) < 12 {
		t.Fatalf("expected the full aggregate, got %d findings:\n%s", len(fs), fs.Error())
	}
	// Base findings are the one global blast radius.
	for _, f := range fs.Rejections() {
		if f.Severity != SeverityRejectGlobal {
			t.Fatalf("base rejection %s has severity %s, want reject-global", f.RuleID, f.Severity)
		}
	}
	// Every finding is actionable on its own.
	for _, f := range fs {
		if f.RuleID == "" || f.Message == "" || f.Layer != LayerBase {
			t.Fatalf("finding is not actionable: %+v", f)
		}
		if f.Severity.IsRejection() && f.Field == "" {
			t.Fatalf("rejection with no field path: %+v", f)
		}
	}
}

func TestValidateBaseRequiresCompleteRecord(t *testing.T) {
	t.Parallel()
	fs := ValidateBase(mustBase(t, `{"flags":[{"key":"a.b"}]}`))
	assertHas(t, fs, "B00")
	n := 0
	for _, f := range fs {
		if f.RuleID == "B00" {
			n++
		}
	}
	if n != 3 { // type, enabled, default_value
		t.Fatalf("want 3 B00 findings, got %d:\n%s", n, fs.Error())
	}
}

func TestValidateOverlayRejectsTypeRestatement(t *testing.T) {
	t.Parallel()
	// Rejected even when it MATCHES: allowing a matching restatement is what
	// invites a future non-matching one.
	fs := ValidateOverlay(mustOverlay(t, `{"environment":"prod","flags":[{"key":"a.b","type":"bool"}]}`))
	assertHas(t, fs, "O02")
	if fs.MaxSeverity() != SeverityRejectFlag {
		t.Fatalf("overlay findings must be flag-scoped, got %s", fs.MaxSeverity())
	}
}

func TestValidateOverlayRejectsNullOnScalars(t *testing.T) {
	t.Parallel()
	// For a scalar in a strict precedence chain, null and absent mean the same
	// thing, so a null is always author confusion.
	fs := ValidateOverlay(mustOverlay(t, `{"environment":"prod","flags":[{
      "key":"a.b","enabled":null,"default_value":null,"off_value":null,"evaluation_order":null,
      "rollout":{"basis_points":null,"bucket_namespace":null,"bucket_by":null,"on_value":null,"off_value":null}}]}`))
	assertHas(t, fs, "O04")
	n := 0
	for _, f := range fs {
		if f.RuleID == "O04" {
			n++
		}
	}
	if n != 9 {
		t.Fatalf("want 9 O04 findings (4 scalars + 5 rollout scalars), got %d:\n%s", n, fs.Error())
	}
	// rollout itself may be null; that is the nullable composite.
	fs = ValidateOverlay(mustOverlay(t, `{"environment":"prod","flags":[{"key":"a.b","rollout":null}]}`))
	assertNot(t, fs, "O04")
}

func TestValidateOverlayRuleListModeMustBeDeclared(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"rules without mode": `{"environment":"prod","flags":[{"key":"a.b","rules":[]}]}`,
		"mode without rules": `{"environment":"prod","flags":[{"key":"a.b","rules_mode":"append"}]}`,
		"unknown mode":       `{"environment":"prod","flags":[{"key":"a.b","rules":[],"rules_mode":"prepend"}]}`,
		"null + append":      `{"environment":"prod","flags":[{"key":"a.b","rules":null,"rules_mode":"append"}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			fs := ValidateOverlay(mustOverlay(t, doc))
			assertHas(t, fs, "O03")
		})
	}
	for _, ok := range []string{
		`{"environment":"prod","flags":[{"key":"a.b","rules":[],"rules_mode":"replace"}]}`,
		`{"environment":"prod","flags":[{"key":"a.b","rules":[],"rules_mode":"append"}]}`,
		`{"environment":"prod","flags":[{"key":"a.b","rules":null,"rules_mode":"replace"}]}`,
	} {
		if fs := ValidateOverlay(mustOverlay(t, ok)); fs.Has("O03") {
			t.Fatalf("valid rules_mode rejected: %s\n%s", ok, fs.Error())
		}
	}
}

func TestValidateOverlayRangeAndDuplicates(t *testing.T) {
	t.Parallel()
	fs := ValidateOverlay(mustOverlay(t, `{"environment":"prod","flags":[
      {"key":"a.b","rollout":{"basis_points":-1}},
      {"key":"a.b","rollout":{"basis_points":10001}},
      {"key":"a.b","evaluation_order":"rollout_then_rules"},
      {"key":"a.b","rules_mode":"replace","rules":[{"id":"x","conditions":[{"attribute":"c","op":"nope","values":["v"]}],"value":true}]}
    ]}`))
	assertHas(t, fs, "O05", "O11", "O12", "B06")
}

func TestValidateOpsRequiresTTL(t *testing.T) {
	t.Parallel()
	fs := ValidateOps(mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false}]}`), testNow)
	assertHas(t, fs, "O08")
	// expires_at, reason and owner are each separately named.
	fields := map[string]bool{}
	for _, f := range fs {
		if f.RuleID == "O08" {
			fields[f.Field] = true
		}
	}
	for _, want := range []string{FieldExpiresAt, FieldReason, FieldOwner} {
		if !fields[want] {
			t.Fatalf("O08 did not name %s: %v", want, fields)
		}
	}
}

func TestValidateOpsRejectsTTLOverThirtyDays(t *testing.T) {
	t.Parallel()
	over := testNow.Add(MaxOpsOverrideTTL + time.Hour).Format(time.RFC3339)
	fs := ValidateOps(mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false,
      "expires_at":"`+over+`","reason":"r","owner":"o"}]}`), testNow)
	assertHas(t, fs, "O09")
	if !fs.HasRejections() {
		t.Fatal("an over-cap TTL must be a rejection")
	}

	// Just inside the cap: legal, but past 72h it is config rather than an
	// incident tool, so it warns.
	inside := testNow.Add(MaxOpsOverrideTTL - time.Hour).Format(time.RFC3339)
	fs = ValidateOps(mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false,
      "expires_at":"`+inside+`","reason":"r","owner":"o"}]}`), testNow)
	assertNot(t, fs, "O09")
	assertHas(t, fs, "O10")
	if fs.HasRejections() {
		t.Fatalf("an in-cap TTL must not be a rejection:\n%s", fs.Error())
	}

	// Short TTL: entirely clean.
	short := testNow.Add(time.Hour).Format(time.RFC3339)
	fs = ValidateOps(mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false,
      "expires_at":"`+short+`","reason":"r","owner":"o"}]}`), testNow)
	if len(fs) != 0 {
		t.Fatalf("clean override produced findings:\n%s", fs.Error())
	}
}

func TestValidateOpsEnforcesWhitelist(t *testing.T) {
	t.Parallel()
	// An unbounded emergency layer is a second config system with none of the
	// review of the first one.
	fs := ValidateOps(mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false,
      "default_value":true,"rules":[],"bucket_namespace":"x",
      "expires_at":"2026-01-01T01:00:00Z","reason":"r","owner":"o"}]}`), testNow)
	assertHas(t, fs, "O07")
	named := map[string]bool{}
	for _, f := range fs {
		if f.RuleID == "O07" {
			named[f.Field] = true
		}
	}
	for _, want := range []string{"default_value", "rules", "bucket_namespace"} {
		if !named[want] {
			t.Fatalf("O07 did not name %q: %v", want, named)
		}
	}
	// enabled and basis_points ARE the whitelist.
	fs = ValidateOps(mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false,"basis_points":0,
      "expires_at":"2026-01-01T01:00:00Z","reason":"r","owner":"o"}]}`), testNow)
	assertNot(t, fs, "O07")
}

func TestValidateOpsExpiredOverrideWarnsAndIsDropped(t *testing.T) {
	t.Parallel()
	past := testNow.Add(-time.Hour).Format(time.RFC3339)
	l := mustOps(t, `{"environment":"prod","overrides":[{"key":"a.b","enabled":false,
      "expires_at":"`+past+`","reason":"r","owner":"o"}]}`)
	fs := ValidateOps(l, testNow)
	assertHas(t, fs, "M11")
	if fs.HasRejections() {
		t.Fatal("an expired override self-heals; it is not a rejection")
	}
	if !opsExpired(&l.Overrides[0], testNow) {
		t.Fatal("expired override must be dropped from the merge")
	}
}

// -----------------------------------------------------------------------------
// Pass 2 -- post-merge
// -----------------------------------------------------------------------------

func postMerge(t *testing.T, baseDoc, overlayDoc, opsDoc string) Findings {
	t.Helper()
	base := mustBase(t, baseDoc)
	bf := &base.Flags[0]
	var ov *OverlayFlag
	if overlayDoc != "" {
		l := mustOverlay(t, overlayDoc)
		for i := range l.Flags {
			if l.Flags[i].Key == bf.Key {
				ov = &l.Flags[i]
			}
		}
	}
	var ops *OpsOverride
	if opsDoc != "" {
		l := mustOps(t, opsDoc)
		for i := range l.Overrides {
			if l.Overrides[i].Key == bf.Key {
				ops = &l.Overrides[i]
			}
		}
	}
	f, prov := mergeFlag(bf, ov, ops)
	return validateResolved("prod", f, bf, ov, ops, prov)
}

func TestPostMergeCanonicalIsClean(t *testing.T) {
	t.Parallel()
	if fs := postMerge(t, canonicalBase, "", ""); len(fs) != 0 {
		t.Fatalf("canonical resolved flag produced findings:\n%s", fs.Error())
	}
}

func TestPostMergeOverlayValueTypeMismatch(t *testing.T) {
	t.Parallel()
	// M01: the overlay carries no type, so only the merge can catch this. It is
	// exactly the class of error that makes eager resolution mandatory -- catching
	// it lazily would mean catching it inside an evaluation.
	fs := postMerge(t, canonicalBase, `{"environment":"prod","flags":[{"key":"`+flagKey+`","default_value":"yes","off_value":7}]}`, "")
	assertHas(t, fs, "M01")
	n := 0
	for _, f := range fs {
		if f.RuleID == "M01" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want M01 for both default_value and off_value, got %d:\n%s", n, fs.Error())
	}
	for _, f := range fs.ForFlag(flagKey) {
		if f.RuleID == "M01" && f.Layer != LayerOverlay {
			t.Fatalf("M01 must name the layer that supplied the bad value, got %s", f.Layer)
		}
	}
}

func TestPostMergeRuleValueTypeMismatch(t *testing.T) {
	t.Parallel()
	fs := postMerge(t, canonicalBase, `{"environment":"prod","flags":[{"key":"`+flagKey+`","rules_mode":"append",
      "rules":[{"id":"bad-type","conditions":[{"attribute":"c","op":"eq","values":["v"]}],"value":"a-string"}]}]}`, "")
	assertHas(t, fs, "M06")
}

func TestPostMergeAppendedRuleIDCollision(t *testing.T) {
	t.Parallel()
	// Needs both lists, so it is only decidable after the merge.
	fs := postMerge(t, canonicalBase, `{"environment":"prod","flags":[{"key":"`+flagKey+`","rules_mode":"append",
      "rules":[{"id":"internal-staff","conditions":[{"attribute":"c","op":"eq","values":["v"]}],"value":true}]}]}`, "")
	assertHas(t, fs, "M03", "M04")
}

func TestPostMergeRolloutWithoutOnOffValues(t *testing.T) {
	t.Parallel()
	fs := postMerge(t, `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":false,
      "rollout":{"basis_points":100,"bucket_by":"user_id"}}]}`, "", "")
	assertHas(t, fs, "M17")
	n := 0
	for _, f := range fs {
		if f.RuleID == "M17" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want M17 for both on_value and off_value, got %d:\n%s", n, fs.Error())
	}
}

func TestPostMergeRolloutValueTypeMismatch(t *testing.T) {
	t.Parallel()
	fs := postMerge(t, canonicalBase, `{"environment":"prod","flags":[{"key":"`+flagKey+`",
      "rollout":{"on_value":"a-string","off_value":3}}]}`, "")
	assertHas(t, fs, "M18")
}

func TestPostMergeResolvedBasisPointsOutOfRange(t *testing.T) {
	t.Parallel()
	base := mustBase(t, canonicalBase)
	f, prov := mergeFlag(&base.Flags[0], nil, nil)
	f.Rollout.BasisPoints = 20000 // as if a future layer widened the range
	fs := validateResolved("prod", f, &base.Flags[0], nil, nil, prov)
	assertHas(t, fs, "M05")
}

// O2: two orderings accept byte-identical config and mean different things, so a
// flag carrying both rules and a rollout is REJECTED rather than defaulted.
func TestPostMergeRequiresExplicitEvaluationOrder(t *testing.T) {
	t.Parallel()
	noOrder := `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":false,
      "rules":[{"id":"r","conditions":[{"attribute":"c","op":"eq","values":["v"]}],"value":true}],
      "rollout":{"basis_points":100,"bucket_by":"user_id","on_value":true,"off_value":false}}]}`
	fs := postMerge(t, noOrder, "", "")
	assertHas(t, fs, "M09")
	if fs.MaxSeverity() != SeverityRejectFlag {
		t.Fatalf("M09 must reject, got %s", fs.MaxSeverity())
	}

	// Rules alone, or a rollout alone, need no declared order.
	rulesOnly := `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":false,
      "rules":[{"id":"r","conditions":[{"attribute":"c","op":"eq","values":["v"]}],"value":true}]}]}`
	assertNot(t, postMerge(t, rulesOnly, "", ""), "M09")

	rolloutOnly := `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":false,
      "rollout":{"basis_points":100,"bucket_by":"user_id","on_value":true,"off_value":false}}]}`
	assertNot(t, postMerge(t, rolloutOnly, "", ""), "M09")

	// And a combination an OVERLAY creates is caught just the same.
	overlayCreates := `{"environment":"prod","flags":[{"key":"a.b","rules_mode":"replace",
      "rules":[{"id":"r","conditions":[{"attribute":"c","op":"eq","values":["v"]}],"value":true}]}]}`
	assertHas(t, postMerge(t, rolloutOnly, overlayCreates, ""), "M09")
}

func TestPostMergeOpsBasisPointsWithoutRollout(t *testing.T) {
	t.Parallel()
	// An ops override that silently cannot take effect is worse than one that is
	// rejected: on-call believes the rollout was throttled and it was not.
	noRollout := `{"flags":[{"key":"a.b","type":"bool","owner":"o","enabled":true,"default_value":false}]}`
	fs := postMerge(t, noRollout, "", `{"environment":"prod","overrides":[{"key":"a.b","basis_points":0,
      "expires_at":"2026-01-01T01:00:00Z","reason":"r","owner":"o"}]}`)
	assertHas(t, fs, "M19")
}

func TestFindingsErrorListsEveryViolation(t *testing.T) {
	t.Parallel()
	fs := ValidateBase(mustBase(t, `{"flags":[{"key":"BAD","type":"nope","enabled":true,"default_value":1}]}`))
	msg := fs.Error()
	for _, id := range fs.RuleIDs() {
		if !strings.Contains(msg, id) {
			t.Fatalf("Error() omitted %s:\n%s", id, msg)
		}
	}
	if strings.Count(msg, "\n") < len(fs) {
		t.Fatalf("Error() did not render one line per finding:\n%s", msg)
	}
	if fs.Err() == nil {
		t.Fatal("Err() must be non-nil when there are rejections")
	}
	if (Findings{{RuleID: "X", Severity: SeverityWarn}}).Err() != nil {
		t.Fatal("Err() must be nil when there are only warnings")
	}
	if Findings(nil).Err() != nil {
		t.Fatal("Err() on no findings must be a true nil error")
	}
}

func TestQuarantineBudget(t *testing.T) {
	t.Parallel()
	// The floor dominates for small environments; the fraction for large ones.
	for _, tc := range []struct{ flags, want int }{
		{0, 20}, {1, 20}, {100, 20}, {400, 20}, {401, 20}, {5000, 250}, {10000, 500},
	} {
		if got := QuarantineBudget(tc.flags); got != tc.want {
			t.Fatalf("QuarantineBudget(%d) = %d, want %d", tc.flags, got, tc.want)
		}
	}
}

func TestSeverityOrdering(t *testing.T) {
	t.Parallel()
	if !(SeverityWarn < SeverityRejectFlag && SeverityRejectFlag < SeverityRejectEnv && SeverityRejectEnv < SeverityRejectGlobal) {
		t.Fatal("severity must order by blast radius")
	}
	if SeverityWarn.IsRejection() {
		t.Fatal("a warning is not a rejection")
	}
	if core.BucketSpace != 10000 {
		t.Fatalf("basis-point range assumes a bucket space of 10000, core says %d", core.BucketSpace)
	}
}

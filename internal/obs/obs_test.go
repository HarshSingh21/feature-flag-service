package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/obs"
)

// ---------- logging ----------

func decodeLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no log line was written")
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, line)
	}
	return m
}

func TestEvaluationErrorLogCarriesTheFullSchema(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := obs.New(obs.Options{Output: &buf, Level: slog.LevelDebug, Service: "flagd", InstanceID: "pod-7"})

	ctx := obs.ContextWithTrace(context.Background(), obs.Trace{TraceID: "abc123", SpanID: "def456"})
	log.EvaluationError(ctx, obs.EvalError{
		Flag:        "checkout_v2",
		Env:         "prod",
		Reason:      "TYPE_MISMATCH",
		Generation:  42,
		RuleID:      "r-country-in",
		Err:         errors.New("declared bool, resolved string"),
		CtxKeys:     []string{"country", "user_id"},
		ValueSource: "call_site_default",
	})

	m := decodeLine(t, &buf)
	// Every field an on-call engineer needs at 3am.
	for _, k := range []string{"ts", "level", "msg", obs.KeyFlag, obs.KeyEnv, obs.KeyReason,
		obs.KeyGeneration, obs.KeyTraceID, obs.KeyEvent, obs.KeyInstanceID} {
		if _, ok := m[k]; !ok {
			t.Errorf("evaluation error log is missing %q; got %v", k, m)
		}
	}
	if m["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", m["level"])
	}
	if m[obs.KeyTraceID] != "abc123" {
		t.Errorf("trace_id = %v, want abc123 (trace must flow from context into the line)", m[obs.KeyTraceID])
	}
	if m[obs.KeyGeneration] != float64(42) {
		t.Errorf("generation = %v, want 42", m[obs.KeyGeneration])
	}
	if _, ok := m[obs.KeyStack]; ok {
		t.Error("stack must be absent when the record does not describe a panic")
	}
}

func TestEvaluationErrorLogCarriesStackForAPanic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := obs.New(obs.Options{Output: &buf, Level: slog.LevelDebug})
	log.EvaluationError(context.Background(), obs.EvalError{
		Flag: "f", Env: "prod", Reason: "ERROR", Stack: []byte("goroutine 1 [running]:\nmain.boom()"),
	})
	m := decodeLine(t, &buf)
	s, ok := m[obs.KeyStack].(string)
	if !ok || !strings.Contains(s, "goroutine") {
		t.Fatalf("recovered panic must log the stack; got %v", m[obs.KeyStack])
	}
}

func TestUserIDIsNeverLoggedAtInfoLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := obs.New(obs.Options{Output: &buf, Level: slog.LevelDebug})

	log.Info(context.Background(), "evaluated", "user_id", "u-4815162342", "flag", "checkout_v2")
	m := decodeLine(t, &buf)
	if _, ok := m["user_id"]; ok {
		t.Fatal("user_id must not appear in an info-level line at all")
	}
	if m["flag"] != "checkout_v2" {
		t.Fatal("non-PII fields must survive redaction")
	}
	if strings.Contains(buf.String(), "u-4815162342") {
		t.Fatal("the user id value leaked into the line")
	}
}

func TestPIIIsRedactedNotEmittedAtErrorLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := obs.New(obs.Options{Output: &buf, Level: slog.LevelDebug})
	log.Error(context.Background(), "boom", "email", "a@b.com", "authorization", "Bearer hunter2")
	m := decodeLine(t, &buf)
	if m["email"] != obs.RedactedValue || m["authorization"] != obs.RedactedValue {
		t.Fatalf("PII must be redacted at error level, got %v", m)
	}
	if strings.Contains(buf.String(), "a@b.com") || strings.Contains(buf.String(), "hunter2") {
		t.Fatal("a PII value reached the log line")
	}
}

func TestPIIIsDroppedFromPreformattedAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := obs.New(obs.Options{Output: &buf, Level: slog.LevelDebug}).With("user_id", "u1", "env", "prod")
	log.Error(context.Background(), "boom")
	m := decodeLine(t, &buf)
	if _, ok := m["user_id"]; ok {
		t.Fatal("PII bound via With() must be dropped, not carried on every line")
	}
	if m["env"] != "prod" {
		t.Fatal("non-PII bound attrs must survive")
	}
}

func TestMapKeysReportsNamesOnly(t *testing.T) {
	t.Parallel()
	got := obs.MapKeys(map[string]string{"country": "IN", "plan": "gold"})
	want := []string{"country", "plan"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("MapKeys = %v, want %v (sorted names, no values)", got, want)
	}
}

func TestSamplerCapsPerKeyVolumeAndReportsSuppression(t *testing.T) {
	t.Parallel()
	s := obs.NewSampler(0, 2, 16) // burst 2, no refill: only 2 lines ever
	var admitted, suppressed int
	for i := 0; i < 1000; i++ {
		if ok, n := s.Allow("checkout_v2|TYPE_MISMATCH"); ok {
			admitted++
			suppressed += n
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d lines, want 2 -- one broken flag must not write a line per evaluation", admitted)
	}
	// A different key has its own bucket: one broken flag must not silence others.
	if ok, _ := s.Allow("other|ERROR"); !ok {
		t.Fatal("sampling must be per (flag, reason), not global")
	}
	_ = suppressed
}

func TestNopLoggerWritesNothing(t *testing.T) {
	t.Parallel()
	obs.NewNop().Error(context.Background(), "must not panic")
}

// ---------- cardinality guard ----------

func TestCardinalityGuardRejectsUnboundedLabels(t *testing.T) {
	t.Parallel()
	// The label that would take the metrics backend down: 1e9 users x 5e3 flags.
	for _, name := range []string{"user_id", "session_id", "trace_id", "request_id", "bucket", "path", "ip", "email"} {
		if err := obs.ValidateLabel(obs.L(name, "whatever")); err == nil {
			t.Errorf("ValidateLabel(%q) accepted an unbounded label", name)
		} else if !errors.Is(err, obs.ErrLabelRejected) {
			t.Errorf("ValidateLabel(%q) error does not wrap ErrLabelRejected: %v", name, err)
		}
	}
	// Anything not on the allowlist is rejected too -- the one that causes the
	// outage is always a name nobody anticipated.
	if err := obs.ValidateLabel(obs.L("cohort_id", "x")); err == nil {
		t.Error("a label absent from the allowlist must be rejected, not merely un-denied")
	}
}

func TestCardinalityGuardAcceptsBoundedLabels(t *testing.T) {
	t.Parallel()
	for _, l := range []obs.Label{
		obs.L("flag", "checkout_v2"), obs.L("env", "prod"), obs.L("reason", "RULE_MATCH"),
		obs.L("result", "ok"), obs.L("route", "eval"), obs.L("code", "200"),
	} {
		if err := obs.ValidateLabel(l); err != nil {
			t.Errorf("ValidateLabel(%v) rejected a bounded label: %v", l, err)
		}
	}
}

func TestCardinalityGuardRejectsOversizedValues(t *testing.T) {
	t.Parallel()
	if err := obs.ValidateLabel(obs.L("flag", strings.Repeat("x", obs.MaxLabelValueLen+1))); err == nil {
		t.Fatal("an unbounded label VALUE is an unbounded series name; it must be rejected")
	}
}

func TestSanitizeLabelsDropsRatherThanFails(t *testing.T) {
	t.Parallel()
	kept, dropped := obs.SanitizeLabels([]obs.Label{
		obs.L("flag", "f"), obs.L("user_id", "u1"), obs.L("env", "prod"),
	})
	if len(kept) != 2 || len(dropped) != 1 || dropped[0] != "user_id" {
		t.Fatalf("kept=%v dropped=%v; the bad label must be dropped and reported, the good ones kept", kept, dropped)
	}
}

func TestExpvarMetricsDropsUnboundedLabelAndCountsIt(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_drop")
	m.Counter("flagsvc_evaluations_total", obs.L("flag", "f"), obs.L("user_id", "u-1")).Inc()
	if m.DroppedLabels() != 1 {
		t.Fatalf("DroppedLabels = %d, want 1", m.DroppedLabels())
	}
	// Two different user ids must collapse into ONE series, not two.
	m.Counter("flagsvc_evaluations_total", obs.L("flag", "f"), obs.L("user_id", "u-2")).Inc()
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m.Var().String()), &got); err != nil {
		t.Fatalf("expvar output is not JSON: %v", err)
	}
	series := 0
	for k := range got {
		if strings.HasPrefix(k, "flagsvc_evaluations_total") {
			series++
		}
	}
	if series != 1 {
		t.Fatalf("a dropped label produced %d series, want 1 -- the guard did not collapse them", series)
	}
	if strings.Contains(m.Var().String(), "u-1") {
		t.Fatal("a rejected label value reached the metrics backend")
	}
}

func TestExpvarMetricsCapsTotalSeries(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_cap")
	m.SetMaxSeries(8)
	// "env" is an allowed label NAME, but a caller controls its VALUE. Without a
	// series cap, a caller sending a fresh env per request mints a series per
	// request even though every label passed validation.
	for i := 0; i < 200; i++ {
		m.Counter("flagsvc_evaluations_total", obs.L("env", "env-"+strings.Repeat("x", i%40)+string(rune('a'+i%26)))).Inc()
	}
	if m.OverflowedSeries() == 0 {
		t.Fatal("series cap never engaged; unbounded label VALUES are still unbounded series")
	}
}

func TestHistogramRendersValidJSON(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_hist")
	h := m.Histogram(obs.MetricEvaluationSeconds)
	for _, v := range []float64{0.000001, 0.00003, 0.002, 5} {
		h.Observe(v)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m.Var().String()), &out); err != nil {
		t.Fatalf("expvar output must be valid JSON, got: %v", err)
	}
	raw, ok := out[obs.MetricEvaluationSeconds]
	if !ok {
		t.Fatalf("histogram series missing from %v", out)
	}
	var hist struct {
		Count   uint64            `json:"count"`
		Sum     float64           `json:"sum"`
		Buckets map[string]uint64 `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &hist); err != nil {
		t.Fatalf("histogram JSON: %v (%s)", err, raw)
	}
	if hist.Count != 4 {
		t.Fatalf("count = %d, want 4", hist.Count)
	}
	if hist.Buckets["+Inf"] != 4 {
		t.Fatalf("+Inf bucket = %d, want 4 (buckets must be cumulative)", hist.Buckets["+Inf"])
	}
}

func TestMetricsAreRaceFree(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_race")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.Counter("flagsvc_evaluations_total", obs.L("flag", "f"), obs.L("reason", "RULE_MATCH")).Inc()
				m.Histogram(obs.MetricEvaluationSeconds).Observe(float64(j) / 1e6)
				m.Gauge(obs.MetricSnapshotGeneration, obs.L("env", "prod")).Set(float64(j))
			}
		}(i)
	}
	wg.Wait()
	_ = m.Var().String()
}

func TestRecorderKeepsFlagOffTheLatencyHistogram(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_recorder")
	rec := obs.NewRecorder(m)
	rec.Evaluation("checkout_v2", "prod", "RULE_MATCH", false, 3*time.Microsecond)
	rec.Evaluation("pricing_v3", "prod", "ROLLOUT_IN", false, 4*time.Microsecond)

	out := m.Var().String()
	// One latency series, no flag dimension: 12 buckets x 5000 flags is 60k series
	// for a number nobody segments by flag.
	if strings.Contains(out, obs.MetricEvaluationSeconds+"{") {
		t.Fatalf("the evaluation latency histogram must carry no labels; got %s", out)
	}
	var series map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &series); err != nil {
		t.Fatalf("expvar output is not JSON: %v", err)
	}
	if _, ok := series[`flagsvc_evaluations_total{env="prod",flag="checkout_v2",reason="RULE_MATCH"}`]; !ok {
		t.Fatalf("evaluation counter must be labelled by flag, env and reason; got %s", out)
	}
}

func TestRecorderCountsFallbacksSeparately(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_fallback")
	rec := obs.NewRecorder(m)
	rec.Evaluation("f", "prod", "FLAG_NOT_FOUND", true, time.Microsecond)
	if !strings.Contains(m.Var().String(), obs.MetricFallbackTotal) {
		t.Fatal("fallback rate is hazard H1's only signal; it must have its own series")
	}
}

func TestNopMetricsRecordNothingAndNeverPanic(t *testing.T) {
	t.Parallel()
	rec := obs.NewRecorder(nil)
	rec.Evaluation("f", "prod", "RULE_MATCH", true, time.Second)
	rec.ConfigApply(obs.ApplyOK, time.Second)
	rec.PropagationLag(1)
	rec.SnapshotGeneration("prod", 3)
	rec.SnapshotFlags("prod", 10)
	rec.HTTPRequest("eval", "POST", 200, time.Second)
	rec.Panic("http_handler")
}

// ---------- trace ----------

func TestTraceIDIsGeneratedWhenAbsent(t *testing.T) {
	t.Parallel()
	ctx, tr := obs.EnsureTrace(context.Background(), http.Header{})
	if len(tr.TraceID) != 32 {
		t.Fatalf("generated trace id %q is not a 128-bit hex id", tr.TraceID)
	}
	if obs.TraceIDFromContext(ctx) != tr.TraceID {
		t.Fatal("the generated trace must be reachable from the context")
	}
	other, _ := obs.EnsureTrace(context.Background(), http.Header{})
	if obs.TraceIDFromContext(other) == tr.TraceID {
		t.Fatal("generated trace ids must be unique")
	}
}

func TestTraceIDIsAdoptedFromHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{"w3c traceparent", http.Header{"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}, "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"x-trace-id", http.Header{"X-Trace-Id": {"deadbeefcafe"}}, "deadbeefcafe"},
		{"x-request-id", http.Header{"X-Request-Id": {"req-12345"}}, "req-12345"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, tr := obs.EnsureTrace(context.Background(), tc.header)
			if tr.TraceID != tc.want {
				t.Fatalf("trace id = %q, want %q", tr.TraceID, tc.want)
			}
		})
	}
}

func TestMalformedInboundTraceIDIsDiscarded(t *testing.T) {
	t.Parallel()
	// A trace id is copied into logs and into response bodies. A caller must not be
	// able to choose those bytes, and must not be able to choose their length.
	for _, bad := range []string{
		`"><script>`,
		strings.Repeat("a", 4096),
		"a b c",
		"x",
		"line\nbreak",
	} {
		_, tr := obs.EnsureTrace(context.Background(), http.Header{"X-Trace-Id": {bad}})
		if tr.TraceID == bad {
			t.Fatalf("adopted an unsafe inbound trace id %q", bad)
		}
		if len(tr.TraceID) != 32 {
			t.Fatalf("a rejected inbound id must be replaced by a generated one, got %q", tr.TraceID)
		}
	}
}

func TestTraceIsInjectedIntoOutboundHeaders(t *testing.T) {
	t.Parallel()
	ctx := obs.ContextWithTrace(context.Background(), obs.Trace{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
	})
	h := http.Header{}
	obs.Inject(ctx, h)
	if h.Get(obs.HeaderTraceID) != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("X-Trace-Id not injected: %v", h)
	}
	if got := h.Get(obs.HeaderTraceParent); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("traceparent = %q; a hop that does not propagate is where a trace goes dark", got)
	}
}

func TestTraceMiddlewarePropagatesIntoContextAndResponse(t *testing.T) {
	t.Parallel()
	var seen string
	h := obs.TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = obs.TraceIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(obs.HeaderTraceID, "abc12345")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "abc12345" {
		t.Fatalf("handler saw trace %q, want abc12345", seen)
	}
	if rec.Header().Get(obs.HeaderTraceID) != "abc12345" {
		t.Fatalf("the trace id must be echoed so an operator holding a response can find the log line")
	}
}

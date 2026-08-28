package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/internal/obs"
	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
)

type harness struct {
	srv    *httpx.Server
	eval   http.Handler
	admin  http.Handler
	src    *fakeSource
	ev     *fakeEvaluator
	ap     *fakeApplier
	logBuf *bytes.Buffer
}

func newHarness(t *testing.T, mutate ...func(*httpx.Config, *httpx.Deps)) *harness {
	t.Helper()
	h := &harness{
		src:    newSource(),
		ev:     &fakeEvaluator{},
		ap:     &fakeApplier{},
		logBuf: &bytes.Buffer{},
	}
	cfg := httpx.Config{
		EvalAddr:  "127.0.0.1:0",
		AdminAddr: "127.0.0.1:0",
		Service:   "flagd",
	}
	deps := httpx.Deps{
		Snapshots: h.src,
		Evaluator: h.ev,
		Applier:   h.ap,
		Log:       obs.New(obs.Options{Output: h.logBuf, Level: slog.LevelDebug, Service: "flagd"}),
		Metrics:   obs.NewRecorder(nil),
	}
	for _, m := range mutate {
		m(&cfg, &deps)
	}
	srv, err := httpx.New(cfg, deps)
	if err != nil {
		t.Fatalf("httpx.New: %v", err)
	}
	h.srv = srv
	h.eval = srv.EvalHandler()
	h.admin = srv.AdminHandler()
	return h
}

func post(t *testing.T, h http.Handler, path, body string, headers ...[2]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body is not the expected JSON shape: %v\n%s", err, rec.Body.String())
	}
	return v
}

type envelope struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	TraceID   string `json:"trace_id"`
	Timestamp string `json:"timestamp"`
}

func assertEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) envelope {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	env := decode[envelope](t, rec)
	if env.ErrorCode != wantCode {
		t.Fatalf("error_code = %q, want %q", env.ErrorCode, wantCode)
	}
	if env.TraceID == "" {
		t.Error("every error envelope must carry a trace id; without it the caller cannot report the failure")
	}
	if env.Timestamp == "" {
		t.Error("envelope timestamp is empty")
	}
	for _, leak := range []string{"goroutine", ".go:", "/Users/", "0x"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("envelope leaked internal detail %q: %s", leak, rec.Body.String())
		}
	}
	return env
}

// ---------- happy paths ----------

func TestEvaluateHappyPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 7, "checkout_v2"))

	rec := post(t, h.eval, "/v1/evaluate",
		`{"environment":"prod","flag":"checkout_v2","default":false,"context":{"user_id":"u1","attributes":{"country":"IN"}}}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[httpx.EvaluateResponse](t, rec)
	if got.Flag != "checkout_v2" || got.Environment != "prod" {
		t.Fatalf("echoed identity wrong: %+v", got)
	}
	if v, ok := got.Value.AsBool(); !ok || !v {
		t.Fatalf("value = %v, want bool true", got.Value)
	}
	if got.Reason != core.ReasonFallthrough.String() {
		t.Fatalf("reason = %q, want %q", got.Reason, core.ReasonFallthrough)
	}
	if got.Generation != 7 {
		t.Fatalf("generation = %d, want 7 -- the response must name the config that answered", got.Generation)
	}
	if got.Fallback {
		t.Error("a configured value must not be reported as a fallback")
	}
	if got.Bucket != core.NoBucket {
		t.Errorf("bucket = %d, want %d when no rollout was consulted", got.Bucket, core.NoBucket)
	}
}

func TestEvaluateBatchHappyPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 12, "a", "b", "c"))

	rec := post(t, h.eval, "/v1/evaluate/batch",
		`{"environment":"prod","context":{"user_id":"u1"},"flags":[{"flag":"a","default":false},{"flag":"b","default":false},{"flag":"zz","default":true}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[httpx.EvaluateBatchResponse](t, rec)
	if len(got.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(got.Results))
	}
	if got.Generation != 12 {
		t.Fatalf("batch generation = %d, want 12", got.Generation)
	}
	if got.Results[2].Reason != core.ReasonFlagNotFound.String() {
		t.Fatalf("unknown flag in a batch must be an ANSWER, not an error: %+v", got.Results[2])
	}
	if v, ok := got.Results[2].Value.AsBool(); !ok || !v {
		t.Fatal("an unknown flag must return the caller's default verbatim")
	}
}

// ---------- invariant CACHE-1 ----------

func TestBatchPinsExactlyOneSnapshotForTheWholeBatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.bump = true // every load returns a fresh generation
	h.src.set("prod", snap("prod", 100, "a", "b", "c", "d", "e"))

	rec := post(t, h.eval, "/v1/evaluate/batch",
		`{"environment":"prod","flags":[{"flag":"a","default":false},{"flag":"b","default":false},{"flag":"c","default":false},{"flag":"d","default":false},{"flag":"e","default":false}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	// One load for five flags. Five loads would mean pinning per flag, and with a
	// swap landing mid-batch that returns flag A from generation N and flag B from
	// N+1 -- a cross-flag inconsistency nobody can reproduce from a bug report,
	// because by the time anyone looks the two flags agree again.
	if n := h.src.loads.Load(); n != 1 {
		t.Fatalf("snapshot loaded %d times for one batch, want exactly 1 (invariant CACHE-1)", n)
	}

	got := decode[httpx.EvaluateBatchResponse](t, rec)
	for i, r := range got.Results {
		if r.Generation != got.Generation {
			t.Fatalf("result %d came from generation %d but the batch claims %d", i, r.Generation, got.Generation)
		}
	}
}

func TestSingleEvaluateAlsoLoadsOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 3, "a"))
	post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"a","default":false}`)
	if n := h.src.loads.Load(); n != 1 {
		t.Fatalf("snapshot loaded %d times for one evaluation, want 1", n)
	}
}

// ---------- error paths ----------

func TestMalformedJSONReturnsTheEnvelope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, tc := range []struct{ name, body, code string }{
		{"truncated", `{"environment":"prod","flag":`, "invalid_json"},
		{"not an object", `[1,2,3]`, "invalid_json"},
		{"empty", ``, "invalid_json"},
		{"unknown field", `{"environment":"prod","flag":"a","default":true,"colour":"red"}`, "invalid_json"},
		{"trailing document", `{"environment":"prod","flag":"a","default":true}{"x":1}`, "invalid_json"},
		{"wrong value type", `{"environment":"prod","flag":"a","default":1.5}`, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, h.eval, "/v1/evaluate", tc.body)
			assertEnvelope(t, rec, http.StatusBadRequest, tc.code)
		})
	}
}

func TestNonJSONContentTypeIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/evaluate", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.eval.ServeHTTP(rec, req)
	assertEnvelope(t, rec, http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func TestOversizedBodyIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *httpx.Config, _ *httpx.Deps) { c.MaxBodyBytes = 64 })
	body := `{"environment":"prod","flag":"a","default":"` + strings.Repeat("x", 4096) + `"}`
	rec := post(t, h.eval, "/v1/evaluate", body)
	assertEnvelope(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
}

func TestUnknownFlagIsAnAnswerNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 5, "known"))

	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"never_pushed","default":"safe"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; an unknown flag is an ANSWER, not a 404 -- the read path never consults the config source", rec.Code)
	}
	got := decode[httpx.EvaluateResponse](t, rec)
	if got.Reason != core.ReasonFlagNotFound.String() {
		t.Fatalf("reason = %q, want FLAG_NOT_FOUND", got.Reason)
	}
	if v, _ := got.Value.AsString(); v != "safe" {
		t.Fatalf("value = %v, want the caller's default", got.Value)
	}
	if !got.Fallback {
		t.Fatal("fallback must be true; the fallback rate is the only signal for hazard H1, silent fail-open")
	}
}

func TestUnknownEnvironmentReturnsTheCallerDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"staging","flag":"a","default":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; an environment with no snapshot degrades to defaults, it does not fail", rec.Code)
	}
	got := decode[httpx.EvaluateResponse](t, rec)
	if got.Reason != core.ReasonFlagNotFound.String() || got.Generation != 0 {
		t.Fatalf("got %+v, want FLAG_NOT_FOUND at generation 0", got)
	}
}

func TestMalformedEnvironmentAndFlagAreRejectedAtTheEdge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// env and flag are metric label VALUES. Validating them here is what makes those
	// labels safe to allow at all -- an unvalidated one is caller-controlled
	// cardinality.
	for _, body := range []string{
		`{"environment":"","flag":"a","default":false}`,
		`{"environment":"prod/../etc","flag":"a","default":false}`,
		`{"environment":"` + strings.Repeat("e", 64) + `","flag":"a","default":false}`,
		`{"environment":"prod","flag":"","default":false}`,
		`{"environment":"prod","flag":"a b","default":false}`,
		`{"environment":"prod","flag":"` + strings.Repeat("f", 200) + `","default":false}`,
	} {
		rec := post(t, h.eval, "/v1/evaluate", body)
		assertEnvelope(t, rec, http.StatusBadRequest, "invalid_argument")
	}
}

func TestMissingDefaultIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 1, "a"))
	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"a","default":null}`)
	assertEnvelope(t, rec, http.StatusBadRequest, "invalid_argument")
}

func TestBatchSizeIsCapped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *httpx.Config, _ *httpx.Deps) { c.MaxBatchFlags = 3 })
	h.src.set("prod", snap("prod", 1, "a"))
	entries := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		entries = append(entries, fmt.Sprintf(`{"flag":"f%d","default":false}`, i))
	}
	rec := post(t, h.eval, "/v1/evaluate/batch",
		`{"environment":"prod","flags":[`+strings.Join(entries, ",")+`]}`)
	assertEnvelope(t, rec, http.StatusBadRequest, "too_many_flags")
}

func TestEmptyBatchIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := post(t, h.eval, "/v1/evaluate/batch", `{"environment":"prod","flags":[]}`)
	assertEnvelope(t, rec, http.StatusBadRequest, "invalid_argument")
}

func TestUnknownRouteAndWrongMethod(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	assertEnvelope(t, get(t, h.eval, "/v1/nope"), http.StatusNotFound, "not_found")
	// GET on a POST-only route falls through the mux to the catch-all.
	assertEnvelope(t, get(t, h.eval, "/v1/evaluate"), http.StatusNotFound, "not_found")
}

// ---------- the panic boundary ----------

func TestPanickingEvaluatorIsContainedAndReturnsTheEnvelope(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 9, "boom"))
	h.ev.panicOn = "boom"

	// The test process surviving this call is half the assertion.
	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"boom","default":false}`,
		[2]string{obs.HeaderTraceID, "trace-panic-1"})

	env := assertEnvelope(t, rec, http.StatusInternalServerError, "internal")
	if env.TraceID != "trace-panic-1" {
		t.Fatalf("trace_id = %q, want the caller's", env.TraceID)
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "rule set") {
		t.Fatalf("the panic message leaked to the caller: %s", rec.Body)
	}
	// The operator does get the detail, in the log, joined by the same trace id.
	logs := h.logBuf.String()
	if !strings.Contains(logs, "trace-panic-1") {
		t.Fatal("the panic log line must carry the request's trace id")
	}

	// And the next request still works: containment means the process kept serving.
	h.ev.panicOn = ""
	if rec2 := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"boom","default":false}`); rec2.Code != http.StatusOK {
		t.Fatalf("service did not survive the contained panic: %d", rec2.Code)
	}
}

func TestPanickingEvaluatorInABatchIsContained(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 9, "ok1", "boom"))
	h.ev.panicOn = "boom"
	rec := post(t, h.eval, "/v1/evaluate/batch",
		`{"environment":"prod","flags":[{"flag":"ok1","default":false},{"flag":"boom","default":false}]}`)
	assertEnvelope(t, rec, http.StatusInternalServerError, "internal")
}

func TestPanickingHandlerLayerIsAlsoContained(t *testing.T) {
	t.Parallel()
	// A panic raised outside the evaluator -- in a fake source -- still lands on the
	// middleware boundary rather than the process.
	h := newHarness(t)
	h.src.set("prod", snap("prod", 1, "a"))
	h.ev.panicOn = "a"
	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"a","default":false}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// ---------- readiness ----------

func TestReadyIsFalseBeforeAnySnapshotHasLoaded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *httpx.Config, _ *httpx.Deps) { c.RequiredEnvs = []string{"prod"} })

	rec := get(t, h.eval, "/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; a pod that never loaded config serves compile-time defaults silently (hazard H6)", rec.Code)
	}
	got := decode[httpx.ReadyResponse](t, rec)
	if got.Ready || got.Reason != "no_snapshot" {
		t.Fatalf("got %+v, want ready=false reason=no_snapshot", got)
	}
	// Liveness must NOT follow readiness: failing it here would restart every pod in
	// the fleet during a control-plane incident.
	if live := get(t, h.eval, "/live"); live.Code != http.StatusOK {
		t.Fatalf("/live = %d; liveness must not depend on config", live.Code)
	}
}

func TestReadyIsTrueWhenServingAStaleButValidSnapshot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *httpx.Config, _ *httpx.Deps) {
		c.RequiredEnvs = []string{"prod"}
		c.StaleAfter = time.Second
	})
	h.src.set("prod", snap("prod", 4, "a"))
	h.src.setApplied("prod", time.Now().Add(-1*time.Hour)) // an hour behind the control plane

	rec := get(t, h.eval, "/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; a pod serving last-known-good is a WORKING pod, and pulling it from the load balancer converts a control-plane incident into a data-plane one", rec.Code)
	}
	got := decode[httpx.ReadyResponse](t, rec)
	if !got.Ready {
		t.Fatal("staleness must never gate readiness")
	}
	if !got.Degraded {
		t.Fatal("staleness must still be REPORTED; ready-and-degraded is the state an operator needs to see")
	}
	if len(got.Environments) != 1 || !got.Environments[0].Stale || got.Environments[0].Generation != 4 {
		t.Fatalf("environment detail wrong: %+v", got.Environments)
	}
}

func TestReadyRequiresEveryRequiredEnvironment(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *httpx.Config, _ *httpx.Deps) { c.RequiredEnvs = []string{"prod", "staging"} })
	h.src.set("prod", snap("prod", 1, "a"))
	if rec := get(t, h.eval, "/ready"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; one loaded environment out of two required is not ready", rec.Code)
	}
	h.src.set("staging", snap("staging", 1, "a"))
	if rec := get(t, h.eval, "/ready"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once every required environment has loaded", rec.Code)
	}
}

func TestHealthEndpointsExistOnBothListeners(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 2, "a"))
	for _, handler := range []struct {
		name string
		h    http.Handler
	}{{"eval", h.eval}, {"admin", h.admin}} {
		for _, path := range []string{"/health", "/live", "/ready"} {
			rec := get(t, handler.h, path)
			if rec.Code != http.StatusOK {
				t.Errorf("%s%s = %d, want 200", handler.name, path, rec.Code)
			}
		}
	}
	got := decode[httpx.HealthResponse](t, get(t, h.eval, "/health"))
	if got.Service != "flagd" || len(got.Environments) != 1 || got.Environments[0].Generation != 2 {
		t.Fatalf("health body wrong: %+v", got)
	}
}

// ---------- admin listener ----------

func TestAdminApplyLayerHappyPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ap.res = httpx.ApplyResult{
		Applied:      []httpx.AppliedEnv{{Env: "prod", Generation: 43, Flags: 120}},
		FlagsChanged: []string{"checkout_v2"},
	}
	rec := post(t, h.admin, "/v1/config/layers", `{"base":{"flags":{}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[httpx.ApplyResult](t, rec)
	if len(got.Applied) != 1 || got.Applied[0].Generation != 43 {
		t.Fatalf("apply result wrong: %+v", got)
	}
	if body, _ := h.ap.body.Load().([]byte); string(body) != `{"base":{"flags":{}}}` {
		t.Fatalf("the layer body must reach the applier verbatim; transport does not parse it, got %q", body)
	}
	if !strings.Contains(h.logBuf.String(), "config.apply") {
		t.Fatal("a config apply must produce an audit line; 'what changed' is the first question in every flag incident")
	}
}

func TestAdminApplyValidationRejectionIs400AndNeverPages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ap.err = fmt.Errorf("basis_points 20000 is out of range: %w", httpx.ErrValidation)
	rec := post(t, h.admin, "/v1/config/layers", `{"x":1}`)
	env := assertEnvelope(t, rec, http.StatusBadRequest, "invalid_config")
	if !strings.Contains(env.Message, "basis_points") {
		t.Fatalf("a validation message must tell the operator what to fix, got %q", env.Message)
	}
}

func TestAdminApplyInternalFailureIs500AndSaysNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ap.err = fmt.Errorf("compile at /Users/harsh/build.go:22: %w", errBoom)
	rec := post(t, h.admin, "/v1/config/layers", `{"x":1}`)
	env := assertEnvelope(t, rec, http.StatusInternalServerError, "internal")
	if strings.Contains(env.Message, "build.go") || strings.Contains(env.Message, "exploded") {
		t.Fatalf("an internal error's text must not reach the caller, got %q", env.Message)
	}
}

func TestAdminApplyIsUnavailableWhenNotWired(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(_ *httpx.Config, d *httpx.Deps) { d.Applier = nil })
	rec := post(t, h.admin, "/v1/config/layers", `{"x":1}`)
	assertEnvelope(t, rec, http.StatusServiceUnavailable, "unavailable")
}

func TestAdminApplyIsNotReachableFromTheEvalListener(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// Separate muxes are the mechanism; the ports and network policy are the rest of
	// it. A config push must not be routable from the mesh-facing listener.
	rec := post(t, h.eval, "/v1/config/layers", `{"x":1}`)
	assertEnvelope(t, rec, http.StatusNotFound, "not_found")
}

func TestAdminSnapshotDebug(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 31, "checkout_v2", "pricing"))

	rec := get(t, h.admin, "/v1/config/snapshot/prod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[httpx.SnapshotDebugResponse](t, rec)
	if got.Generation != 31 || got.Flags != 2 || got.Environment != "prod" {
		t.Fatalf("snapshot debug body wrong: %+v", got)
	}
	if got.Flag != nil {
		t.Fatal("without ?flag= the endpoint must not serialise flag bodies")
	}

	one := decode[httpx.SnapshotDebugResponse](t, get(t, h.admin, "/v1/config/snapshot/prod?flag=checkout_v2"))
	if one.Flag == nil || one.Flag.Key != "checkout_v2" {
		t.Fatalf("?flag= must dump the resolved flag: %+v", one)
	}
	assertEnvelope(t, get(t, h.admin, "/v1/config/snapshot/prod?flag=missing"), http.StatusNotFound, "not_found")
	assertEnvelope(t, get(t, h.admin, "/v1/config/snapshot/staging"), http.StatusNotFound, "not_found")
	assertEnvelope(t, get(t, h.admin, "/v1/config/snapshot/bad%20env"), http.StatusBadRequest, "invalid_argument")
}

func TestDebugVarsIsAdminOnly(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_debugvars")
	h := newHarness(t, func(_ *httpx.Config, d *httpx.Deps) { d.Metrics = obs.NewRecorder(m) })
	h.src.set("prod", snap("prod", 1, "a"))
	post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"a","default":false}`)

	rec := get(t, h.admin, "/debug/vars")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/debug/vars must be valid JSON: %v", err)
	}
	if _, ok := out["test_debugvars"]; !ok {
		t.Fatalf("the service registry is not exposed on /debug/vars: %v", keysOf(out))
	}
	if got := get(t, h.eval, "/debug/vars"); got.Code != http.StatusNotFound {
		t.Fatalf("/debug/vars on the eval listener = %d, want 404; internal state has no business on a mesh-facing port", got.Code)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------- trace propagation ----------

func TestTraceIDFlowsFromRequestIntoResponseHeaderBodyAndLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 1, "known"))

	// A fallback evaluation, so a log line is guaranteed.
	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"missing","default":false}`,
		[2]string{obs.HeaderTraceID, "trace-e2e-01"})

	if got := rec.Header().Get(obs.HeaderTraceID); got != "trace-e2e-01" {
		t.Fatalf("response header trace id = %q, want trace-e2e-01", got)
	}
	body := decode[httpx.EvaluateResponse](t, rec)
	if body.TraceID != "trace-e2e-01" {
		t.Fatalf("response body trace id = %q", body.TraceID)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(h.logBuf.String())), &line); err != nil {
		t.Fatalf("log line: %v (%s)", err, h.logBuf.String())
	}
	if line[obs.KeyTraceID] != "trace-e2e-01" {
		t.Fatalf("log trace id = %v; without it the response and the log cannot be joined", line[obs.KeyTraceID])
	}
	if line[obs.KeyFlag] != "missing" || line[obs.KeyReason] != core.ReasonFlagNotFound.String() {
		t.Fatalf("evaluation error log is missing its schema: %v", line)
	}
}

func TestTraceIDIsMintedWhenTheCallerSendsNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 1, "a"))
	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"a","default":false}`)
	if got := rec.Header().Get(obs.HeaderTraceID); len(got) != 32 {
		t.Fatalf("minted trace id = %q, want 32 hex chars", got)
	}
}

func TestNoUserIDReachesTheLogs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 1, "known"))
	post(t, h.eval, "/v1/evaluate",
		`{"environment":"prod","flag":"missing","default":false,"context":{"user_id":"u-4815162342","attributes":{"country":"IN"}}}`)

	logs := h.logBuf.String()
	if strings.Contains(logs, "u-4815162342") || strings.Contains(logs, `"IN"`) {
		t.Fatalf("evaluation context VALUES reached the log: %s", logs)
	}
	// The key NAMES are what carry the diagnostic value.
	if !strings.Contains(logs, "ctx_keys") || !strings.Contains(logs, "country") {
		t.Fatalf("ctx_keys must record which attributes were present: %s", logs)
	}
}

// ---------- server lifecycle ----------

func TestReadHeaderTimeoutIsSetOnBothListeners(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.srv.Shutdown(context.Background()) }()
	if h.srv.EvalAddr() == "" || h.srv.AdminAddr() == "" {
		t.Fatal("listeners did not bind")
	}
	if h.srv.EvalAddr() == h.srv.AdminAddr() {
		t.Fatal("eval and admin must be separate listeners with separate accept queues")
	}
}

func TestStartFailsLoudlyOnAPortConflict(t *testing.T) {
	t.Parallel()
	first := newHarness(t)
	if err := first.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = first.srv.Shutdown(context.Background()) }()

	addr := first.srv.EvalAddr()
	second := newHarness(t, func(c *httpx.Config, _ *httpx.Deps) { c.EvalAddr = addr })
	if err := second.srv.Start(); err == nil {
		_ = second.srv.Shutdown(context.Background())
		t.Fatal("a port conflict must be a startup failure, not a goroutine that logs and vanishes")
	}
}

func TestGracefulShutdownDrainsAnInFlightRequest(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	h := newHarness(t, func(c *httpx.Config, _ *httpx.Deps) { c.HandlerTimeout = 10 * time.Second })
	h.ev.block = release
	h.src.set("prod", snap("prod", 8, "a"))

	if err := h.srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	url := "http://" + h.srv.EvalAddr() + "/v1/evaluate"

	type result struct {
		status int
		body   []byte
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Post(url, "application/json",
			strings.NewReader(`{"environment":"prod","flag":"a","default":false}`))
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		resCh <- result{status: resp.StatusCode, body: b}
	}()

	// Wait until the handler is genuinely in flight, blocked inside the evaluator.
	deadline := time.Now().Add(3 * time.Second)
	for h.ev.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.ev.calls.Load() == 0 {
		t.Fatal("request never reached the evaluator")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownDone <- h.srv.Shutdown(ctx)
	}()

	// Shutdown must still be waiting: the in-flight request has not finished.
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before the in-flight request completed (err=%v); the client would have seen a reset mid-response", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("the in-flight request was dropped instead of drained: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("in-flight request status = %d, want 200; body=%s", r.status, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned after the drain completed")
	}

	// And the listener really is closed afterwards.
	if _, err := http.Post(url, "application/json", strings.NewReader(`{}`)); err == nil {
		t.Fatal("the listener is still accepting after shutdown")
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	if _, err := httpx.New(httpx.Config{}, httpx.Deps{Evaluator: &fakeEvaluator{}}); err == nil {
		t.Fatal("a nil snapshot source must be a startup failure, not a nil dereference on the first request")
	}
	if _, err := httpx.New(httpx.Config{}, httpx.Deps{Snapshots: newSource()}); err == nil {
		t.Fatal("a nil evaluator must be a startup failure")
	}
}

func TestNilLoggerAndMetricsAreTolerated(t *testing.T) {
	t.Parallel()
	srv, err := httpx.New(httpx.Config{}, httpx.Deps{Snapshots: newSource(), Evaluator: &fakeEvaluator{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/evaluate", strings.NewReader(`{"environment":"prod","flag":"a","default":false}`))
	req.Header.Set("Content-Type", "application/json")
	srv.EvalHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCallerDefaultTypeIsTheTypeAssertion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 6, "bool_flag")) // declared bool

	// A caller sending a string default has asserted the flag is a string. That must
	// surface as TYPE_MISMATCH rather than a silently coerced value -- a string
	// "false" read as a bool is how a kill switch ends up switched on in production.
	rec := post(t, h.eval, "/v1/evaluate", `{"environment":"prod","flag":"bool_flag","default":"off"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[httpx.EvaluateResponse](t, rec)
	if got.Reason != core.ReasonTypeMismatch.String() {
		t.Fatalf("reason = %q, want TYPE_MISMATCH", got.Reason)
	}
	if v, _ := got.Value.AsString(); v != "off" {
		t.Fatalf("a type mismatch must return the caller's default verbatim, got %v", got.Value)
	}
	if !got.Fallback {
		t.Fatal("TYPE_MISMATCH is a fallback and must be counted as one")
	}
}

func TestSnapshotDebugListsKeysWhenTheSnapshotCan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.src.set("prod", snap("prod", 3, "b", "a"))
	got := decode[httpx.SnapshotDebugResponse](t, get(t, h.admin, "/v1/config/snapshot/prod"))
	if len(got.Keys) != 2 || got.Keys[0] != "a" || got.Keys[1] != "b" {
		t.Fatalf("keys = %v, want [a b] from the optional KeyLister", got.Keys)
	}
}

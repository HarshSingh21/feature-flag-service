package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/config"
	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/internal/transport/apierr"
	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// ---------------------------------------------------------------------------
// Adapters.
//
// These live in the test because the production binary does not have them yet:
// cmd/flagd still wires the stub SnapshotSource and a nil LayerApplier (see the
// TODO(config) block in cmd/flagd/main.go). They are the smallest possible
// widening of config.Store onto the interfaces internal/transport/http declares,
// and nothing more — no behaviour is invented here.
// ---------------------------------------------------------------------------

// storeSource widens *config.Store onto httpx.SnapshotSource.
type storeSource struct{ store *config.Store }

var (
	_ httpx.SnapshotSource    = storeSource{}
	_ httpx.FreshnessReporter = storeSource{}
	// The engine satisfies the transport's evaluator with no adapter at all.
	_ httpx.Evaluator  = (*core.Evaluator)(nil)
	_ client.Evaluator = (*core.Evaluator)(nil)
)

// Snapshot normalises "no snapshot" to a nil INTERFACE. Returning the typed nil
// *config.ResolvedSnapshot would produce a non-nil interface holding a nil
// pointer, and every `snap == nil` guard downstream would silently stop working.
func (s storeSource) Snapshot(env string) (core.Snapshot, bool) {
	snap, ok := s.store.Snapshot(env)
	if !ok || snap == nil {
		return nil, false
	}
	return snap, true
}

// Environments lists only environments that have actually published, which is
// what the interface documents. Store.Environments() also lists environments
// that are merely known.
func (s storeSource) Environments() []string {
	var out []string
	for _, e := range s.store.Environments() {
		if snap, ok := s.store.Snapshot(e); ok && snap != nil {
			out = append(out, e)
		}
	}
	return out
}

func (s storeSource) LastAppliedAt(env string) (time.Time, bool) {
	snap, ok := s.store.Snapshot(env)
	if !ok || snap == nil {
		return time.Time{}, false
	}
	return snap.BuiltAt(), true
}

// layerApplier is the admin write path: decode, hand to the store, map the
// BuildReport onto the transport's result shape.
type layerApplier struct {
	store *config.Store

	mu   sync.Mutex
	last *config.BuildReport
}

var _ httpx.LayerApplier = (*layerApplier)(nil)

func (a *layerApplier) ApplyLayer(_ context.Context, body []byte) (httpx.ApplyResult, error) {
	layer, err := decodeLayer(body)
	if err != nil {
		return httpx.ApplyResult{}, fmt.Errorf("%s: %w", oneLine(err.Error()), httpx.ErrValidation)
	}

	rep := a.store.Set(layer)
	a.mu.Lock()
	a.last = rep
	a.mu.Unlock()

	var out httpx.ApplyResult
	envs := make([]string, 0, len(rep.PerEnv))
	for e := range rep.PerEnv {
		envs = append(envs, e)
	}
	sort.Strings(envs)
	for _, e := range envs {
		res := rep.PerEnv[e]
		if !res.Published {
			continue
		}
		flags := 0
		if snap, ok := a.store.Snapshot(e); ok && snap != nil {
			flags = snap.Len()
		}
		out.Applied = append(out.Applied, httpx.AppliedEnv{Env: e, Generation: res.Generation, Flags: flags})
	}

	if len(out.Applied) == 0 && rep.Findings().HasRejections() {
		// The operator's mistake. Nothing was published anywhere, the live
		// snapshot pointer was never touched, and the caller gets a 400.
		return httpx.ApplyResult{}, fmt.Errorf("%s: %w", oneLine(rep.Findings().Error()), httpx.ErrValidation)
	}
	return out, nil
}

// lastReport exposes the structured rejection to the test.
//
// It has to, because the structured report CANNOT cross the HTTP boundary:
// httpx.LayerApplier returns a bare error on the rejection path, and
// apierr.Sanitize collapses any multi-line message to "internal error" and
// truncates the rest at 200 bytes. See the finding in the report.
func (a *layerApplier) lastReport() *config.BuildReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

func decodeLayer(body []byte) (config.Layer, error) {
	var head struct {
		Layer string `json:"layer"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("config layer body is not a JSON object")
	}
	switch head.Layer {
	case "base":
		return config.ParseBaseLayer(body)
	case "overlay":
		return config.ParseOverlayLayer(body)
	case "ops":
		return config.ParseOpsLayer(body)
	default:
		return nil, fmt.Errorf("unknown layer discriminator %q; want base, overlay or ops", head.Layer)
	}
}

// oneLine flattens a message so apierr.Sanitize does not replace the whole thing
// with "internal error" the moment it sees a newline.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", "; ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}

// ---------------------------------------------------------------------------
// killSwitch — "the service dies".
//
// It sits OUTSIDE the httpx middleware chain, which matters: http.TimeoutHandler
// wraps the ResponseWriter in something that is not an http.Hijacker, so a
// kill mounted inside the chain could only answer 503. Hijacking and closing the
// connection gives the client a transport-level failure — a dead socket, the
// thing that actually happens — rather than a polite error body.
// ---------------------------------------------------------------------------

type killSwitch struct {
	dead atomic.Bool
}

func (k *killSwitch) serveDead(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	http.Error(w, "service is down", http.StatusServiceUnavailable)
}

// ---------------------------------------------------------------------------
// harness — the real service, behind the real transport.
// ---------------------------------------------------------------------------

type harness struct {
	store   *config.Store
	applier *layerApplier
	gate    *killSwitch

	adminSrv *httptest.Server
	evalSrv  *httptest.Server

	hc *http.Client // client A's HTTP client, and the test's own probe client
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store := config.New(config.WithEnvironments(envDev, envProd))
	applier := &layerApplier{store: store}

	srv, err := httpx.New(httpx.Config{
		// Generous handler timeout: the suite runs under -race on a busy machine
		// and a transport timeout would be a test artefact, not a finding.
		HandlerTimeout: 10 * time.Second,
		MaxBatchFlags:  500,
		MaxBodyBytes:   4 << 20,
		StaleAfter:     time.Hour,
		RequiredEnvs:   []string{envProd},
		Service:        "flagd-e2e",
		InstanceID:     instanceID,
	}, httpx.Deps{
		Snapshots: storeSource{store},
		Evaluator: core.New(),
		Applier:   applier,
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}

	// One kill switch in front of BOTH listeners: when the service dies, it dies
	// for the operator and the application at the same instant.
	gate := &killSwitch{}
	h := &harness{
		store:   store,
		applier: applier,
		gate:    gate,
		hc: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 64,
			},
		},
	}
	h.adminSrv = httptest.NewServer(gateFor(gate, srv.AdminHandler()))
	h.evalSrv = httptest.NewServer(gateFor(gate, srv.EvalHandler()))

	t.Cleanup(func() {
		h.hc.CloseIdleConnections()
		h.gate.dead.Store(false) // let the servers drain rather than hijack-kill
		h.evalSrv.Close()
		h.adminSrv.Close()
	})
	return h
}

// gateFor shares one kill switch across two handlers.
func gateFor(k *killSwitch, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k.dead.Load() {
			k.serveDead(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *harness) kill()   { h.gate.dead.Store(true) }
func (h *harness) revive() { h.gate.dead.Store(false) }

// ---------------------------------------------------------------------------
// Client A — the operator. Nothing but HTTP.
// ---------------------------------------------------------------------------

type applyOutcome struct {
	status int
	result httpx.ApplyResult
	errEnv apierr.Envelope
}

// apply is client A's whole vocabulary: POST a layer to the admin listener.
// It never touches *testing.T so it is safe to call from a writer goroutine.
func (h *harness) apply(body []byte) (applyOutcome, error) {
	req, err := http.NewRequest(http.MethodPost, h.adminSrv.URL+"/v1/config/layers", bytes.NewReader(body))
	if err != nil {
		return applyOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.hc.Do(req)
	if err != nil {
		return applyOutcome{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return applyOutcome{}, err
	}
	out := applyOutcome{status: resp.StatusCode}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out.result); err != nil {
			return out, fmt.Errorf("decode apply result: %w (body %s)", err, raw)
		}
		return out, nil
	}
	if err := json.Unmarshal(raw, &out.errEnv); err != nil {
		return out, fmt.Errorf("decode error envelope: %w (body %s)", err, raw)
	}
	return out, nil
}

func (h *harness) mustApply(t *testing.T, body []byte) applyOutcome {
	t.Helper()
	out, err := h.apply(body)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.status != http.StatusOK {
		t.Fatalf("apply: status %d, code %q, message %q", out.status, out.errEnv.ErrorCode, out.errEnv.Message)
	}
	return out
}

// generationIn reports the generation the SERVER is on for env, read back over
// the same admin API an operator would use.
func (h *harness) generationIn(t *testing.T, env string) int64 {
	t.Helper()
	var resp httpx.SnapshotDebugResponse
	status := h.getJSON(t, h.adminSrv.URL+"/v1/config/snapshot/"+env, &resp)
	if status != http.StatusOK {
		t.Fatalf("snapshot debug %s: status %d", env, status)
	}
	return resp.Generation
}

// valueIn reports what the server currently resolves key to in env.
func (h *harness) valueIn(t *testing.T, env, key string) string {
	t.Helper()
	var resp httpx.SnapshotDebugResponse
	status := h.getJSON(t, h.adminSrv.URL+"/v1/config/snapshot/"+env+"?flag="+key, &resp)
	if status != http.StatusOK {
		t.Fatalf("snapshot debug %s/%s: status %d", env, key, status)
	}
	if resp.Flag == nil {
		t.Fatalf("snapshot debug %s/%s: no flag in response", env, key)
	}
	v, _ := resp.Flag.DefaultValue.AsString()
	return v
}

func (h *harness) getJSON(t *testing.T, url string, dst any) int {
	t.Helper()
	resp, err := h.hc.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", url, err)
	}
	if resp.StatusCode == http.StatusOK && dst != nil {
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("GET %s: decode: %v (body %s)", url, err, raw)
		}
	}
	return resp.StatusCode
}

// evaluateBatchOverHTTP drives the SERVER-side batch endpoint, which is where
// invariant CACHE-1 is enforced in eval.go. It is used by the mid-batch variant
// to prove the pin at the transport, not only in the SDK.
func (h *harness) evaluateBatchOverHTTP(env string, keys []string) (httpx.EvaluateBatchResponse, error) {
	req := httpx.EvaluateBatchRequest{Environment: env}
	for _, k := range keys {
		req.Flags = append(req.Flags, httpx.BatchFlag{Flag: k, Default: core.String(callerDefault)})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return httpx.EvaluateBatchResponse{}, err
	}
	hreq, err := http.NewRequest(http.MethodPost, h.evalSrv.URL+"/v1/evaluate/batch", bytes.NewReader(body))
	if err != nil {
		return httpx.EvaluateBatchResponse{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := h.hc.Do(hreq)
	if err != nil {
		return httpx.EvaluateBatchResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpx.EvaluateBatchResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return httpx.EvaluateBatchResponse{}, fmt.Errorf("batch: status %d body %s", resp.StatusCode, raw)
	}
	var out httpx.EvaluateBatchResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return httpx.EvaluateBatchResponse{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Client B — the application.
// ---------------------------------------------------------------------------

// clientLog counts what the SDK reports, so "zero errors" (A4) is checked
// against the client's own account of the run and not only against the results.
type clientLog struct {
	mu     sync.Mutex
	events map[string]int
}

func newClientLog() *clientLog { return &clientLog{events: map[string]int{}} }

func (l *clientLog) Log(_ context.Context, _ client.Level, event string, _ ...any) {
	l.mu.Lock()
	l.events[event]++
	l.mu.Unlock()
}

func (l *clientLog) count(event string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.events[event]
}

// newClient builds client B: a real pkg/client.Client fed by an HTTP Source.
//
// The timings are compressed relative to production defaults (30s dead stream,
// 5m reconcile) because the suite must finish in seconds. Nothing about the
// SHAPE of the update path is changed — push-equivalent frames, dead-stream
// detection, reconcile backstop and jittered reconnect are all still in play.
func (h *harness) newClient(t *testing.T, env string) (*client.Client, *httpSource, *clientLog) {
	t.Helper()
	src := newHTTPSource(h.adminSrv.URL, flagKeys(flagCount), instanceID, 20*time.Millisecond)
	lg := newClientLog()

	cl, err := client.New(
		client.WithEnvironment(env),
		client.WithSource(src),
		client.WithLogger(lg),
		client.WithDeadStreamThreshold(750*time.Millisecond),
		client.WithReconcileInterval(2*time.Second),
		client.WithFetchTimeout(3*time.Second),
		client.WithStalenessWarning(time.Hour),
		client.WithBackoff(client.FullJitterBackoff(5*time.Millisecond, 50*time.Millisecond)),
	)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	t.Cleanup(func() {
		_ = cl.Close()
		src.close()
	})
	return cl, src, lg
}

// waitFor polls a predicate until it holds or the deadline passes. This is the
// only waiting primitive in the suite: §2.4 forbids sleep-then-assert, and every
// assertion is made against the observation log rather than against the clock.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return cond()
		}
		time.Sleep(time.Millisecond)
	}
}

func mustWaitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	if !waitFor(d, cond) {
		t.Fatalf("timed out after %s waiting for %s", d, what)
	}
}

// appliedGeneration pulls one environment's post-write generation out of an
// apply result, failing loudly if the write did not touch that environment.
func appliedGeneration(t *testing.T, out applyOutcome, env string) int64 {
	t.Helper()
	for _, a := range out.result.Applied {
		if a.Env == env {
			return a.Generation
		}
	}
	t.Fatalf("apply did not publish to %s: applied=%v", env, out.result.Applied)
	return 0
}

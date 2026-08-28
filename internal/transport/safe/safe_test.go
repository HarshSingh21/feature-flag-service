package safe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/obs"
	"github.com/HarshSingh21/feature-flag-service/internal/transport/safe"
)

type capture struct {
	mu     sync.Mutex
	calls  int
	site   string
	value  any
	stacks []string
}

func (c *capture) fn(_ context.Context, site string, v any, stack []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.site = site
	c.value = v
	c.stacks = append(c.stacks, string(stack))
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestMiddlewareTurnsAPanicIntoTheStandardEnvelope(t *testing.T) {
	t.Parallel()
	var c capture
	h := obs.TraceMiddleware(safe.Middleware(c.fn)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("evaluator dereferenced a nil rule set")
	})))

	req := httptest.NewRequest(http.MethodPost, "/v1/evaluate", nil)
	req.Header.Set(obs.HeaderTraceID, "trace-abcdef")
	rec := httptest.NewRecorder()

	// The test process is still alive after this line. That is the assertion.
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q, want JSON", ct)
	}
	var env struct {
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
		TraceID   string `json:"trace_id"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the standard envelope: %v (%s)", err, rec.Body)
	}
	if env.ErrorCode != "internal" {
		t.Errorf("error_code = %q, want internal", env.ErrorCode)
	}
	if env.TraceID != "trace-abcdef" {
		t.Errorf("trace_id = %q; the envelope must carry the caller's trace", env.TraceID)
	}
	if env.Timestamp == "" {
		t.Error("envelope timestamp is empty")
	}
	// The one thing that must never cross the boundary.
	body := rec.Body.String()
	for _, leak := range []string{"goroutine", ".go:", "nil rule set", "/Users/", "0x"} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaked internal detail %q: %s", leak, body)
		}
	}
	// ...but the operator still gets the whole stack, in the log.
	if c.count() != 1 {
		t.Fatalf("panic reporter called %d times, want 1", c.count())
	}
	if !strings.Contains(c.stacks[0], "goroutine") {
		t.Fatal("the reporter must receive the stack, since the caller never does")
	}
	if c.site != safe.SiteHTTP {
		t.Errorf("site = %q, want %q", c.site, safe.SiteHTTP)
	}
}

func TestMiddlewareDoesNotOverwriteACommittedResponse(t *testing.T) {
	t.Parallel()
	var c capture
	h := safe.Middleware(c.fn)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":true`))
		panic("boom after commit")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; a committed response cannot be retracted", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "error_code") {
		t.Fatal("must not append an envelope to a half-written body")
	}
	if c.count() != 1 {
		t.Fatal("the panic must still be reported")
	}
}

func TestMiddlewarePassesThroughErrAbortHandler(t *testing.T) {
	t.Parallel()
	var c capture
	h := safe.Middleware(c.fn)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler must reach net/http unchanged, got %v", v)
		}
		if c.count() != 0 {
			t.Fatal("ErrAbortHandler is not a fault and must not be reported as one")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

func TestGoContainsAPanicOnItsOwnGoroutine(t *testing.T) {
	t.Parallel()
	// This is the trap the package exists for. A deferred recover in THIS function
	// would not catch a panic raised on another goroutine; that panic would
	// terminate the whole test binary. If this test completes, the boundary is on
	// the right stack.
	var c capture
	done := make(chan struct{})
	safe.Go(context.Background(), safe.SiteGoroutine, func(ctx context.Context, site string, v any, stack []byte) {
		c.fn(ctx, site, v, stack)
		close(done)
	}, func() {
		var m map[string]int
		m["boom"] = 1 // assignment to entry in nil map
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("panic reporter was never called")
	}
	if c.count() != 1 {
		t.Fatalf("reporter called %d times, want 1", c.count())
	}
	if !strings.Contains(c.stacks[0], "goroutine") {
		t.Fatal("no stack captured for the goroutine panic")
	}
}

func TestGoDoneSignalsCompletionEvenWhenTheGoroutinePanics(t *testing.T) {
	t.Parallel()
	var c capture
	done := safe.GoDone(context.Background(), safe.SiteGoroutine, c.fn, func() {
		panic("background refresh exploded")
	})
	select {
	case <-done:
		// A shutdown path waiting on this channel must not hang because the
		// goroutine died. That is how a graceful shutdown becomes a SIGKILL.
	case <-time.After(2 * time.Second):
		t.Fatal("GoDone never closed its channel after a panic; a waiting shutdown would hang forever")
	}
}

func TestDoReportsWhetherItPanicked(t *testing.T) {
	t.Parallel()
	var c capture
	if safe.Do(context.Background(), safe.SiteEvaluate, c.fn, func() {}) {
		t.Fatal("Do reported a panic for a clean call")
	}
	if !safe.Do(context.Background(), safe.SiteEvaluate, c.fn, func() { panic("x") }) {
		t.Fatal("Do did not report a panic")
	}
	if c.count() != 1 {
		t.Fatalf("reporter called %d times, want 1", c.count())
	}
}

func TestRecoverAsADeferredBoundary(t *testing.T) {
	t.Parallel()
	var c capture
	func() {
		defer safe.Recover(context.Background(), safe.SiteCompile, c.fn)
		panic("compile failed")
	}()
	if c.count() != 1 {
		t.Fatal("deferred Recover did not contain the panic")
	}
}

func TestBoundaryHoldsWhenTheReporterItselfPanics(t *testing.T) {
	t.Parallel()
	// A boundary that can be defeated by its own error handler is not a boundary.
	panicked := safe.Do(context.Background(), safe.SiteEvaluate,
		func(context.Context, string, any, []byte) { panic("logger is nil") },
		func() { panic("original") })
	if !panicked {
		t.Fatal("Do must still report the original panic")
	}
}

func TestNilReporterIsSafe(t *testing.T) {
	t.Parallel()
	if !safe.Do(context.Background(), safe.SiteEvaluate, nil, func() { panic("x") }) {
		t.Fatal("a nil reporter must not defeat containment")
	}
}

func TestConcurrentPanicsAreAllContained(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	var wg sync.WaitGroup
	report := func(context.Context, string, any, []byte) { n.Add(1) }
	for i := 0; i < 64; i++ {
		wg.Add(1)
		safe.Go(context.Background(), safe.SiteGoroutine, report, func() {
			defer wg.Done()
			panic("concurrent boom")
		})
	}
	wg.Wait()
	// wg.Done runs in a defer registered inside fn, so it fires before Recover.
	// Give the reporters a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < 64 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n.Load() != 64 {
		t.Fatalf("contained %d of 64 panics", n.Load())
	}
}

func TestReporterWiresLoggerAndMetrics(t *testing.T) {
	t.Parallel()
	m := obs.NewExpvarMetrics("test_safe_reporter")
	rep := safe.Reporter(obs.NewNop(), obs.NewRecorder(m))
	rep(context.Background(), safe.SiteHTTP, "boom", []byte("stack"))
	if !strings.Contains(m.Var().String(), obs.MetricPanicsTotal) {
		t.Fatal("a contained panic must still increment a counter; silent containment is indistinguishable from health")
	}
}

package httpx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/obs"
	"github.com/HarshSingh21/feature-flag-service/internal/transport/apierr"
	"github.com/HarshSingh21/feature-flag-service/internal/transport/safe"
)

// Config is the transport configuration. Every timeout here has a default, because
// an unset timeout in net/http means "wait forever", and "forever" is how a service
// dies from a handful of idle connections rather than from load.
type Config struct {
	EvalAddr  string
	AdminAddr string

	// ReadHeaderTimeout bounds how long a client may take to send its request
	// headers. Leaving it unset is a slowloris hole: a client that opens a
	// connection and dribbles one header byte per minute holds a server goroutine
	// and a file descriptor indefinitely, and a few thousand of them exhaust the
	// process without ever sending a valid request.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// HandlerTimeout bounds the handler itself. Evaluation is microseconds, so
	// hitting this means something is genuinely wrong and shedding fast beats
	// answering slowly -- "slow, not down" is the failure mode that takes callers
	// out, because their connections pile up waiting on ours.
	HandlerTimeout time.Duration

	// MaxBodyBytes caps a request body. Unbounded decoding is a trivial memory DoS.
	MaxBodyBytes int64

	// MaxBatchFlags caps one batch request. It bounds worst-case handler latency and
	// response size; without it one caller can ask for every flag in the fleet.
	MaxBatchFlags int

	// RequiredEnvs are the environments that must have a snapshot before this pod is
	// ready. Empty means "any one environment is enough", which is the right default
	// for a single-tenant deployment and the wrong one for a shared pod -- set it.
	RequiredEnvs []string

	// StaleAfter is when a snapshot is REPORTED as stale. It never gates readiness.
	StaleAfter time.Duration

	Service    string
	InstanceID string
}

func (c Config) withDefaults() Config {
	if c.EvalAddr == "" {
		c.EvalAddr = ":8080"
	}
	if c.AdminAddr == "" {
		c.AdminAddr = ":9090"
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = 2 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 5 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 60 * time.Second
	}
	if c.HandlerTimeout <= 0 {
		c.HandlerTimeout = 3 * time.Second
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 1 << 20 // 1 MiB
	}
	if c.MaxBatchFlags <= 0 {
		c.MaxBatchFlags = 500
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 60 * time.Second
	}
	if c.Service == "" {
		c.Service = "flagd"
	}
	return c
}

// Deps are the collaborators the HTTP surface needs. All are interfaces declared in
// deps.go; none of them is a concrete type from another package.
type Deps struct {
	Snapshots SnapshotSource
	Evaluator Evaluator
	// Applier may be nil. The admin apply endpoint then answers 503 unavailable,
	// which is the honest answer for a binary whose write path is not wired yet --
	// far better than accepting a config push into a void.
	Applier LayerApplier
	Log     *obs.Logger
	Metrics *obs.Recorder
	// Expvar is mounted at /debug/vars on the admin listener. Nil means the default
	// expvar registry.
	Expvar http.Handler
}

// Server owns both listeners.
type Server struct {
	cfg     Config
	deps    Deps
	onPanic safe.PanicFunc
	started time.Time

	evalSrv  *http.Server
	adminSrv *http.Server

	mu        sync.Mutex
	listeners []net.Listener
	errCh     chan error
	closeOnce sync.Once
}

// New validates the dependencies and builds both servers.
//
// It refuses to construct a server without a snapshot source and an evaluator. A nil
// there would be a nil-pointer panic on the first request instead of a startup
// failure, which turns a config mistake into a production incident.
func New(cfg Config, deps Deps) (*Server, error) {
	if deps.Snapshots == nil {
		return nil, errors.New("httpx: Deps.Snapshots is required")
	}
	if deps.Evaluator == nil {
		return nil, errors.New("httpx: Deps.Evaluator is required")
	}
	if deps.Log == nil {
		deps.Log = obs.NewNop()
	}
	if deps.Metrics == nil {
		deps.Metrics = obs.NewRecorder(nil)
	}
	cfg = cfg.withDefaults()

	s := &Server{
		cfg:     cfg,
		deps:    deps,
		onPanic: safe.Reporter(deps.Log, deps.Metrics),
		started: time.Now(),
		errCh:   make(chan error, 2),
	}
	s.evalSrv = s.newHTTPServer(cfg.EvalAddr, s.EvalHandler())
	s.adminSrv = s.newHTTPServer(cfg.AdminAddr, s.AdminHandler())
	return s, nil
}

func (s *Server) newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.ReadTimeout,
		WriteTimeout:      s.cfg.WriteTimeout,
		IdleTimeout:       s.cfg.IdleTimeout,
		// ErrorLog is deliberately left nil: net/http then writes to the standard
		// logger, which the process redirects. Wiring slog in here would double-log
		// every connection reset at error level.
	}
}

// EvalHandler is the evaluation listener's handler. Exported so tests can drive the
// exact production stack through httptest without binding a port.
func (s *Server) EvalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/evaluate", s.handleEvaluate)
	mux.HandleFunc("POST /v1/evaluate/batch", s.handleEvaluateBatch)
	s.mountHealth(mux)
	mux.HandleFunc("/", s.handleNotFound)
	return s.wrap(mux)
}

// AdminHandler is the admin listener's handler.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/config/layers", s.handleApplyLayer)
	mux.HandleFunc("GET /v1/config/snapshot/{env}", s.handleSnapshotDebug)
	ev := s.deps.Expvar
	if ev == nil {
		ev = expvarHandler()
	}
	mux.Handle("GET /debug/vars", ev)
	s.mountHealth(mux)
	mux.HandleFunc("/", s.handleNotFound)
	return s.wrap(mux)
}

// Health lives on BOTH listeners. A probe that can only reach one port cannot tell
// you the other one is wedged, and Kubernetes probes the port it is told about.
func (s *Server) mountHealth(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /live", s.handleLive)
	mux.HandleFunc("GET /ready", s.handleReady)
}

// wrap builds the middleware chain, outermost first.
//
// Order is load bearing:
//   - trace is outermost so a trace id exists before anything can fail, including the
//     panic boundary's own log line;
//   - metrics next, so it observes the status the client actually received;
//   - the panic boundary sits outside the timeout handler, because TimeoutHandler
//     re-panics on the outer goroutine and something has to catch it;
//   - TimeoutHandler is innermost so it bounds only the handler, not the middleware.
func (s *Server) wrap(h http.Handler) http.Handler {
	h = http.TimeoutHandler(h, s.cfg.HandlerTimeout, apierr.TimeoutBody)
	h = safe.Middleware(s.onPanic)(h)
	h = s.metricsMiddleware(h)
	h = obs.TraceMiddleware(h)
	return h
}

// Start binds both listeners and begins serving.
//
// Binding happens synchronously so a port conflict is a startup failure with a
// non-zero exit, not a goroutine that logs and vanishes while the process reports
// itself healthy.
func (s *Server) Start() error {
	evalLn, err := net.Listen("tcp", s.cfg.EvalAddr)
	if err != nil {
		return fmt.Errorf("httpx: bind eval listener %s: %w", s.cfg.EvalAddr, err)
	}
	adminLn, err := net.Listen("tcp", s.cfg.AdminAddr)
	if err != nil {
		_ = evalLn.Close()
		return fmt.Errorf("httpx: bind admin listener %s: %w", s.cfg.AdminAddr, err)
	}
	s.mu.Lock()
	s.listeners = []net.Listener{evalLn, adminLn}
	s.mu.Unlock()

	s.serve(s.evalSrv, evalLn, "eval")
	s.serve(s.adminSrv, adminLn, "admin")
	return nil
}

func (s *Server) serve(srv *http.Server, ln net.Listener, name string) {
	ctx := context.Background()
	// safe.Go, not `go`: an accept-loop panic on a bare goroutine takes the process
	// down and every flag it serves with it.
	safe.Go(ctx, safe.SiteGoroutine, s.onPanic, func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errCh <- fmt.Errorf("httpx: %s listener: %w", name, err)
			return
		}
		s.errCh <- nil
	})
}

// Err delivers a fatal listener error. main selects on it alongside the signal
// context so a dead listener is as loud as a SIGTERM.
func (s *Server) Err() <-chan error { return s.errCh }

// EvalAddr and AdminAddr report the bound addresses, which is how a caller learns the
// port when the configured address used :0.
func (s *Server) EvalAddr() string  { return s.boundAddr(0) }
func (s *Server) AdminAddr() string { return s.boundAddr(1) }

func (s *Server) boundAddr(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.listeners) {
		return ""
	}
	return s.listeners[i].Addr().String()
}

// Shutdown drains both listeners.
//
// http.Server.Shutdown stops accepting, closes idle connections, and waits for
// in-flight requests to finish. The deadline on ctx is the bound: without one, a
// single wedged request holds the pod past the orchestrator's grace period and it
// gets SIGKILLed mid-request anyway, which is the outcome graceful shutdown exists to
// avoid.
func (s *Server) Shutdown(ctx context.Context) error {
	var evalErr, adminErr error
	s.closeOnce.Do(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			adminErr = s.adminSrv.Shutdown(ctx)
		}()
		// Drain eval last-ish but concurrently: admin traffic is low volume and
		// stopping it first prevents a config push landing during the drain.
		evalErr = s.evalSrv.Shutdown(ctx)
		<-done
	})
	return errors.Join(evalErr, adminErr)
}

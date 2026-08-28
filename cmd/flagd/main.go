// Command flagd is the feature flag evaluation service.
//
// It runs two HTTP listeners in one process: evaluation on :8080 and admin on :9090.
// See internal/transport/http for why they are separate listeners but not separate
// deployables.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/internal/obs"
	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
)

func main() {
	if err := run(); err != nil {
		// Startup failures must be loud and must exit non-zero. A process that
		// cannot bind its port but stays alive is the worst outcome: the
		// orchestrator sees a running container, the probe never gets answered, and
		// the rollout stalls with no obvious cause.
		fmt.Fprintf(os.Stderr, "flagd: fatal: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	evalAddr        string
	adminAddr       string
	logLevel        string
	instanceID      string
	requiredEnvs    string
	shutdownTimeout time.Duration
	handlerTimeout  time.Duration
	maxBatchFlags   int
	maxBodyBytes    int64
	staleAfter      time.Duration
	allowPII        bool
}

// parseOptions reads flags, with an environment-variable fallback for each so the
// same binary is configurable by a Kubernetes manifest without a wrapper script.
// Explicit flags win over the environment.
func parseOptions(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("flagd", flag.ContinueOnError)
	fs.StringVar(&o.evalAddr, "eval-addr", env("FLAGD_EVAL_ADDR", ":8080"), "listen address for the evaluation API")
	fs.StringVar(&o.adminAddr, "admin-addr", env("FLAGD_ADMIN_ADDR", ":9090"), "listen address for the admin API")
	fs.StringVar(&o.logLevel, "log-level", env("FLAGD_LOG_LEVEL", "info"), "debug | info | warn | error")
	fs.StringVar(&o.instanceID, "instance-id", env("FLAGD_INSTANCE_ID", defaultInstanceID()), "identity of this replica, stamped on every log line")
	fs.StringVar(&o.requiredEnvs, "required-envs", env("FLAGD_REQUIRED_ENVS", ""), "comma-separated environments that must have a snapshot before /ready succeeds")
	fs.DurationVar(&o.shutdownTimeout, "shutdown-timeout", envDuration("FLAGD_SHUTDOWN_TIMEOUT", 25*time.Second), "bound on draining in-flight requests at SIGTERM")
	fs.DurationVar(&o.handlerTimeout, "handler-timeout", envDuration("FLAGD_HANDLER_TIMEOUT", 3*time.Second), "per-request handler deadline")
	fs.DurationVar(&o.staleAfter, "stale-after", envDuration("FLAGD_STALE_AFTER", 60*time.Second), "age at which a snapshot is reported stale (advisory; never gates readiness)")
	fs.IntVar(&o.maxBatchFlags, "max-batch-flags", envInt("FLAGD_MAX_BATCH_FLAGS", 500), "maximum flags in one batch evaluation")
	fs.Int64Var(&o.maxBodyBytes, "max-body-bytes", int64(envInt("FLAGD_MAX_BODY_BYTES", 1<<20)), "maximum request body size")
	fs.BoolVar(&o.allowPII, "allow-pii-logs", false, "DEBUG ONLY: disable log redaction of personally identifying fields")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	return o, nil
}

func run() error {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		return err
	}

	level, err := parseLevel(opts.logLevel)
	if err != nil {
		return err
	}

	log := obs.New(obs.Options{
		Output:     os.Stdout,
		Level:      level,
		Service:    "flagd",
		InstanceID: opts.instanceID,
		AllowPII:   opts.allowPII,
		// One evaluation-error line per second per (flag, reason), burst 5. Without
		// this, one misconfigured flag at a million evaluations a second writes a
		// million log lines a second and takes down the logging pipeline -- an
		// outage strictly worse than the flag bug that caused it.
		Sampler: obs.NewSampler(1, 5, 4096),
	})
	metrics := obs.NewExpvarMetrics("flagd")
	rec := obs.NewRecorder(metrics)

	// The shutdown context is cancelled by SIGTERM (orchestrator) or SIGINT (a human
	// with a terminal). Both mean the same thing here: stop accepting, drain, exit.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	deps, err := wire(ctx, log, rec)
	if err != nil {
		return fmt.Errorf("wiring dependencies: %w", err)
	}

	srv, err := httpx.New(httpx.Config{
		EvalAddr:       opts.evalAddr,
		AdminAddr:      opts.adminAddr,
		HandlerTimeout: opts.handlerTimeout,
		MaxBodyBytes:   opts.maxBodyBytes,
		MaxBatchFlags:  opts.maxBatchFlags,
		RequiredEnvs:   splitCSV(opts.requiredEnvs),
		StaleAfter:     opts.staleAfter,
		Service:        "flagd",
		InstanceID:     opts.instanceID,
	}, deps)
	if err != nil {
		return err
	}

	if err := srv.Start(); err != nil {
		return err
	}
	log.Info(ctx, "flagd started",
		"eval_addr", srv.EvalAddr(),
		"admin_addr", srv.AdminAddr(),
		"required_envs", splitCSV(opts.requiredEnvs),
	)

	select {
	case <-ctx.Done():
		log.Info(context.Background(), "shutdown signal received, draining")
	case err := <-srv.Err():
		if err != nil {
			// A dead listener is as fatal as a signal. Without this branch the
			// process would sit here with no listener, reporting itself alive.
			_ = srv.Shutdown(context.Background())
			return err
		}
	}

	// Bounded drain. Unbounded would let one wedged request hold the pod past the
	// orchestrator's grace period, at which point it is SIGKILLed mid-request --
	// exactly the outcome graceful shutdown exists to avoid.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error(context.Background(), "shutdown did not complete cleanly", obs.KeyError, err.Error())
		return err
	}
	log.Info(context.Background(), "flagd stopped")
	return nil
}

// The real engine satisfies the transport's Evaluator interface with no adapter.
// This assertion is what keeps that true: if either signature drifts, the build
// breaks here rather than at the seam during integration.
var _ httpx.Evaluator = (*core.Evaluator)(nil)

// wire builds the service's dependencies.
//
// The evaluator is real. The CONFIG STORE is the remaining stub.
//
// TODO(config): wire internal/config. Three lines change, and only three:
//
//	store := config.New(...)
//	deps.Snapshots = storeSource{store}   // adapter below; see the note on it
//	deps.Applier   = &storeApplier{store} // unmarshal Layer, call store.Set,
//	                                      // map BuildReport -> httpx.ApplyResult,
//	                                      // and wrap operator mistakes with
//	                                      // httpx.ErrValidation so they answer 400
//	                                      // and never page
//
// config.Store already exposes Snapshot(env) (*config.ResolvedSnapshot, bool) and
// Environments() []string, and *config.ResolvedSnapshot satisfies core.Snapshot, so
// the adapter is a return-type widening and nothing more. ResolvedSnapshot.BuiltAt()
// also makes httpx.FreshnessReporter a two-line addition, which is what lights up the
// staleness field on /ready and /health.
//
// Until then the stub is deliberately honest rather than convenient: the snapshot
// source reports that nothing has loaded, so /ready answers 503 and this pod takes no
// traffic. A stub that pretended to be ready would be the exact failure hazard H6
// describes -- a fleet quietly serving compile-time defaults with every dashboard
// green.
func wire(_ context.Context, log *obs.Logger, rec *obs.Recorder) (httpx.Deps, error) {
	log.Warn(context.Background(), "config store is not wired; serving no snapshots and reporting NOT ready",
		obs.KeyEvent, "startup.stub_wiring")
	return httpx.Deps{
		Snapshots: emptySnapshotSource{},
		Evaluator: core.New(),
		Applier:   nil, // admin apply answers 503 until the real applier is wired
		Log:       log,
		Metrics:   rec,
	}, nil
}

// emptySnapshotSource has never loaded anything. It is not an error state -- see
// internal/transport/http/deps.go -- but it does keep /ready false.
type emptySnapshotSource struct{}

func (emptySnapshotSource) Snapshot(string) (core.Snapshot, bool) { return nil, false }
func (emptySnapshotSource) Environments() []string                { return nil }

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// defaultInstanceID answers "which pod?" in a log line. The hostname is what the
// orchestrator already shows, so it is the identifier an engineer can act on.
func defaultInstanceID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

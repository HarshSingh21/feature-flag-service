package obs

import (
	"strconv"
	"time"
)

// Recorder is the typed façade over Metrics.
//
// It exists so that no call site outside this package ever constructs a Label by
// hand. That is the structural half of the cardinality defence: adding a
// high-cardinality dimension requires editing this file and failing a review, not
// passing a string at a call site during an incident. ValidateLabel is the
// mechanical backstop for the paths that do build labels dynamically.
type Recorder struct {
	m Metrics
}

// NewRecorder wraps m. A nil m yields a no-op recorder, so nothing has to nil-check.
func NewRecorder(m Metrics) *Recorder {
	if m == nil {
		m = NopMetrics()
	}
	return &Recorder{m: m}
}

// Metrics exposes the underlying registry.
func (r *Recorder) Metrics() Metrics { return r.m }

// Evaluation records one flag evaluation.
//
// flag, env and reason are all bounded and all three are labels. The latency
// histogram deliberately carries NO labels: twelve buckets times five thousand
// flags is sixty thousand series for a number nobody segments by flag. If per-flag
// latency is ever genuinely needed, that is a trace, not a metric.
func (r *Recorder) Evaluation(flag, env, reason string, fallback bool, d time.Duration) {
	r.m.Counter(MetricEvaluationsTotal, L("flag", flag), L("env", env), L("reason", reason)).Inc()
	r.m.Histogram(MetricEvaluationSeconds).Observe(d.Seconds())
	if fallback {
		// The fallback rate is hazard H1, silent fail-open: a degraded service
		// returns the caller's default for everything and nothing errors. Alert on
		// this being unexpectedly LOW as well as high -- a metric that is always
		// zero is usually a metric that was never wired up.
		r.m.Counter(MetricFallbackTotal, L("flag", flag), L("env", env), L("reason", reason)).Inc()
	}
}

// BatchLatency records the wall time of a whole batch evaluation, separate from the
// per-flag histogram so the per-call overhead the batch endpoint exists to amortise
// stays measurable.
func (r *Recorder) BatchLatency(d time.Duration) {
	r.m.Histogram(MetricEvaluationSeconds).Observe(d.Seconds())
}

// ConfigApply records the outcome of a config push. result must be one of
// ApplyOK, ApplyRejectedValidation, ApplyRejectedInternal -- a closed set, because
// rejected_validation must never page (the operator got a synchronous 400 and the
// system behaved correctly) while rejected_internal always must.
func (r *Recorder) ConfigApply(result string, d time.Duration) {
	r.m.Counter(MetricConfigApplyTotal, L("result", result)).Inc()
	r.m.Histogram(MetricConfigApplySeconds, L("stage", "publish")).Observe(d.Seconds())
}

// PropagationLag records publish-to-apply latency, in seconds.
//
// Kept as both a histogram (for the SLO) and a gauge of the last sample (so a
// dashboard has something to show when no config has changed for six hours, which
// is exactly when a broken propagation path is invisible).
func (r *Recorder) PropagationLag(seconds float64) {
	r.m.Histogram(MetricPropagationLag).Observe(seconds)
	r.m.Gauge(MetricPropagationLag + "_last").Set(seconds)
}

// SnapshotGeneration publishes the live generation for an environment. This is the
// gauge that answers "which config is this pod actually serving?".
func (r *Recorder) SnapshotGeneration(env string, generation int64) {
	r.m.Gauge(MetricSnapshotGeneration, L("env", env)).Set(float64(generation))
}

// SnapshotFlags publishes the flag count for an environment.
func (r *Recorder) SnapshotFlags(env string, n int) {
	r.m.Gauge(MetricSnapshotFlags, L("env", env)).Set(float64(n))
}

// HTTPRequest records one served request. route is a registered route pattern, never
// a raw request path -- a raw path is attacker-controlled and therefore unbounded.
func (r *Recorder) HTTPRequest(route, method string, status int, d time.Duration) {
	code := strconv.Itoa(status)
	r.m.Counter(MetricHTTPRequestsTotal, L("route", route), L("method", method), L("code", code)).Inc()
	r.m.Histogram(MetricHTTPSeconds, L("route", route)).Observe(d.Seconds())
}

// Panic records a contained panic at a named boundary. site is a fixed set.
func (r *Recorder) Panic(site string) {
	r.m.Counter(MetricPanicsTotal, L("site", site)).Inc()
}

package obs

import (
	"errors"
	"expvar"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metric names. Kept as constants for the same reason log keys are.
const (
	MetricEvaluationsTotal   = "flagsvc_evaluations_total"
	MetricEvaluationSeconds  = "flagsvc_evaluation_duration_seconds"
	MetricFallbackTotal      = "flagsvc_evaluation_fallback_total"
	MetricConfigApplyTotal   = "flagsvc_config_apply_total"
	MetricConfigApplySeconds = "flagsvc_config_apply_duration_seconds"
	MetricPropagationLag     = "flagsvc_propagation_lag_seconds"
	MetricSnapshotGeneration = "flagsvc_snapshot_generation"
	MetricSnapshotFlags      = "flagsvc_snapshot_flags"
	MetricHTTPRequestsTotal  = "flagsvc_http_requests_total"
	MetricHTTPSeconds        = "flagsvc_http_request_duration_seconds"
	MetricPanicsTotal        = "flagsvc_panics_total"
	MetricLabelsDropped      = "flagsvc_metrics_labels_dropped_total"
	MetricSeriesOverflow     = "flagsvc_metrics_series_overflow_total"
)

// Config-apply results. A closed set, because it is a metric label.
const (
	ApplyOK                 = "ok"
	ApplyRejectedValidation = "rejected_validation"
	ApplyRejectedInternal   = "rejected_internal"
)

// Label is one metric dimension.
//
// The API is a typed struct rather than a map[string]string or a variadic string
// pair list because the cardinality rule has to be enforceable at the point of use;
// see ValidateLabel.
type Label struct {
	Name  string
	Value string
}

// L is shorthand for constructing a Label.
func L(name, value string) Label { return Label{Name: name, Value: value} }

// allowedLabelNames is a closed allowlist. Anything not on it is dropped.
//
// An allowlist, not a denylist. A denylist only stops the high-cardinality labels
// somebody already thought of; the one that takes the metrics backend down will be
// a name nobody anticipated, added at 2am by an engineer who just wants to see which
// cohort got the treatment.
var allowedLabelNames = map[string]bool{
	"flag":   true, // bounded by config, ~500-5000
	"env":    true, // bounded, ~5, and validated at the transport edge
	"reason": true, // closed Go enum, 10 values
	"result": true, // closed enum: ok | rejected_validation | rejected_internal
	"stage":  true, // closed enum: validate | resolve | build | serialize | publish
	"state":  true, // closed enum: connected | acked_current
	"route":  true, // fixed set of registered routes, never a raw request path
	"method": true, // HTTP verbs
	"code":   true, // HTTP status codes
	"site":   true, // fixed set of panic boundary names
}

// deniedLabelNames exist purely so the rejection carries a specific reason. They are
// already excluded by the allowlist; naming them turns a silent drop into a
// diagnosable one and documents the trap for the next reader.
var deniedLabelNames = map[string]string{
	"user_id":    "unbounded: 1e9 users x 500 flags is 5e11 series, an outage of the metrics backend",
	"tenant_id":  "unbounded until a ceiling is proven; log it instead",
	"session_id": "unbounded",
	"request_id": "unbounded",
	"trace_id":   "unbounded; this is what tracing is for",
	"span_id":    "unbounded",
	"rule_id":    "grows with config edits and is already carried on Result and in logs",
	"bucket":     "10000 distinct values per flag",
	"path":       "attacker-controlled; use the route label",
	"url":        "attacker-controlled",
	"query":      "attacker-controlled",
	"ip":         "unbounded and personally identifying",
	"email":      "unbounded and personally identifying",
	"value":      "flag values are unbounded for string flags",
}

// MaxLabelValueLen bounds a single label value. A label value is a series key
// component; an unbounded one is an unbounded series name.
const MaxLabelValueLen = 96

// ErrLabelRejected is returned by ValidateLabel for any label that must not be used.
var ErrLabelRejected = errors.New("obs: label rejected")

// ValidateLabel is the cardinality guard.
//
// It is a function, not a comment in a style guide, because the failure it prevents
// is invisible in review and catastrophic in production: adding one unbounded label
// does not degrade the metrics backend gradually, it takes it down, and it takes it
// down during the incident that motivated adding the label.
func ValidateLabel(l Label) error {
	name := strings.ToLower(strings.TrimSpace(l.Name))
	if name == "" {
		return fmt.Errorf("%w: empty label name", ErrLabelRejected)
	}
	if why, denied := deniedLabelNames[name]; denied {
		return fmt.Errorf("%w: %q is %s", ErrLabelRejected, name, why)
	}
	if !allowedLabelNames[name] {
		return fmt.Errorf("%w: %q is not on the bounded-label allowlist %v", ErrLabelRejected, name, sortedAllowed())
	}
	if len(l.Value) > MaxLabelValueLen {
		return fmt.Errorf("%w: value for %q is %d bytes, limit %d", ErrLabelRejected, name, len(l.Value), MaxLabelValueLen)
	}
	return nil
}

func sortedAllowed() []string {
	out := make([]string, 0, len(allowedLabelNames))
	for k := range allowedLabelNames {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AllowedLabelNames returns the allowlist, for tests and documentation.
func AllowedLabelNames() []string { return sortedAllowed() }

// SanitizeLabels drops every label that fails the guard and reports what it dropped.
//
// Dropping rather than panicking is deliberate: a bad label must not be able to take
// the service down, but it also must not silently reach the backend. The drop is
// counted on flagsvc_metrics_labels_dropped_total, which is a real alert.
func SanitizeLabels(labels []Label) (kept []Label, dropped []string) {
	for _, l := range labels {
		if err := ValidateLabel(l); err != nil {
			dropped = append(dropped, l.Name)
			continue
		}
		kept = append(kept, Label{Name: strings.ToLower(strings.TrimSpace(l.Name)), Value: l.Value})
	}
	return kept, dropped
}

// seriesKey renders name plus sorted labels into a stable series identifier.
func seriesKey(name string, labels []Label) string {
	if len(labels) == 0 {
		return name
	}
	ls := make([]Label, len(labels))
	copy(ls, labels)
	sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, l := range ls {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(l.Value))
	}
	b.WriteByte('}')
	return b.String()
}

// Counter only ever increases.
type Counter interface {
	Inc()
	Add(delta float64)
}

// Gauge is a value that goes up and down.
type Gauge interface {
	Set(v float64)
	Add(delta float64)
}

// Histogram records a distribution.
type Histogram interface {
	Observe(v float64)
}

// Metrics is the whole metrics surface the service depends on.
//
// It is an interface with an expvar implementation because the alternative -- a
// Prometheus client dependency -- buys nothing this service needs today. A
// Prometheus adapter behind this interface is roughly fifty lines whenever it is
// wanted, and until then the module stays free of a transitive dependency tree.
type Metrics interface {
	Counter(name string, labels ...Label) Counter
	Gauge(name string, labels ...Label) Gauge
	Histogram(name string, labels ...Label) Histogram
}

// ---------- no-op ----------

type nopCounter struct{}

func (nopCounter) Inc()            {}
func (nopCounter) Add(float64)     {}
func (nopCounter) Set(float64)     {}
func (nopCounter) Observe(float64) {}

type nopMetrics struct{}

func (nopMetrics) Counter(string, ...Label) Counter     { return nopCounter{} }
func (nopMetrics) Gauge(string, ...Label) Gauge         { return nopCounter{} }
func (nopMetrics) Histogram(string, ...Label) Histogram { return nopCounter{} }

// NopMetrics returns a Metrics that records nothing. It is the safe default so no
// call site has to nil-check a recorder.
func NopMetrics() Metrics { return nopMetrics{} }

// ---------- expvar ----------

// DefaultLatencyBuckets covers a microsecond-scale evaluation path through a
// multi-second outlier in twelve buckets. The lower half matters: the evaluation
// budget is 50 microseconds, and a histogram whose smallest bucket is 5 ms cannot
// tell a healthy service from a broken one.
var DefaultLatencyBuckets = []float64{
	0.000005, 0.00001, 0.000025, 0.00005, 0.0001, 0.00025,
	0.0005, 0.001, 0.005, 0.025, 0.1, 1,
}

// DefaultPropagationBuckets straddle the 5 second hard propagation contract, with
// resolution around the 1 s p99 target and a bucket boundary exactly on 5 s so the
// SLO is readable straight off the histogram.
var DefaultPropagationBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 30,
}

// DefaultMaxSeries caps total distinct series in one registry.
//
// This is the second half of the cardinality guard. The label allowlist stops
// unbounded label NAMES; this stops unbounded label VALUES -- a caller sending a
// fresh env string on every request would otherwise mint a series per request even
// though "env" is a perfectly respectable label name.
const DefaultMaxSeries = 5000

// ExpvarMetrics is a Metrics backed by expvar, exposed on /debug/vars.
type ExpvarMetrics struct {
	mu        sync.Mutex
	root      *expvar.Map
	series    map[string]expvar.Var
	maxSeries int
	buckets   map[string][]float64

	dropped  *expvar.Int
	overflow *expvar.Int
}

var publishMu sync.Mutex

// publishMap registers name on the global expvar registry, tolerating a name that
// is already registered. expvar.NewMap panics on a duplicate, which would make a
// second registry in one process -- or a second test in one binary -- fatal.
func publishMap(name string) *expvar.Map {
	publishMu.Lock()
	defer publishMu.Unlock()
	if v := expvar.Get(name); v != nil {
		if m, ok := v.(*expvar.Map); ok {
			return m
		}
		name = name + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return expvar.NewMap(name)
}

// NewExpvarMetrics returns a registry publishing under the given expvar name.
func NewExpvarMetrics(namespace string) *ExpvarMetrics {
	if namespace == "" {
		namespace = "flagsvc"
	}
	m := &ExpvarMetrics{
		root:      publishMap(namespace),
		series:    make(map[string]expvar.Var),
		maxSeries: DefaultMaxSeries,
		buckets:   map[string][]float64{},
		dropped:   new(expvar.Int),
		overflow:  new(expvar.Int),
	}
	m.root.Set(MetricLabelsDropped, m.dropped)
	m.root.Set(MetricSeriesOverflow, m.overflow)
	m.SetBuckets(MetricEvaluationSeconds, DefaultLatencyBuckets)
	m.SetBuckets(MetricHTTPSeconds, DefaultLatencyBuckets)
	m.SetBuckets(MetricConfigApplySeconds, DefaultLatencyBuckets)
	m.SetBuckets(MetricPropagationLag, DefaultPropagationBuckets)
	return m
}

// SetBuckets overrides the bucket boundaries for one histogram name. It must be
// called before the first observation on that name.
func (m *ExpvarMetrics) SetBuckets(name string, bounds []float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := make([]float64, len(bounds))
	copy(b, bounds)
	sort.Float64s(b)
	m.buckets[name] = b
}

// SetMaxSeries overrides the series cap.
func (m *ExpvarMetrics) SetMaxSeries(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxSeries = n
}

// DroppedLabels reports how many labels the cardinality guard has rejected.
func (m *ExpvarMetrics) DroppedLabels() int64 { return m.dropped.Value() }

// OverflowedSeries reports how many observations landed in the overflow series
// because the registry hit its series cap.
func (m *ExpvarMetrics) OverflowedSeries() int64 { return m.overflow.Value() }

// Var exposes the registry as an expvar.Var, for mounting on /debug/vars.
func (m *ExpvarMetrics) Var() expvar.Var { return m.root }

// key resolves a metric name plus labels to a series key, applying both halves of
// the cardinality guard.
func (m *ExpvarMetrics) key(name string, labels []Label) string {
	kept, dropped := SanitizeLabels(labels)
	if n := len(dropped); n > 0 {
		m.dropped.Add(int64(n))
	}
	return seriesKey(name, kept)
}

func (m *ExpvarMetrics) get(name string, labels []Label, make func(metricName string) expvar.Var) expvar.Var {
	k := m.key(name, labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.series[k]; ok {
		return v
	}
	if len(m.series) >= m.maxSeries {
		// Collapse into a single overflow series rather than growing without bound.
		// Losing per-label resolution beats losing the metrics backend, and the
		// overflow counter makes the loss visible instead of silent.
		m.overflow.Add(1)
		k = name + "{__overflow__}"
		if v, ok := m.series[k]; ok {
			return v
		}
	}
	v := make(name)
	m.series[k] = v
	m.root.Set(k, v)
	return v
}

func (m *ExpvarMetrics) Counter(name string, labels ...Label) Counter {
	v := m.get(name, labels, func(string) expvar.Var { return new(expvar.Float) })
	return floatMetric{v.(*expvar.Float)}
}

func (m *ExpvarMetrics) Gauge(name string, labels ...Label) Gauge {
	v := m.get(name, labels, func(string) expvar.Var { return new(expvar.Float) })
	return floatMetric{v.(*expvar.Float)}
}

func (m *ExpvarMetrics) Histogram(name string, labels ...Label) Histogram {
	v := m.get(name, labels, func(metricName string) expvar.Var {
		b := m.buckets[metricName]
		if len(b) == 0 {
			b = DefaultLatencyBuckets
		}
		return newHistogram(b)
	})
	h, ok := v.(*histogram)
	if !ok {
		return nopCounter{}
	}
	return h
}

type floatMetric struct{ f *expvar.Float }

func (c floatMetric) Inc()           { c.f.Add(1) }
func (c floatMetric) Add(d float64)  { c.f.Add(d) }
func (c floatMetric) Set(v float64)  { c.f.Set(v) }
func (c floatMetric) Value() float64 { return c.f.Value() }

// histogram is a cumulative bucket histogram that renders itself as JSON for expvar.
type histogram struct {
	mu     sync.Mutex
	bounds []float64
	counts []uint64
	sum    float64
	total  uint64
}

func newHistogram(bounds []float64) *histogram {
	b := make([]float64, len(bounds))
	copy(b, bounds)
	return &histogram{bounds: b, counts: make([]uint64, len(b)+1)}
}

func (h *histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v)
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.total++
	h.mu.Unlock()
}

// Snapshot returns count, sum, and per-bucket counts. For tests and for anything
// that wants the numbers without parsing the JSON.
func (h *histogram) Snapshot() (count uint64, sum float64, counts []uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make([]uint64, len(h.counts))
	copy(c, h.counts)
	return h.total, h.sum, c
}

func (h *histogram) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	b.WriteString(`{"count":`)
	b.WriteString(strconv.FormatUint(h.total, 10))
	b.WriteString(`,"sum":`)
	b.WriteString(strconv.FormatFloat(h.sum, 'g', -1, 64))
	b.WriteString(`,"buckets":{`)
	var cum uint64
	for i, bound := range h.bounds {
		cum += h.counts[i]
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strconv.FormatFloat(bound, 'g', -1, 64))
		b.WriteString(`":`)
		b.WriteString(strconv.FormatUint(cum, 10))
	}
	cum += h.counts[len(h.counts)-1]
	if len(h.bounds) > 0 {
		b.WriteByte(',')
	}
	b.WriteString(`"+Inf":`)
	b.WriteString(strconv.FormatUint(cum, 10))
	b.WriteString("}}")
	return b.String()
}

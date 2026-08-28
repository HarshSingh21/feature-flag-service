// Package obs is the observability edge of the flag service: structured logging,
// metrics, and trace propagation.
//
// It is ring 3. The evaluation core imports nothing from here -- the core returns
// faults as data (core.Reason) and this package decides how they surface. That is
// what keeps "the evaluator never performs I/O" a property rather than a promise.
//
// This package depends on the standard library only.
package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Log field names. They are constants because a dashboard, an alert query, and a
// log-based SLI all key off the exact string; a typo in one call site produces a
// field that exists but is invisible to every query anyone wrote.
const (
	KeyEvent       = "event"
	KeyTraceID     = "trace_id"
	KeySpanID      = "span_id"
	KeyFlag        = "flag"
	KeyEnv         = "env"
	KeyReason      = "reason"
	KeyGeneration  = "generation"
	KeyRuleID      = "rule_id"
	KeyStack       = "stack"
	KeyError       = "err"
	KeyCtxKeys     = "ctx_keys"
	KeySampledOf   = "sampled_of"
	KeyService     = "service"
	KeyInstanceID  = "instance_id"
	KeyValueSource = "returned_value_source"
	KeySite        = "site"
)

// Event names.
const (
	EventEvaluationError = "flag.evaluation.error"
	EventPanic           = "runtime.panic.recovered"
	EventConfigApply     = "config.apply"
	EventHTTPRequest     = "http.request"
)

// piiKeys are log attribute names whose VALUES are never emitted by default.
//
// The evaluation context is arbitrary caller-supplied data and will contain PII --
// that is not a hypothetical, it is what an EvalContext is for. The rule the design
// commits to is: log the NAMES of context keys, never the values. This set is the
// mechanical backstop for the times someone forgets.
//
// A user id is the specific one worth naming. It is the field an engineer most wants
// mid-incident ("which users got the treatment?") and the one that turns a log store
// into a PII store. It is dropped outright below error level.
var piiKeys = map[string]bool{
	"user_id":         true,
	"userid":          true,
	"user":            true,
	"subject":         true,
	"bucket_key":      true,
	"email":           true,
	"phone":           true,
	"ip":              true,
	"ip_address":      true,
	"remote_addr":     true,
	"session_id":      true,
	"cookie":          true,
	"authorization":   true,
	"token":           true,
	"api_key":         true,
	"password":        true,
	"secret":          true,
	"attributes":      true,
	"context_values":  true,
	"eval_context":    true,
	"variant_payload": true,
}

// RedactedValue replaces a PII value at error level, where the presence of the key
// is diagnostically useful even though the value must not be recorded.
const RedactedValue = "[redacted]"

// IsPIIKey reports whether an attribute name is treated as personally identifying.
// Exported so a test can assert the policy rather than infer it.
func IsPIIKey(name string) bool { return piiKeys[strings.ToLower(name)] }

// Options configures a Logger.
type Options struct {
	// Output defaults to os.Stderr.
	Output io.Writer
	// Level defaults to slog.LevelInfo.
	Level slog.Level
	// Service and InstanceID are stamped on every line. InstanceID is what answers
	// "which pod?" when one replica out of forty is misbehaving.
	Service    string
	InstanceID string
	// AllowPII disables redaction. It exists so a local debugging build can be
	// explicit about it; it must never be set in a deployed configuration.
	AllowPII bool
	// Sampler, when non-nil, rate limits high-volume evaluation error logs.
	Sampler *Sampler
}

// Logger is the service's structured logger.
//
// It is a thin wrapper over slog rather than an abstraction over it: the value it
// adds is the redaction handler, automatic trace stamping from context, and the
// fixed evaluation-error schema. Everything else is slog.
type Logger struct {
	l       *slog.Logger
	sampler *Sampler
}

// New builds a JSON logger writing one object per line.
func New(opts Options) *Logger {
	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	h := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: opts.Level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// slog's default key is "time"; the agreed schema says "ts".
			if len(groups) == 0 && a.Key == slog.TimeKey {
				a.Key = "ts"
			}
			return a
		},
	})
	rh := &redactHandler{inner: h, redact: !opts.AllowPII}
	l := slog.New(rh)
	if opts.Service != "" {
		l = l.With(KeyService, opts.Service)
	}
	if opts.InstanceID != "" {
		l = l.With(KeyInstanceID, opts.InstanceID)
	}
	return &Logger{l: l, sampler: opts.Sampler}
}

// NewNop returns a logger that discards everything. Use it in tests and as the
// zero-config default so no call site has to nil-check.
func NewNop() *Logger {
	return &Logger{l: slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 8}))}
}

// Slog exposes the underlying logger for the rare caller that needs slog directly.
func (l *Logger) Slog() *slog.Logger { return l.l }

// With returns a logger with additional permanent attributes.
func (l *Logger) With(args ...any) *Logger {
	return &Logger{l: l.l.With(args...), sampler: l.sampler}
}

// WithTrace pins a trace onto the logger for call sites that do not have a context.
// Prefer passing a context: the handler reads the trace from it automatically.
func (l *Logger) WithTrace(t Trace) *Logger {
	return l.With(KeyTraceID, t.TraceID, KeySpanID, t.SpanID)
}

func (l *Logger) Debug(ctx context.Context, msg string, args ...any) {
	l.l.DebugContext(ctx, msg, args...)
}
func (l *Logger) Info(ctx context.Context, msg string, args ...any) {
	l.l.InfoContext(ctx, msg, args...)
}
func (l *Logger) Warn(ctx context.Context, msg string, args ...any) {
	l.l.WarnContext(ctx, msg, args...)
}
func (l *Logger) Error(ctx context.Context, msg string, args ...any) {
	l.l.ErrorContext(ctx, msg, args...)
}

// EvalError is the fixed schema for an evaluation that could not return a
// configured value. Every field here is one an on-call engineer needs at 3am:
// flag and env say what, reason says why, generation says which config answered,
// trace id joins back to the request, and stack is present only for a panic.
type EvalError struct {
	Flag       string
	Env        string
	Reason     string
	Generation int64
	RuleID     string
	Err        error
	// Stack is set only when this record describes a recovered panic.
	Stack []byte
	// CtxKeys carries the NAMES of the evaluation context attributes present.
	// Never the values -- see the package comment.
	CtxKeys []string
	// ValueSource records what the caller actually received, e.g. "call_site_default".
	ValueSource string
}

// EvaluationError emits one evaluation-error line, subject to sampling.
//
// Sampling is a correctness requirement, not a cost optimisation: one misconfigured
// flag evaluated a million times a second writes a million lines a second and takes
// out the logging pipeline -- a second-order outage strictly worse than the flag bug
// that triggered it. Each emitted line carries sampled_of so true volume stays
// reconstructible.
func (l *Logger) EvaluationError(ctx context.Context, e EvalError) {
	suppressed := 0
	if l.sampler != nil {
		allow, n := l.sampler.Allow(e.Flag + "|" + e.Reason)
		if !allow {
			return
		}
		suppressed = n
	}
	args := make([]any, 0, 20)
	args = append(args,
		KeyEvent, EventEvaluationError,
		KeyFlag, e.Flag,
		KeyEnv, e.Env,
		KeyReason, e.Reason,
		KeyGeneration, e.Generation,
		KeySampledOf, suppressed+1,
	)
	if e.RuleID != "" {
		args = append(args, KeyRuleID, e.RuleID)
	}
	if e.ValueSource != "" {
		args = append(args, KeyValueSource, e.ValueSource)
	}
	if len(e.CtxKeys) > 0 {
		args = append(args, KeyCtxKeys, e.CtxKeys)
	}
	if e.Err != nil {
		args = append(args, KeyError, e.Err.Error())
	}
	if len(e.Stack) > 0 {
		args = append(args, KeyStack, string(e.Stack))
	}
	l.l.ErrorContext(ctx, "flag evaluation returned a fallback value", args...)
}

// Panic records a recovered panic. The stack goes here and only here -- never into
// a response body, where it would hand a caller the service's internal file paths.
func (l *Logger) Panic(ctx context.Context, site string, v any, stack []byte) {
	l.l.ErrorContext(ctx, "recovered panic",
		KeyEvent, EventPanic,
		KeySite, site,
		KeyError, sprint(v),
		KeyStack, string(stack),
	)
}

func sprint(v any) string {
	switch t := v.(type) {
	case error:
		return t.Error()
	case string:
		return t
	default:
		return slog.AnyValue(v).String()
	}
}

// MapKeys returns the sorted names of a map's keys. It is the sanctioned way to
// describe an evaluation context in a log line: names carry the diagnostic value,
// values carry the PII.
func MapKeys[K ~string, V any](m map[K]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// redactHandler is the enforcement point for the no-PII rule.
//
// It is a handler rather than a ReplaceAttr hook because the policy is level
// dependent -- a user id is DROPPED below error level and REDACTED at error level --
// and ReplaceAttr is not given the record's level.
type redactHandler struct {
	inner  slog.Handler
	redact bool
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	if t := TraceFromContext(ctx); t.Valid() {
		out.AddAttrs(slog.String(KeyTraceID, t.TraceID))
		if t.SpanID != "" {
			out.AddAttrs(slog.String(KeySpanID, t.SpanID))
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		if a, ok := h.filter(a, r.Level); ok {
			out.AddAttrs(a)
		}
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactHandler) filter(a slog.Attr, level slog.Level) (slog.Attr, bool) {
	if !h.redact || !IsPIIKey(a.Key) {
		return a, true
	}
	if level < slog.LevelError {
		// Below error, the key is not worth the risk at all. This is the rule that
		// keeps a user id out of the info-level firehose, which is the log stream
		// that gets shipped everywhere and retained longest.
		return slog.Attr{}, false
	}
	return slog.String(a.Key, RedactedValue), true
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Attributes bound here are pre-formatted once and reused across every level,
	// so there is no level to key the policy on. Drop rather than guess.
	kept := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if h.redact && IsPIIKey(a.Key) {
			continue
		}
		kept = append(kept, a)
	}
	return &redactHandler{inner: h.inner.WithAttrs(kept), redact: h.redact}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{inner: h.inner.WithGroup(name), redact: h.redact}
}

// Sampler is a per-key token bucket used to cap evaluation-error log volume.
//
// The key is (flag, reason) -- deliberately not (flag, reason, user) -- so one
// broken flag costs one line per second no matter how many users hit it.
type Sampler struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	maxKeys int
	now     func() time.Time
}

type bucket struct {
	tokens     float64
	last       time.Time
	suppressed int
}

// NewSampler returns a sampler admitting rate lines per second per key with the
// given burst. maxKeys bounds memory: past it, the sampler stops tracking new keys
// and admits them, on the grounds that losing the rate limit is better than an
// unbounded map in the logging path.
func NewSampler(rate, burst float64, maxKeys int) *Sampler {
	if maxKeys <= 0 {
		maxKeys = 4096
	}
	return &Sampler{rate: rate, burst: burst, buckets: make(map[string]*bucket), maxKeys: maxKeys, now: time.Now}
}

// Allow reports whether a line for key may be emitted, and how many lines for that
// key were suppressed since the last admitted one.
func (s *Sampler) Allow(key string) (allow bool, suppressed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	b, ok := s.buckets[key]
	if !ok {
		if len(s.buckets) >= s.maxKeys {
			return true, 0
		}
		b = &bucket{tokens: s.burst, last: now}
		s.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * s.rate
	if b.tokens > s.burst {
		b.tokens = s.burst
	}
	b.last = now
	if b.tokens < 1 {
		b.suppressed++
		return false, 0
	}
	b.tokens--
	n := b.suppressed
	b.suppressed = 0
	return true, n
}

// CallerStack captures the current goroutine's stack for a recovered panic.
//
// It is bounded at 8 KB on purpose. An unbounded stack in a log line is how a
// recursive panic turns a contained incident into a logging-pipeline one.
func CallerStack() []byte {
	buf := make([]byte, 8<<10)
	n := runtime.Stack(buf, false)
	return buf[:n]
}

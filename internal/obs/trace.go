package obs

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Header names understood on ingress and written on egress.
//
// traceparent is preferred when present because it is the interoperable form; the
// bare X-Trace-Id header exists because plenty of internal callers and curl-wielding
// operators will only ever set that one.
const (
	HeaderTraceParent = "traceparent"
	HeaderTraceID     = "X-Trace-Id"
	HeaderRequestID   = "X-Request-Id"
)

// maxIDLen bounds what we will accept from a caller. An unbounded header value
// copied into every log line is a log-volume amplification primitive: one client
// sending a 1 MB X-Trace-Id turns every line it touches into a 1 MB line.
const maxIDLen = 64

// Trace is the correlation identity of one request.
//
// TraceID joins every log line, the error envelope, and any outbound call made
// while serving the request. SpanID identifies this hop specifically.
type Trace struct {
	TraceID string
	SpanID  string
}

// Valid reports whether the trace carries a usable trace id.
func (t Trace) Valid() bool { return t.TraceID != "" }

type ctxKey int

const traceCtxKey ctxKey = iota

// ContextWithTrace returns a context carrying t. Every downstream call, log line,
// and error envelope derives its correlation fields from here rather than from a
// parameter threaded by hand, so that a missed parameter cannot silently drop the
// trace on one branch.
func ContextWithTrace(ctx context.Context, t Trace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, traceCtxKey, t)
}

// ContextWithTraceID is the convenience form for callers that only have an id.
func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	return ContextWithTrace(ctx, Trace{TraceID: traceID, SpanID: NewSpanID()})
}

// TraceFromContext returns the trace carried by ctx, or the zero Trace.
func TraceFromContext(ctx context.Context) Trace {
	if ctx == nil {
		return Trace{}
	}
	t, _ := ctx.Value(traceCtxKey).(Trace)
	return t
}

// TraceIDFromContext returns just the trace id, or "".
func TraceIDFromContext(ctx context.Context) string { return TraceFromContext(ctx).TraceID }

// SpanIDFromContext returns just the span id, or "".
func SpanIDFromContext(ctx context.Context) string { return TraceFromContext(ctx).SpanID }

// NewTraceID mints a 128-bit id rendered as 32 lowercase hex characters, matching
// the W3C trace-context trace-id shape so it can be handed to any tracing backend
// without translation.
func NewTraceID() string { return randomHex(16) }

// NewSpanID mints a 64-bit id rendered as 16 lowercase hex characters.
func NewSpanID() string { return randomHex(8) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any platform we ship to, but a trace id is
		// never worth failing a request over. Degrade to a clock-derived id rather
		// than returning empty, because an empty trace id silently disables
		// correlation for the one request that was interesting enough to hit this.
		binary.BigEndian.PutUint64(b[:min(8, n)], uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b)
}

// TraceIDFromHeaders extracts a caller-supplied trace id.
//
// It returns ok=false for an absent or unusable value so the caller can mint one;
// a malformed inbound id is discarded rather than propagated, because a trace id
// is copied into logs and response bodies and must not carry attacker-chosen bytes.
func TraceIDFromHeaders(h http.Header) (id string, ok bool) {
	if tp := h.Get(HeaderTraceParent); tp != "" {
		if id, ok := traceIDFromTraceParent(tp); ok {
			return id, true
		}
	}
	for _, name := range []string{HeaderTraceID, HeaderRequestID} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			if sanitizeID(v) != "" {
				return sanitizeID(v), true
			}
		}
	}
	return "", false
}

// traceparent is "00-<32 hex trace id>-<16 hex span id>-<2 hex flags>".
func traceIDFromTraceParent(v string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) < 4 {
		return "", false
	}
	id := strings.ToLower(parts[1])
	if len(id) != 32 || !isHex(id) || id == strings.Repeat("0", 32) {
		return "", false
	}
	return id, true
}

// sanitizeID accepts a bounded, log-safe subset and returns "" for anything else.
func sanitizeID(v string) string {
	if len(v) < 4 || len(v) > maxIDLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return ""
		}
	}
	return strings.ToLower(v)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// EnsureTrace returns a context guaranteed to carry a trace, reusing the caller's
// id when it supplied a usable one.
func EnsureTrace(ctx context.Context, h http.Header) (context.Context, Trace) {
	if t := TraceFromContext(ctx); t.Valid() {
		return ctx, t
	}
	id, ok := TraceIDFromHeaders(h)
	if !ok {
		id = NewTraceID()
	}
	t := Trace{TraceID: id, SpanID: NewSpanID()}
	return ContextWithTrace(ctx, t), t
}

// Inject writes the trace carried by ctx into outbound headers. Every client this
// service makes must call it; a hop that does not is where a trace goes dark.
func Inject(ctx context.Context, h http.Header) {
	t := TraceFromContext(ctx)
	if !t.Valid() {
		return
	}
	h.Set(HeaderTraceID, t.TraceID)
	span := t.SpanID
	if len(span) != 16 || !isHex(span) {
		span = NewSpanID()
	}
	if len(t.TraceID) == 32 && isHex(t.TraceID) {
		h.Set(HeaderTraceParent, "00-"+t.TraceID+"-"+span+"-01")
	}
}

// TraceMiddleware establishes the trace for an inbound request and echoes the id
// back to the caller, so an operator holding a curl response can jump straight to
// the log line without guessing.
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, t := EnsureTrace(r.Context(), r.Header)
		w.Header().Set(HeaderTraceID, t.TraceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Package apierr defines the one error envelope every HTTP surface of the flag
// service returns.
//
// It is its own package so that the panic boundary (internal/transport/safe) and the
// HTTP handlers can share a single shape without the boundary importing the handlers.
// One shape matters more than it looks: a caller writing error handling against three
// different JSON shapes writes it against none of them, and falls back to matching on
// status code alone.
package apierr

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/obs"
)

// Code is a stable, machine-readable error classification.
//
// Stable is the operative word. Callers branch on it, alerts group by it, and it
// appears in runbooks. Changing a code is a breaking API change even though nothing
// in the type system says so.
type Code string

const (
	CodeInvalidJSON          Code = "invalid_json"
	CodeInvalidArgument      Code = "invalid_argument"
	CodeNotFound             Code = "not_found"
	CodeMethodNotAllowed     Code = "method_not_allowed"
	CodePayloadTooLarge      Code = "payload_too_large"
	CodeUnsupportedMediaType Code = "unsupported_media_type"
	CodeTooManyFlags         Code = "too_many_flags"
	CodeInvalidConfig        Code = "invalid_config"
	CodeUnavailable          Code = "unavailable"
	CodeTimeout              Code = "timeout"
	CodeInternal             Code = "internal"
)

// Envelope is the wire shape. Field names follow the house standard:
// {error_code, message, trace_id, span_id, timestamp}.
type Envelope struct {
	ErrorCode Code   `json:"error_code"`
	Message   string `json:"message"`
	TraceID   string `json:"trace_id"`
	SpanID    string `json:"span_id,omitempty"`
	Timestamp string `json:"timestamp"`
}

// Status maps a code to its HTTP status.
func Status(c Code) int {
	switch c {
	case CodeInvalidJSON, CodeInvalidArgument, CodeInvalidConfig, CodeTooManyFlags:
		return http.StatusBadRequest
	case CodeNotFound:
		return http.StatusNotFound
	case CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeUnsupportedMediaType:
		return http.StatusUnsupportedMediaType
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// maxMessageLen bounds what reaches a caller. Long messages are almost always a
// wrapped internal error that has accumulated context we did not intend to publish.
const maxMessageLen = 200

// internalMessage is the ONLY thing a caller ever learns about a server-side fault.
// The stack, the file path, the wrapped error text all go to the log, keyed by the
// trace id that is in this envelope. That is the trade: the caller gets a correlation
// handle, not our source tree.
const internalMessage = "internal error"

// Sanitize strips anything that must not cross the trust boundary out of a message.
//
// A raw error string is not safe to return. Go error text routinely carries absolute
// file paths, host names, and internal addresses, and a panic value carries a stack.
// Handing those to a caller is an information disclosure that costs nothing to
// prevent and is impossible to walk back once a client starts parsing it.
func Sanitize(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return internalMessage
	}
	// A stack trace or a source location has leaked in. Do not try to redact it
	// piecewise; replace the whole message.
	lower := strings.ToLower(msg)
	if strings.ContainsAny(msg, "\n\r\t") ||
		strings.Contains(lower, ".go:") ||
		strings.Contains(lower, "goroutine ") ||
		strings.Contains(msg, "/Users/") ||
		strings.Contains(msg, "/home/") ||
		strings.Contains(msg, "0x") {
		return internalMessage
	}
	if len(msg) > maxMessageLen {
		msg = msg[:maxMessageLen]
	}
	return msg
}

// Write renders the envelope with the correct status and content type.
//
// It pulls the trace id from the request context, so no handler has to remember to
// thread it, and echoes it as a header as well as in the body -- the header survives
// a client that only logs response metadata.
func Write(w http.ResponseWriter, r *http.Request, code Code, msg string) {
	var t obs.Trace
	if r != nil {
		t = obs.TraceFromContext(r.Context())
	}
	env := Envelope{
		ErrorCode: code,
		Message:   Sanitize(msg),
		TraceID:   t.TraceID,
		SpanID:    t.SpanID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(env)
	if err != nil {
		// Cannot happen for this struct, but a marshalling failure must not become
		// a second panic inside an error path.
		body = []byte(`{"error_code":"internal","message":"internal error"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	if t.TraceID != "" {
		h.Set(obs.HeaderTraceID, t.TraceID)
	}
	w.WriteHeader(Status(code))
	_, _ = w.Write(body)
}

// WriteInternal is the only way a server-side fault reaches a caller. It exists so
// no handler can accidentally pass err.Error() as the message.
func WriteInternal(w http.ResponseWriter, r *http.Request) {
	Write(w, r, CodeInternal, internalMessage)
}

// TimeoutBody is the static envelope used by http.TimeoutHandler, which writes a
// fixed string and cannot reach the request context for a trace id.
const TimeoutBody = `{"error_code":"timeout","message":"request exceeded server handler deadline","trace_id":"","timestamp":""}`

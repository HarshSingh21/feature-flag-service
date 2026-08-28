package httpx

import (
	"encoding/json"
	"errors"
	"expvar"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/transport/apierr"
)

// routes is the closed set of route labels.
//
// The metric label is derived from this map, never from the raw request path. A raw
// path is attacker-controlled: one client looping over /v1/evaluate/<random> would
// mint a metric series per request and take the metrics backend down, which is a
// worse outage than anything it could do to the flag service itself.
var routes = map[string]string{
	"/v1/evaluate":       "eval",
	"/v1/evaluate/batch": "eval_batch",
	"/v1/config/layers":  "config_apply",
	"/health":            "health",
	"/live":              "live",
	"/ready":             "ready",
	"/debug/vars":        "debug_vars",
}

func routeLabel(path string) string {
	if r, ok := routes[path]; ok {
		return r
	}
	if strings.HasPrefix(path, "/v1/config/snapshot/") {
		return "config_snapshot"
	}
	return "other"
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.deps.Metrics.HTTPRequest(routeLabel(r.URL.Path), r.Method, sw.status, time.Since(start))
	})
}

// decodeJSON reads and decodes a request body under every limit that matters.
//
// The body is capped with MaxBytesReader BEFORE decoding, not after: checking
// Content-Length is not a defence, because a chunked request does not send one.
// DisallowUnknownFields is on so a client that misspells a field learns about it at
// the first request instead of discovering months later that its targeting context
// was silently ignored.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt := strings.TrimSpace(strings.Split(ct, ";")[0])
		if !strings.EqualFold(mt, "application/json") {
			apierr.Write(w, r, apierr.CodeUnsupportedMediaType, "content-type must be application/json")
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apierr.Write(w, r, apierr.CodePayloadTooLarge, "request body exceeds the size limit")
			return false
		}
		// The decoder's message can quote body content, which is caller data we do
		// not want to reflect. Report the class of failure, not the detail; the
		// detail is in the caller's own request.
		apierr.Write(w, r, apierr.CodeInvalidJSON, "request body is not valid JSON for this endpoint")
		return false
	}
	// A second JSON document in the same body means the client is confused about the
	// contract. Accepting it silently would hide that.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		apierr.Write(w, r, apierr.CodeInvalidJSON, "request body must contain exactly one JSON object")
		return false
	}
	return true
}

// writeJSON renders a success body.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		apierr.WriteInternal(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	apierr.Write(w, r, apierr.CodeNotFound, "no such endpoint")
}

func expvarHandler() http.Handler { return expvar.Handler() }

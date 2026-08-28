// Package safe holds the process's panic boundaries.
//
// There are exactly two sanctioned kinds: the per-request transport handler, and any
// goroutine the service starts. A recover() anywhere in the evaluation core would be
// a review reject -- the core is pure and has nothing to recover from.
//
// # The trap
//
// recover() only catches panics on ITS OWN goroutine. A deferred recover in the
// request handler does nothing for a panic raised in a goroutine that handler
// spawned; that panic unwinds its own stack, finds no recovery, and terminates the
// whole process. For this service that means every flag in every environment on this
// replica goes away because one background refresh had a nil map.
//
// That is why Go is the only sanctioned way to start a goroutine here. `go f()`
// written by hand is the bug; it just does not look like one.
//
// # What recover cannot save you from
//
// A concurrent map read and write is a FATAL runtime error, not a panic. recover()
// does not catch it and no boundary in this package will contain it. That is the
// structural argument for the immutable-snapshot-plus-atomic-pointer design: it is
// not a performance preference, it is the removal of a failure mode that no amount
// of defensive coding can contain.
package safe

import (
	"context"
	"net/http"

	"github.com/HarshSingh21/feature-flag-service/internal/obs"
	"github.com/HarshSingh21/feature-flag-service/internal/transport/apierr"
)

// Boundary names. A fixed set, because it is a metric label.
const (
	SiteHTTP      = "http_handler"
	SiteGoroutine = "goroutine"
	SiteEvaluate  = "evaluate"
	SiteCompile   = "snapshot_compile"
)

// PanicFunc is invoked with the recovered value and the stack of the goroutine that
// panicked. It must not itself panic; if it does, the process dies and the whole
// point of the boundary is lost.
type PanicFunc func(ctx context.Context, site string, v any, stack []byte)

// Reporter turns a logger and a metrics recorder into a PanicFunc.
//
// The stack is logged. It is never returned to a caller: a stack trace hands the
// caller our absolute file paths and package layout for free.
func Reporter(log *obs.Logger, rec *obs.Recorder) PanicFunc {
	if log == nil {
		log = obs.NewNop()
	}
	if rec == nil {
		rec = obs.NewRecorder(nil)
	}
	return func(ctx context.Context, site string, v any, stack []byte) {
		rec.Panic(site)
		log.Panic(ctx, site, v, stack)
	}
}

// report is the single place a recovered value is turned into a report. It swallows
// a panic raised by onPanic itself, because a boundary that can be defeated by its
// own error handler is not a boundary.
func report(ctx context.Context, site string, v any, onPanic PanicFunc) {
	if onPanic == nil {
		return
	}
	stack := obs.CallerStack()
	defer func() { _ = recover() }()
	onPanic(ctx, site, v, stack)
}

// Recover is the deferred boundary for an exported entry point:
//
//	defer safe.Recover(ctx, safe.SiteEvaluate, onPanic)
//
// It must be deferred directly. Wrapping it in another closure puts an extra frame
// between recover() and the panicking function and it stops working, silently.
func Recover(ctx context.Context, site string, onPanic PanicFunc) {
	if v := recover(); v != nil {
		report(ctx, site, v, onPanic)
	}
}

// Do runs fn with a panic boundary and reports whether it panicked.
//
// The bool return is the point: the caller decides what a contained panic means.
// For an evaluation it means "return the caller's default with reason ERROR"; for a
// request handler it means "500 with the standard envelope".
func Do(ctx context.Context, site string, onPanic PanicFunc, fn func()) (panicked bool) {
	defer func() {
		if v := recover(); v != nil {
			panicked = true
			report(ctx, site, v, onPanic)
		}
	}()
	fn()
	return false
}

// Go starts a goroutine with its own panic boundary.
//
// This is the ONLY sanctioned way to start a goroutine in this service. A bare
// `go f()` whose f panics kills the process; every flag this replica serves
// disappears because of a bug in a background task that was never on the read path.
// The boundary has to be on the new goroutine, because the parent's deferred recover
// is on a different stack and will never see it.
func Go(ctx context.Context, site string, onPanic PanicFunc, fn func()) {
	go func() {
		defer Recover(ctx, site, onPanic)
		fn()
	}()
}

// GoDone is Go plus a completion signal, for goroutines a shutdown path must wait on.
func GoDone(ctx context.Context, site string, onPanic PanicFunc, fn func()) <-chan struct{} {
	done := make(chan struct{})
	Go(ctx, site, onPanic, func() {
		defer close(done)
		fn()
	})
	return done
}

// Middleware is the per-request panic boundary.
//
// It returns the standard error envelope so a panicking handler is indistinguishable
// to the caller from any other server fault -- same shape, same trace id, no stack.
func Middleware(onPanic PanicFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &recordingWriter{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					// The documented way for a handler to abandon a connection.
					// Re-panic so net/http applies its own semantics.
					panic(v)
				}
				report(r.Context(), SiteHTTP, v, onPanic)
				if rw.wroteHeader {
					// The response is already committed; there is nothing honest
					// left to say on this connection. Dropping it is what tells the
					// client the body is incomplete.
					return
				}
				apierr.WriteInternal(w, r)
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// recordingWriter tracks whether the response has been committed, so the boundary
// does not try to write a 500 over a response that already went out as a 200.
type recordingWriter struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
}

func (w *recordingWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming handlers working through the boundary.
func (w *recordingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets net/http and any other middleware reach the underlying writer.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

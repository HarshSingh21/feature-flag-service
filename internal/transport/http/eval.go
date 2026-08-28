package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/transport/apierr"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/internal/obs"
	"github.com/HarshSingh21/feature-flag-service/internal/transport/safe"
)

// handleEvaluate serves POST /v1/evaluate.
func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	var req EvaluateRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if !s.validateEnv(w, r, req.Environment) {
		return
	}
	if !validFlagKey(req.Flag) {
		writeInvalid(w, r, "flag must be 1-128 characters of [A-Za-z0-9_.-]")
		return
	}
	if req.Default.IsUnknown() {
		// The caller's default is the terminal fallback for every failure path. If
		// the caller does not supply one, there is no safe value to return on a miss
		// and the service would have to invent one -- which is precisely how a kill
		// switch ends up failing open by accident.
		writeInvalid(w, r, "default is required and must be a bool, string, or integer")
		return
	}

	// Pin the snapshot once. Even for a single flag this is a load into a local, so
	// that the response's generation field describes the snapshot that actually
	// answered rather than whatever is live by the time we serialise.
	snap := s.pin(req.Environment)

	res, ok := s.evaluateOne(w, r, snap, req.Flag, req.Environment, req.Context, req.Default)
	if !ok {
		return
	}
	writeJSON(w, r, http.StatusOK, res)
}

// handleEvaluateBatch serves POST /v1/evaluate/batch.
func (s *Server) handleEvaluateBatch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req EvaluateBatchRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if !s.validateEnv(w, r, req.Environment) {
		return
	}
	if len(req.Flags) == 0 {
		writeInvalid(w, r, "flags must contain at least one entry")
		return
	}
	if len(req.Flags) > s.cfg.MaxBatchFlags {
		writeTooManyFlags(w, r, s.cfg.MaxBatchFlags)
		return
	}
	for _, f := range req.Flags {
		if !validFlagKey(f.Flag) {
			writeInvalid(w, r, "every flag must be 1-128 characters of [A-Za-z0-9_.-]")
			return
		}
		if f.Default.IsUnknown() {
			writeInvalid(w, r, "every flag entry requires a default")
			return
		}
	}

	// ---- Invariant CACHE-1 ----
	// ONE load, before the loop, for the whole batch.
	//
	// Pinning per flag would let a config swap land mid-batch and return flag A from
	// generation N and flag B from generation N+1. That produces a cross-flag
	// inconsistency -- "the new checkout was on but the new pricing was off" -- which
	// is essentially impossible to reproduce from a bug report, because by the time
	// anyone looks, both flags agree.
	snap := s.pin(req.Environment)
	gen := generationOf(snap)

	resp := EvaluateBatchResponse{
		Environment: req.Environment,
		Generation:  gen,
		TraceID:     obs.TraceIDFromContext(r.Context()),
		Results:     make([]EvaluateResponse, 0, len(req.Flags)),
	}
	for _, f := range req.Flags {
		res, ok := s.evaluateOne(w, r, snap, f.Flag, req.Environment, req.Context, f.Default)
		if !ok {
			return // evaluateOne already wrote the envelope
		}
		resp.Results = append(resp.Results, res)
	}
	s.deps.Metrics.BatchLatency(time.Since(start))
	writeJSON(w, r, http.StatusOK, resp)
}

// evaluateOne runs the evaluator behind the panic boundary and records the outcome.
//
// It returns ok=false when it has already written an error envelope.
func (s *Server) evaluateOne(w http.ResponseWriter, r *http.Request, snap core.Snapshot, key, env string, ec EvalContextWire, def core.Value) (EvaluateResponse, bool) {
	ctx := r.Context()
	gen := generationOf(snap)

	// A snapshot that has never loaded is not a broken state and not a cache miss.
	// Every lookup against it misses and returns the caller's default, which is the
	// safe value by construction. It is an ANSWER -- FLAG_NOT_FOUND -- which is why
	// that reason must be monitored: a rising rate is how a forgotten config push
	// announces itself. The read path never falls back to the config source.
	if snap == nil {
		res := core.Result{Value: def, Reason: core.ReasonFlagNotFound, Bucket: core.NoBucket}
		return s.finish(r, key, env, ec, res, 0, time.Duration(0)), true
	}

	start := time.Now()
	var res core.Result
	panicked := safe.Do(ctx, safe.SiteEvaluate, s.onPanic, func() {
		res = s.deps.Evaluator.Evaluate(snap, key, ec.toCore(), def.Type(), def)
	})
	elapsed := time.Since(start)

	if panicked {
		// The boundary held: the process is alive and every other request is
		// unaffected. This request gets the standard envelope with its trace id, and
		// nothing else -- the stack went to the log, not to the caller.
		s.deps.Log.EvaluationError(ctx, obs.EvalError{
			Flag:        key,
			Env:         env,
			Reason:      core.ReasonError.String(),
			Generation:  gen,
			CtxKeys:     ec.keys(),
			ValueSource: "call_site_default",
		})
		s.deps.Metrics.Evaluation(key, env, core.ReasonError.String(), true, elapsed)
		apierr.WriteInternal(w, r)
		return EvaluateResponse{}, false
	}
	return s.finish(r, key, env, ec, res, gen, elapsed), true
}

// finish maps a core.Result onto the wire and emits the log line and the metrics.
func (s *Server) finish(r *http.Request, key, env string, ec EvalContextWire, res core.Result, gen int64, elapsed time.Duration) EvaluateResponse {
	reason := res.Reason.String()
	fallback := res.IsFallback()
	s.deps.Metrics.Evaluation(key, env, reason, fallback, elapsed)
	if fallback {
		// Sampled inside the logger: one broken flag at a million evaluations a
		// second must not write a million lines a second.
		s.deps.Log.EvaluationError(r.Context(), obs.EvalError{
			Flag:        key,
			Env:         env,
			Reason:      reason,
			Generation:  gen,
			RuleID:      res.RuleID,
			CtxKeys:     ec.keys(),
			ValueSource: "call_site_default",
		})
	}
	genOut := res.Generation
	if genOut == 0 {
		genOut = gen
	}
	return EvaluateResponse{
		Flag:        key,
		Environment: env,
		Value:       res.Value,
		Reason:      reason,
		RuleID:      res.RuleID,
		Bucket:      res.Bucket,
		Generation:  genOut,
		Fallback:    fallback,
		TraceID:     obs.TraceIDFromContext(r.Context()),
	}
}

// pin performs the one and only snapshot load for a request.
//
// It normalises "no snapshot for this environment" to a nil interface value so that
// every downstream check is a single nil test, rather than a bool that one branch
// forgets to consult.
func (s *Server) pin(env string) core.Snapshot {
	snap, ok := s.deps.Snapshots.Snapshot(env)
	if !ok {
		return nil
	}
	return snap
}

func generationOf(s core.Snapshot) int64 {
	if s == nil {
		return 0
	}
	return s.Generation()
}

// validateEnv rejects an out-of-charset environment before it can become a metric
// label value. See the note on validEnv.
func (s *Server) validateEnv(w http.ResponseWriter, r *http.Request, env string) bool {
	if validEnv(env) {
		return true
	}
	writeInvalid(w, r, "environment must be 1-32 characters of [A-Za-z0-9_.-]")
	return false
}

func writeInvalid(w http.ResponseWriter, r *http.Request, msg string) {
	apierr.Write(w, r, apierr.CodeInvalidArgument, msg)
}

func writeTooManyFlags(w http.ResponseWriter, r *http.Request, max int) {
	apierr.Write(w, r, apierr.CodeTooManyFlags,
		"batch exceeds the maximum of "+strconv.Itoa(max)+" flags")
}

package httpx

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/obs"
	"github.com/HarshSingh21/feature-flag-service/internal/transport/apierr"
)

// handleApplyLayer serves POST /v1/config/layers on the ADMIN listener.
//
// It lives on its own listener and its own http.Server precisely so that a
// config-push storm cannot fill the evaluation listener's accept queue. Evaluation
// is the thing that must never be starved; a delayed config push is a freshness
// problem, a starved evaluation path is an availability one.
func (s *Server) handleApplyLayer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.deps.Applier == nil {
		// Honest 503 rather than a silent 200. Accepting a config push into a void
		// is the worst possible answer: the operator believes the change landed and
		// discovers otherwise during the incident it was meant to fix.
		apierr.Write(w, r, apierr.CodeUnavailable, "config apply is not wired on this instance")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apierr.Write(w, r, apierr.CodePayloadTooLarge, "config layer exceeds the size limit")
			return
		}
		apierr.Write(w, r, apierr.CodeInvalidArgument, "could not read request body")
		return
	}
	if len(body) == 0 {
		apierr.Write(w, r, apierr.CodeInvalidArgument, "config layer body is empty")
		return
	}

	start := time.Now()
	res, err := s.deps.Applier.ApplyLayer(ctx, body)
	elapsed := time.Since(start)

	switch {
	case err == nil:
		s.deps.Metrics.ConfigApply(obs.ApplyOK, elapsed)
		for _, a := range res.Applied {
			s.deps.Metrics.SnapshotGeneration(a.Env, a.Generation)
			s.deps.Metrics.SnapshotFlags(a.Env, a.Flags)
		}
		s.deps.Log.Info(ctx, "config applied",
			obs.KeyEvent, obs.EventConfigApply,
			"applied", res.Applied,
			"flags_changed", res.FlagsChanged,
			"total_ms", elapsed.Milliseconds(),
		)
		writeJSON(w, r, http.StatusOK, res)

	case errors.Is(err, ErrValidation):
		// The operator's mistake, caught before the pointer was ever stored. The
		// live snapshot is untouched. Synchronous 400, counted, never paged.
		s.deps.Metrics.ConfigApply(obs.ApplyRejectedValidation, elapsed)
		s.deps.Log.Warn(ctx, "config rejected by validation",
			obs.KeyEvent, obs.EventConfigApply,
			"result", obs.ApplyRejectedValidation,
			obs.KeyError, err.Error(),
		)
		apierr.Write(w, r, apierr.CodeInvalidConfig, err.Error())

	default:
		// Our fault. This one pages: it means a push that passed validation could
		// not be built, so the fleet is running config the operator believes has
		// been superseded.
		s.deps.Metrics.ConfigApply(obs.ApplyRejectedInternal, elapsed)
		s.deps.Log.Error(ctx, "config apply failed",
			obs.KeyEvent, obs.EventConfigApply,
			"result", obs.ApplyRejectedInternal,
			obs.KeyError, err.Error(),
		)
		apierr.WriteInternal(w, r)
	}
}

// handleSnapshotDebug serves GET /v1/config/snapshot/{env} on the ADMIN listener.
//
// Admin-only because it answers "what is this pod actually serving right now", which
// is the first question in every flag incident and also a description of internal
// state that has no business on a mesh-exposed port.
//
// With ?flag=<key> it dumps the single resolved flag. Without it, the response
// carries counts and -- if the snapshot implements KeyLister -- the key names, but
// never every flag body: an endpoint that serialises the whole corpus is a memory
// spike waiting for the one moment the service is already under stress.
func (s *Server) handleSnapshotDebug(w http.ResponseWriter, r *http.Request) {
	env := r.PathValue("env")
	if !s.validateEnv(w, r, env) {
		return
	}
	snap := s.pin(env)
	if snap == nil {
		apierr.Write(w, r, apierr.CodeNotFound, "no snapshot has been published for this environment")
		return
	}
	resp := SnapshotDebugResponse{
		Environment: snap.Env(),
		Generation:  snap.Generation(),
		Flags:       snap.Len(),
	}
	if kl, ok := snap.(KeyLister); ok {
		resp.Keys = kl.Keys()
	}
	if key := r.URL.Query().Get("flag"); key != "" {
		if !validFlagKey(key) {
			writeInvalid(w, r, "flag must be 1-128 characters of [A-Za-z0-9_.-]")
			return
		}
		f, ok := snap.Flag(key)
		if !ok {
			apierr.Write(w, r, apierr.CodeNotFound, "no such flag in this snapshot")
			return
		}
		resp.Flag = f
	}
	writeJSON(w, r, http.StatusOK, resp)
}

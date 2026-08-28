package httpx

import (
	"net/http"
	"time"
)

// handleLive serves GET /live.
//
// Liveness answers exactly one question: is this process wedged, such that only a
// restart will help? It must never consult config, dependencies, or readiness. A
// liveness probe that fails during a control-plane incident restarts every pod in the
// fleet, discarding the last-known-good config they were successfully serving from --
// turning a freshness problem into a total outage.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]any{"status": "alive"})
}

// handleHealth serves GET /health: the human-facing detail view.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	envs := s.envHealth()
	writeJSON(w, r, http.StatusOK, HealthResponse{
		Status:       "ok",
		Service:      s.cfg.Service,
		InstanceID:   s.cfg.InstanceID,
		UptimeSecond: time.Since(s.started).Seconds(),
		Environments: envs,
	})
}

// handleReady serves GET /ready.
//
// The rule, stated precisely:
//
//	NOT ready  <=>  some required environment has never had a snapshot loaded.
//	READY      <=>  every required environment has a snapshot, however old it is.
//
// The first half is hazard H6: a pod that has never loaded config serves compile-time
// defaults for everything, silently and successfully. Nothing errors, no alert fires,
// and a rolling deploy can quietly put the whole fleet in that state. Readiness is the
// only gate that catches it.
//
// The second half is the one people get wrong. Staleness must NOT fail readiness. A
// pod serving last-known-good config is a working pod: every evaluation returns a real
// configured value, just not the newest one. Refusing its traffic during a
// control-plane incident takes a fleet that was still serving customers and removes it
// from the load balancer -- converting a control-plane incident into a data-plane
// outage, which is strictly worse and entirely self-inflicted.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	envs := s.envHealth()

	ready := true
	reason := ""
	degraded := false
	for _, e := range envs {
		if !e.Loaded {
			ready = false
			reason = "no_snapshot"
		}
		if e.Stale {
			degraded = true
		}
	}
	if len(envs) == 0 {
		ready = false
		reason = "no_snapshot"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, r, status, ReadyResponse{
		Ready:        ready,
		Reason:       reason,
		Degraded:     degraded,
		Environments: envs,
	})
}

// envHealth reports one row per environment that readiness cares about.
//
// When RequiredEnvs is configured, it is authoritative: an environment that is
// required but absent produces an unloaded row, which is what makes it fail
// readiness. Without it, an environment that never loaded is simply invisible and the
// pod reports ready while serving nothing for it.
func (s *Server) envHealth() []EnvHealth {
	names := s.cfg.RequiredEnvs
	if len(names) == 0 {
		names = s.deps.Snapshots.Environments()
	}
	fresh, _ := s.deps.Snapshots.(FreshnessReporter)

	out := make([]EnvHealth, 0, len(names))
	for _, env := range names {
		row := EnvHealth{Env: env, AgeSeconds: -1}
		if snap := s.pin(env); snap != nil {
			row.Loaded = true
			row.Generation = snap.Generation()
			row.Flags = snap.Len()
		}
		if fresh != nil {
			if at, ok := fresh.LastAppliedAt(env); ok {
				age := time.Since(at)
				row.AgeSeconds = age.Seconds()
				row.Stale = age > s.cfg.StaleAfter
			}
		}
		out = append(out, row)
	}
	return out
}

package client

import (
	"context"
	"sync"
	"sync/atomic"
)

// State is the client's lifecycle state, per docs/03-lld.md §6.2.
//
// It is the answer to two operational questions: what does /ready return, and
// what does the flag_client_state gauge report.
type State uint32

const (
	// StateUninitialized is the cold start state: no snapshot has ever been
	// applied, from the network or from L2 disk. Every evaluation returns the
	// caller's default and raises the alarm. This is the only state in which the
	// client is knowingly serving something other than real config, so it must be
	// loud rather than quiet.
	StateUninitialized State = iota

	// StateHealthy means a snapshot is live and the update stream is being heard
	// from within the dead-stream threshold.
	StateHealthy

	// StateDegradedStale means a snapshot is live and serving, but the client is
	// not confident it is current: either it was hydrated from L2 disk at startup
	// and has never been confirmed against the source, or the update stream has
	// been silent past the dead-stream threshold.
	//
	// This is a working state. It serves last-known-good, never blocks, never
	// errors. It is not a reason to refuse traffic.
	StateDegradedStale

	// StateClosed means Close has been called. Evaluations still return caller
	// defaults rather than panicking, because a shutdown race must not take down
	// an in-flight request.
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateUninitialized:
		return "UNINITIALIZED"
	case StateHealthy:
		return "HEALTHY"
	case StateDegradedStale:
		return "DEGRADED_STALE"
	case StateClosed:
		return "CLOSED"
	default:
		return "UNKNOWN"
	}
}

// Serving reports whether the client has real config to evaluate against.
// It is false only in StateUninitialized.
func (s State) Serving() bool { return s == StateHealthy || s == StateDegradedStale }

// stateMachine owns the transitions of docs/03-lld.md §6.2 and nothing else.
//
// The current state is an atomic so the read path and /ready can observe it
// without a lock; the mutex serialises *transitions* only, which happen at
// config-change frequency (order 1/minute), never at evaluation frequency.
type stateMachine struct {
	cur atomic.Uint32

	mu sync.Mutex

	readyOnce sync.Once
	readyCh   chan struct{}

	onChange func(from, to State, reason string)
}

func newStateMachine(onChange func(from, to State, reason string)) *stateMachine {
	if onChange == nil {
		onChange = func(State, State, string) {}
	}
	return &stateMachine{readyCh: make(chan struct{}), onChange: onChange}
}

func (sm *stateMachine) state() State { return State(sm.cur.Load()) }

// ready is closed the first time the client leaves StateUninitialized, whether
// via the network or via L2 disk. It is what WaitForReady selects on.
func (sm *stateMachine) ready() <-chan struct{} { return sm.readyCh }

// set performs a transition, emitting the change hook exactly once per actual
// move. Re-entering the state you are already in is a no-op, not an event: a
// DEGRADED_STALE client that fails to reconnect every 5 seconds must not emit a
// state-change event every 5 seconds.
func (sm *stateMachine) set(to State, reason string) (changed bool) {
	sm.mu.Lock()
	from := State(sm.cur.Load())
	if from == StateClosed || from == to {
		sm.mu.Unlock()
		return false
	}
	sm.cur.Store(uint32(to))
	sm.mu.Unlock()

	if to.Serving() {
		sm.readyOnce.Do(func() { close(sm.readyCh) })
	}
	sm.onChange(from, to, reason)
	return true
}

// onHydratedFromDisk records UNINITIALIZED -> DEGRADED_STALE.
//
// Hydration from L2 is deliberately not HEALTHY. The disk copy is by definition
// unconfirmed — it was written by a previous process life and nothing has
// verified it is still current. Calling that HEALTHY would mean a pod that came
// up during a total flag-service outage reported green while serving config of
// unknown age, which is precisely the silent fail-open of hazard H1.
func (sm *stateMachine) onHydratedFromDisk() {
	sm.set(StateDegradedStale, "l2_hydrated")
}

// onStreamDead records HEALTHY -> DEGRADED_STALE.
func (sm *stateMachine) onStreamDead(reason string) {
	sm.set(StateDegradedStale, reason)
}

// onLiveConfirmation records UNINITIALIZED -> HEALTHY and DEGRADED_STALE ->
// HEALTHY. gen is the generation the source reports as current, which arrives
// either on a snapshot or on a heartbeat.
//
// The recovery condition is "the stream is alive AND the generation we hold is
// not behind the one the source reports". docs/03-lld.md §6.2 words this as
// "generation advanced", and a strict reading — requiring a strictly greater
// generation than the one we entered DEGRADED_STALE holding — is wrong in a way worth spelling out: if the config genuinely did not change
// during the outage, no generation will ever advance, and the pod would sit in
// DEGRADED_STALE alarming forever despite holding byte-current config. That
// turns a recovered incident into a permanent false alarm, which is how alerts
// get muted. So an advanced generation recovers, and so does a live
// confirmation that what we hold is already current.
func (sm *stateMachine) onLiveConfirmation(heldGen, sourceGen int64) {
	if heldGen < sourceGen {
		// We are provably behind. A resync is in flight; stay degraded until it
		// lands, otherwise we would report HEALTHY while knowingly stale.
		return
	}
	sm.set(StateHealthy, "recovered")
}

func (sm *stateMachine) close() { sm.set(StateClosed, "shutdown") }

// waitForReady blocks until the client is serving real config, ctx is done, or
// the client is closed. It is init-time sugar for teams that want a bounded
// wait; nothing in the read path ever calls it.
func (sm *stateMachine) waitForReady(ctx context.Context) bool {
	if sm.state().Serving() {
		return true
	}
	select {
	case <-sm.readyCh:
		return true
	case <-ctx.Done():
		return sm.state().Serving()
	}
}

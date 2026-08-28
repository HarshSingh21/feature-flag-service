package client

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// Update is one frame from the config plane.
//
// A frame carrying a Snapshot is a config change. A frame with a nil Snapshot
// is a heartbeat: it carries no config, only the generation the source
// currently holds. Heartbeats are what make a *silently* dead stream
// detectable — a TCP connection that is open but delivering nothing looks
// identical to a quiet config plane, and "no changes for an hour" is the normal
// case, so silence cannot be the signal.
type Update struct {
	// Snapshot is the new config, or nil for a heartbeat.
	Snapshot core.Snapshot

	// Generation is the source's current generation. On a snapshot frame it
	// equals Snapshot.Generation(); on a heartbeat it is the value to compare
	// against what the client holds.
	Generation int64

	// InstanceID identifies the serving process. A bare generation counter
	// resets on restart, so a client holding generation 900 that meets a
	// restarted source at generation 3 must not conclude it is ahead. When this
	// changes, generation comparison is meaningless and the client resyncs
	// unconditionally.
	InstanceID string
}

// IsHeartbeat reports whether this frame carries liveness only.
func (u Update) IsHeartbeat() bool { return u.Snapshot == nil }

// Source is the config plane as this package consumes it.
//
// It is declared here, where it is used, rather than imported from the
// transport package — which is why this package compiles and tests without
// internal/transport existing. Implementations are expected to be safe for
// concurrent use.
type Source interface {
	// Fetch returns the current state for env. It is used for cold start, for
	// resync after a detected gap, and for the reconcile poll. It must respect
	// ctx cancellation. It is never called from the evaluation path.
	Fetch(ctx context.Context, env string) (Update, error)

	// Subscribe opens a push stream for env. The returned channel delivers
	// snapshot and heartbeat frames and is closed when the stream ends, for any
	// reason. The implementation must not block indefinitely on send; dropping
	// a frame is acceptable because snapshots are absolute state, not deltas.
	Subscribe(ctx context.Context, env string) (<-chan Update, error)
}

// BackoffFunc maps a zero-based attempt number to a reconnect delay.
type BackoffFunc func(attempt int) time.Duration

// FullJitterBackoff is the default reconnect strategy: a uniformly random delay
// in [0, min(maxDelay, base*2^attempt)].
//
// The jitter is the entire point, not a refinement. Unjittered exponential
// backoff is synchronised backoff: when the flag service restarts, forty pods
// that all disconnected within the same millisecond retry at exactly the same
// offsets forever, converting a restart into a self-inflicted thundering herd
// that keeps the service from coming up. Full jitter spreads the fleet across
// the whole window and de-correlates it permanently.
func FullJitterBackoff(base, maxDelay time.Duration) BackoffFunc {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	if maxDelay < base {
		maxDelay = base
	}
	return func(attempt int) time.Duration {
		if attempt < 0 {
			attempt = 0
		}
		if attempt > 30 {
			attempt = 30
		}
		window := base << uint(attempt)
		if window > maxDelay || window <= 0 {
			window = maxDelay
		}
		return time.Duration(rand.Int64N(int64(window) + 1))
	}
}

// updater is the hybrid update consumer of docs/03-lld.md §6: push as the fast
// path, heartbeats to detect a silently dead stream, and a slow reconcile poll
// as the correctness backstop.
//
// The reconcile poll is not redundancy for the push path's *events*; it is
// coverage for the push path's *bugs*. Push handles the event class — a config
// changed — and it handles it in milliseconds. Reconcile handles the class of
// faults where push believes it succeeded and did not: a coalescing bug, a
// dropped frame on a slow consumer, a subscription that silently unbound
// server-side. No amount of push hardening covers that class, because the push
// path is the thing under suspicion. Hence a poll measured in minutes, not
// seconds: it is an auditor, not a data path.
type updater struct {
	c            *Client
	src          Source
	env          string
	deadStream   time.Duration
	reconcile    time.Duration
	fetchTimeout time.Duration
	backoff      BackoffFunc
}

func (u *updater) run(ctx context.Context) {
	if u.reconcile > 0 {
		u.c.wg.Add(1)
		go func() {
			defer u.c.wg.Done()
			u.reconcileLoop(ctx)
		}()
	}

	attempt := 0
	for ctx.Err() == nil {
		ok := u.cycle(ctx)
		if ok {
			attempt = 0
		} else {
			attempt++
		}
		u.c.reportStaleness(ctx)
		d := u.backoff(attempt)
		u.c.logf(ctx, LevelInfo, "flag.client.reconnect", "attempt", attempt, "delay_ms", d.Milliseconds())
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// cycle performs one fetch-then-subscribe pass. It reports whether the pass got
// far enough to count as a successful connection, which is what resets backoff.
func (u *updater) cycle(ctx context.Context) bool {
	// Bootstrap or catch up before subscribing. Doing this first means a client
	// that starts during a push outage still converges as soon as unary fetch
	// works, and a client reconnecting after a long gap does not depend on the
	// source replaying anything.
	fetched := u.fetchOnce(ctx, "bootstrap")

	ch, err := u.src.Subscribe(ctx, u.env)
	if err != nil {
		u.c.metrics.Connected(false)
		u.c.logf(ctx, LevelWarn, "flag.client.subscribe.failed", "err", err)
		return fetched
	}
	u.c.metrics.Connected(true)
	defer u.c.metrics.Connected(false)

	// The dead-stream timer is reset by every frame, snapshot or heartbeat.
	// Expiry means the stream is open but mute, which is indistinguishable from
	// a broken one and must be treated as broken.
	timer := time.NewTimer(u.deadStream)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return true
		case up, open := <-ch:
			if !open {
				u.c.logf(ctx, LevelWarn, "flag.client.stream.closed", "generation", u.c.Generation())
				u.degrade(ctx, "stream_closed")
				return true
			}
			u.onFrame(ctx, up)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(u.deadStream)
		case <-timer.C:
			u.c.metrics.Resync("stream_dead")
			u.c.logf(ctx, LevelWarn, "flag.client.stream.dead",
				"threshold_ms", u.deadStream.Milliseconds(), "generation", u.c.Generation())
			u.degrade(ctx, "stream_dead")
			return true
		}
	}
}

func (u *updater) onFrame(ctx context.Context, up Update) {
	u.c.markLive()

	if up.IsHeartbeat() {
		held := u.c.Generation()
		cur := u.c.cache.load()
		instanceChanged := cur != nil && up.InstanceID != "" && cur.instanceID != "" && cur.instanceID != up.InstanceID
		if instanceChanged || up.Generation > held {
			// The heartbeat says the source has moved on and we did not hear
			// about it: a push frame was lost. Snapshots are absolute state, so
			// the repair is simply to fetch the current one — there is no
			// missed delta to replay and therefore no way to end up partially
			// applied.
			reason := "heartbeat_gap"
			if instanceChanged {
				reason = "instance_changed"
			}
			u.c.metrics.Resync(reason)
			u.c.logf(ctx, LevelWarn, "flag.client.resync",
				"reason", reason, "held_generation", held, "source_generation", up.Generation)
			u.fetchOnce(ctx, reason)
			return
		}
		u.c.sm.onLiveConfirmation(held, up.Generation)
		return
	}
	u.applyUpdate(ctx, up, "push")
}

// fetchOnce performs one unary fetch. It reports whether it succeeded.
func (u *updater) fetchOnce(ctx context.Context, why string) bool {
	fctx, cancel := context.WithTimeout(ctx, u.fetchTimeout)
	defer cancel()
	up, err := u.src.Fetch(fctx, u.env)
	if err != nil {
		u.c.logf(ctx, LevelWarn, "flag.client.fetch.failed", "reason", why, "err", err)
		return false
	}
	u.c.markLive()
	u.applyUpdate(ctx, up, why)
	return true
}

// applyUpdate is the single funnel for every path that can change L1.
//
// The acceptance rule is deliberately narrow: apply when the source instance
// changed, or when the generation is at least what we hold. Refusing a strictly
// older generation stops a late-arriving frame from a slow path (a reconcile
// fetch that raced a push) from rewinding config that has already moved on.
func (u *updater) applyUpdate(ctx context.Context, up Update, via string) {
	if up.Snapshot == nil {
		u.c.sm.onLiveConfirmation(u.c.Generation(), up.Generation)
		return
	}
	gen := up.Snapshot.Generation()
	cur := u.c.cache.load()

	instanceChanged := cur != nil && cur.instanceID != "" && up.InstanceID != "" && cur.instanceID != up.InstanceID
	if cur != nil && !instanceChanged && gen < cur.gen {
		u.c.logf(ctx, LevelInfo, "flag.client.apply.skipped",
			"reason", "older_generation", "held", cur.gen, "offered", gen)
		u.c.sm.onLiveConfirmation(cur.gen, gen)
		return
	}
	if cur != nil && !instanceChanged && gen == cur.gen && !cur.fromDisk {
		// Nothing new. This is the steady state of the reconcile poll and must
		// stay silent, but it *is* a live confirmation that what we hold is
		// current, which is how a DEGRADED_STALE client recovers when the
		// config genuinely did not change during the outage.
		u.c.sm.onLiveConfirmation(cur.gen, gen)
		return
	}
	if cur != nil && !instanceChanged && gen > cur.gen+1 && cur.gen > 0 {
		// We are converging on absolute state, so nothing is lost — but the gap
		// is evidence that a push frame went missing, and that is a fault in
		// the update path worth surfacing rather than swallowing.
		u.c.metrics.Resync("generation_gap")
		u.c.logf(ctx, LevelWarn, "flag.client.generation.gap",
			"held", cur.gen, "applied", gen, "missed", gen-cur.gen-1, "via", via)
	}

	e := &entry{
		snap:       up.Snapshot,
		gen:        gen,
		instanceID: up.InstanceID,
		appliedAt:  u.c.now(),
	}
	u.c.cache.apply(e)
	u.c.metrics.Generation(gen)
	u.c.sm.onLiveConfirmation(gen, up.Generation)
	u.c.logf(ctx, LevelInfo, "flag.client.apply",
		"generation", gen, "flags", up.Snapshot.Len(), "via", via, "env", u.env)
}

func (u *updater) degrade(ctx context.Context, reason string) {
	if u.c.cache.load() == nil {
		// Never initialised: there is nothing stale to serve, so the state is
		// still UNINITIALIZED and the alarm for that is already raised.
		return
	}
	u.c.sm.onStreamDead(reason)
	u.c.reportStaleness(ctx)
}

func (u *updater) reconcileLoop(ctx context.Context) {
	// The first tick is jittered so a fleet that deployed together does not
	// reconcile together. The poll is cheap, but 40 pods issuing it in the same
	// millisecond every 5 minutes is a periodic spike for no reason.
	first := time.Duration(rand.Int64N(int64(u.reconcile) + 1))
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.fetchOnce(ctx, "reconcile")
			t.Reset(u.reconcile)
		}
	}
}

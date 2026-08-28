package e2e

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// readerInterval paces client B's loop.
//
// A genuinely tight loop would record a million observations a second and turn
// the log into the thing under measurement. 200µs keeps the loop hot enough that
// a swap lands inside it many times over while keeping the log to a few thousand
// entries.
const readerInterval = 200 * time.Microsecond

// reader is client B: it evaluates a 100-flag batch in a loop and records every
// answer into the observation log.
//
// It owns exactly one goroutine, bound to the context passed to start, and stop
// does not return until that goroutine has exited. No test can leave it running.
type reader struct {
	cl   *client.Client
	reqs []client.Request
	log  *observationLog

	wg     sync.WaitGroup
	panics atomic.Int64
	loops  atomic.Int64
}

func newReader(cl *client.Client, keys []string) *reader {
	reqs := make([]client.Request, 0, len(keys))
	ec := core.EvalContext{UserID: "user-4711", TenantID: "tenant-9"}
	for _, k := range keys {
		reqs = append(reqs, client.Request{
			Flag: k,
			// The caller default is a token the configuration can never produce,
			// so A1 can tell "B fell back" apart from "B read config".
			Default:     core.String(callerDefault),
			EvalContext: ec,
		})
	}
	return &reader{cl: cl, reqs: reqs, log: newObservationLog()}
}

func (r *reader) start(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		buf := make([]core.Result, len(r.reqs))
		tick := time.NewTicker(readerInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			r.once(ctx, &buf)
		}
	}()
}

// once performs one batch evaluation and records it.
//
// The recover is not decoration. A4 requires zero panics, and the only way to
// assert that rather than assume it is to have a boundary that would count one.
func (r *reader) once(ctx context.Context, buf *[]core.Result) {
	defer func() {
		if rec := recover(); rec != nil {
			r.panics.Add(1)
		}
	}()

	start := time.Now()
	out := r.cl.BatchAppend(ctx, *buf, r.reqs)
	latency := time.Since(start)
	*buf = out
	r.loops.Add(1)

	if len(out) == 0 {
		return
	}
	value, _ := out[0].Value.AsString()
	obs := observation{
		at:         start.Add(latency),
		value:      value,
		generation: out[0].Generation,
		reason:     out[0].Reason,
		latency:    latency,
		state:      r.cl.State(),
	}
	for i := range out {
		if out[i].Generation != obs.generation {
			obs.genSplit = true
		}
		v, _ := out[i].Value.AsString()
		if v != value {
			obs.valueSplit = true
		}
		if out[i].Reason == core.ReasonError {
			obs.errs++
		}
		if out[i].Reason.IsFallback() {
			obs.fallbacks++
		}
	}
	r.log.record(obs)
}

// stop waits for the loop goroutine. The caller must have cancelled its context
// first; stop does not cancel on its own, so a forgotten cancel hangs the test
// visibly rather than leaking a goroutine invisibly.
func (r *reader) stop() { r.wg.Wait() }

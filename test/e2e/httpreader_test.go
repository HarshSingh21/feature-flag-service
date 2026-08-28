package e2e

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// httpBatchReader drives POST /v1/evaluate/batch on the real evaluation
// listener in a loop.
//
// It exists so the mid-batch variant tests BOTH pins. The SDK pins in
// pkg/client.BatchAppend; the service pins in internal/transport/http/eval.go,
// once per request, before the loop over flags. They are separate pieces of
// code that can fail separately, and the transport one is the only one an
// HTTP-only caller depends on.
type httpBatchReader struct {
	h    *harness
	env  string
	keys []string

	wg       sync.WaitGroup
	batches  atomic.Int64
	torn     atomic.Int64
	failures atomic.Int64

	genMu sync.Mutex
	gens  map[int64]struct{}
}

func newHTTPBatchReader(h *harness, env string, keys []string) *httpBatchReader {
	return &httpBatchReader{h: h, env: env, keys: keys, gens: map[int64]struct{}{}}
}

func (r *httpBatchReader) start(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			resp, err := r.h.evaluateBatchOverHTTP(r.env, r.keys)
			if err != nil {
				r.failures.Add(1)
				continue
			}
			r.batches.Add(1)
			r.genMu.Lock()
			r.gens[resp.Generation] = struct{}{}
			r.genMu.Unlock()
			if len(resp.Results) == 0 {
				continue
			}
			// The contract of EvaluateBatchResponse is that the generation is a
			// TOP-LEVEL field because every result came from it. A per-result
			// generation that disagrees, or two different values in one result
			// set, is the tear.
			first, _ := resp.Results[0].Value.AsString()
			for i := range resp.Results {
				v, _ := resp.Results[i].Value.AsString()
				if resp.Results[i].Generation != resp.Generation || v != first {
					r.torn.Add(1)
					break
				}
			}
		}
	}()
}

func (r *httpBatchReader) stop()         { r.wg.Wait() }
func (r *httpBatchReader) count() int64  { return r.batches.Load() }
func (r *httpBatchReader) errors() int64 { return r.failures.Load() }
func (r *httpBatchReader) tears() (int64, int64) {
	return r.torn.Load(), r.batches.Load()
}

// generationsSeen reports how many distinct generations the loop observed. Fewer
// than two means no swap landed while it was running, so a torn batch was never
// even possible and the variant proved nothing.
func (r *httpBatchReader) generationsSeen() int {
	r.genMu.Lock()
	defer r.genMu.Unlock()
	return len(r.gens)
}

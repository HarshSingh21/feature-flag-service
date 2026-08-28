package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// httpSource is client B's config plane, spoken over the REAL admin HTTP API.
//
// # Why it is shaped like this
//
// internal/transport/http exposes no snapshot-download endpoint and no push
// stream. What it does expose is GET /v1/config/snapshot/{env}, which reports
// the live generation, and GET /v1/config/snapshot/{env}?flag=<key>, which
// returns one fully resolved core.Flag. Those two are enough to build a real
// Source over the real transport:
//
//   - Fetch reads the generation, reads every flag, then re-reads the generation
//     and RETRIES if it moved. Without that second read the client could
//     assemble a snapshot from two server generations — a torn read manufactured
//     by the fetcher rather than by the service, which would make A1/A2 test the
//     harness instead of the system.
//
//   - Subscribe cannot be a push stream because the transport has none. It polls
//     the CHEAP generation endpoint and emits heartbeat frames carrying the
//     server's current generation. That is not a workaround bolted on for the
//     test: heartbeat-carries-generation is exactly the frame the SDK's updater
//     is built around, and a heartbeat reporting a generation ahead of the one
//     held triggers the same resync-by-unary-fetch path a dropped push frame
//     would. The convergence delta reported by A5 is therefore an honest
//     measurement of THIS transport, and would only shrink with a real stream.
//
// Every goroutine it starts is owned by the context the SDK passes in and is
// waited for by close().
type httpSource struct {
	admin      string
	keys       []string
	instanceID string
	poll       time.Duration

	hc *http.Client
	wg sync.WaitGroup

	// fetches counts completed unary fetches, so a test can tell a converged
	// client from a lucky one.
	fetches atomic.Int64
}

var _ client.Source = (*httpSource)(nil)

// errNoSnapshot is what the admin API's 404 means: the environment has never
// published. It is a normal cold-start condition, not a fault.
var errNoSnapshot = errors.New("e2e: no snapshot published for environment")

func newHTTPSource(adminURL string, keys []string, instance string, poll time.Duration) *httpSource {
	return &httpSource{
		admin:      adminURL,
		keys:       keys,
		instanceID: instance,
		poll:       poll,
		hc: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 64,
			},
		},
	}
}

// close waits for every goroutine this source started. Call it AFTER
// Client.Close, which cancels the context they are bound to.
func (s *httpSource) close() {
	s.wg.Wait()
	s.hc.CloseIdleConnections()
}

func (s *httpSource) fetchCount() int64 { return s.fetches.Load() }

// Fetch assembles one coherent snapshot from the admin API.
func (s *httpSource) Fetch(ctx context.Context, env string) (client.Update, error) {
	const attempts = 4
	var lastGen int64
	for attempt := 0; attempt < attempts; attempt++ {
		gen, err := s.generation(ctx, env)
		if err != nil {
			return client.Update{}, err
		}
		flags := make([]core.Flag, 0, len(s.keys))
		for _, k := range s.keys {
			f, err := s.flag(ctx, env, k)
			if err != nil {
				return client.Update{}, err
			}
			if f != nil {
				flags = append(flags, *f)
			}
		}
		after, err := s.generation(ctx, env)
		if err != nil {
			return client.Update{}, err
		}
		if after != gen {
			// The server published while we were reading. Discard and retry
			// rather than hand the client a snapshot stitched from two
			// generations.
			lastGen = after
			continue
		}
		s.fetches.Add(1)
		return client.Update{
			Snapshot:   client.NewMemSnapshot(env, gen, flags),
			Generation: gen,
			InstanceID: s.instanceID,
		}, nil
	}
	return client.Update{}, fmt.Errorf("e2e: config kept moving under the fetch (last generation %d)", lastGen)
}

// Subscribe emits a heartbeat every poll interval carrying the server's current
// generation, and closes the channel the moment the service stops answering —
// which is what a broken stream looks like to the SDK.
func (s *httpSource) Subscribe(ctx context.Context, env string) (<-chan client.Update, error) {
	// Fail the subscribe if the service is unreachable right now, so a dead
	// service is a failed connection rather than a stream that goes quiet.
	if _, err := s.generation(ctx, env); err != nil && !errors.Is(err, errNoSnapshot) {
		return nil, err
	}

	ch := make(chan client.Update, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(ch)

		t := time.NewTicker(s.poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			gen, err := s.generation(ctx, env)
			if err != nil {
				if errors.Is(err, errNoSnapshot) {
					continue // nothing published yet; the stream is alive
				}
				return // the stream is dead; closing the channel says so
			}
			frame := client.Update{Generation: gen, InstanceID: s.instanceID}
			// Last-write-wins, exactly like the store's own fan-out: a heartbeat
			// is absolute state, so a superseded one is worthless.
			select {
			case ch <- frame:
			default:
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- frame:
				default:
				}
			}
		}
	}()
	return ch, nil
}

func (s *httpSource) generation(ctx context.Context, env string) (int64, error) {
	var resp httpx.SnapshotDebugResponse
	status, err := s.get(ctx, s.admin+"/v1/config/snapshot/"+env, &resp)
	if err != nil {
		return 0, err
	}
	switch status {
	case http.StatusOK:
		return resp.Generation, nil
	case http.StatusNotFound:
		return 0, errNoSnapshot
	default:
		return 0, fmt.Errorf("e2e: snapshot debug %s: status %d", env, status)
	}
}

func (s *httpSource) flag(ctx context.Context, env, key string) (*core.Flag, error) {
	var resp httpx.SnapshotDebugResponse
	status, err := s.get(ctx, s.admin+"/v1/config/snapshot/"+env+"?flag="+key, &resp)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		return resp.Flag, nil
	case http.StatusNotFound:
		// The flag is not in this snapshot. That is an answer, not an error, and
		// evaluation will report FLAG_NOT_FOUND for it.
		return nil, nil
	default:
		return nil, fmt.Errorf("e2e: snapshot debug %s/%s: status %d", env, key, status)
	}
}

func (s *httpSource) get(ctx context.Context, url string, dst any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusOK && dst != nil {
		if err := json.Unmarshal(raw, dst); err != nil {
			return resp.StatusCode, fmt.Errorf("e2e: decode %s: %w", url, err)
		}
	}
	return resp.StatusCode, nil
}

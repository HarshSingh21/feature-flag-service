package client

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// refEvaluator is the shipped engine. Tests and benchmarks run against the real
// evaluator rather than a stub, because a stub would let the client's contract
// drift from the engine's without anything failing.
func refEvaluator() Evaluator { return core.New() }

// panicEvaluator panics on a named flag and behaves normally otherwise, so a
// test can prove containment without disabling every other assertion.
type panicEvaluator struct {
	on string
}

func (p *panicEvaluator) Evaluate(snap core.Snapshot, key string, ec core.EvalContext, want core.ValueType, def core.Value) core.Result {
	if p.on == "" || key == p.on {
		panic("evaluator exploded on " + key)
	}
	return core.New().Evaluate(snap, key, ec, want, def)
}

// bucketOf is a test-local hash for building keys that need to differ; it is
// deliberately NOT the production bucketing function.
func bucketOf(key string) int32 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return int32((h >> 32) * uint64(core.BucketSpace) >> 32)
}

// --- source ---------------------------------------------------------------

type fakeSub struct {
	ch     chan Update
	mu     sync.Mutex
	closed bool
}

func newFakeSub() *fakeSub { return &fakeSub{ch: make(chan Update, 64)} }

func (s *fakeSub) push(u Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- u:
	default:
	}
}

func (s *fakeSub) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

type fakeSource struct {
	mu         sync.Mutex
	fetch      func() (Update, error)
	fetchCalls int
	subErr     error
	subCalls   int
	sub        *fakeSub
	subReady   chan *fakeSub
}

func newFakeSource() *fakeSource {
	return &fakeSource{subReady: make(chan *fakeSub, 16)}
}

func (s *fakeSource) setFetch(f func() (Update, error)) {
	s.mu.Lock()
	s.fetch = f
	s.mu.Unlock()
}

func (s *fakeSource) Fetch(ctx context.Context, env string) (Update, error) {
	s.mu.Lock()
	s.fetchCalls++
	f := s.fetch
	s.mu.Unlock()
	if f == nil {
		return Update{}, errors.New("fake: no fetch configured")
	}
	return f()
}

func (s *fakeSource) Subscribe(ctx context.Context, env string) (<-chan Update, error) {
	s.mu.Lock()
	s.subCalls++
	err := s.subErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	sub := newFakeSub()
	s.mu.Lock()
	s.sub = sub
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		sub.close()
	}()
	select {
	case s.subReady <- sub:
	default:
	}
	return sub.ch, nil
}

func (s *fakeSource) counts() (fetches, subs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetchCalls, s.subCalls
}

func (s *fakeSource) awaitSub(d time.Duration) *fakeSub {
	select {
	case sub := <-s.subReady:
		return sub
	case <-time.After(d):
		return nil
	}
}

// --- metrics --------------------------------------------------------------

type recMetrics struct {
	NopMetrics
	mu           sync.Mutex
	evals        map[core.Reason]int
	uninit       int
	transitions  []string
	generations  []int64
	resyncs      []string
	l2Errs       int
	l2Ok         int
	connectedSeq []bool
}

func newRecMetrics() *recMetrics {
	return &recMetrics{evals: map[core.Reason]int{}}
}

func (m *recMetrics) Evaluation(_ string, r core.Reason) {
	m.mu.Lock()
	m.evals[r]++
	m.mu.Unlock()
}
func (m *recMetrics) UninitializedEvaluation(string) {
	m.mu.Lock()
	m.uninit++
	m.mu.Unlock()
}
func (m *recMetrics) StateChanged(from, to State) {
	m.mu.Lock()
	m.transitions = append(m.transitions, from.String()+"->"+to.String())
	m.mu.Unlock()
}
func (m *recMetrics) Generation(g int64) {
	m.mu.Lock()
	m.generations = append(m.generations, g)
	m.mu.Unlock()
}
func (m *recMetrics) Connected(c bool) {
	m.mu.Lock()
	m.connectedSeq = append(m.connectedSeq, c)
	m.mu.Unlock()
}
func (m *recMetrics) Resync(reason string) {
	m.mu.Lock()
	m.resyncs = append(m.resyncs, reason)
	m.mu.Unlock()
}
func (m *recMetrics) L2Write(err error) {
	m.mu.Lock()
	if err != nil {
		m.l2Errs++
	} else {
		m.l2Ok++
	}
	m.mu.Unlock()
}

func (m *recMetrics) snapshotTransitions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.transitions...)
}

func (m *recMetrics) snapshotResyncs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.resyncs...)
}

func (m *recMetrics) evalCount(r core.Reason) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.evals[r]
}

func (m *recMetrics) l2Counts() (ok, errs int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.l2Ok, m.l2Errs
}

func (m *recMetrics) uninitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.uninit
}

// --- stores ---------------------------------------------------------------

type memStore struct {
	mu        sync.Mutex
	snap      core.Snapshot
	writtenAt time.Time
	loadErr   error
	saveErr   error
	saves     int
	saved     chan struct{}
}

func newMemStore() *memStore {
	return &memStore{loadErr: ErrNoSnapshot, saved: make(chan struct{}, 64)}
}

func (s *memStore) Load(env string) (core.Snapshot, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, time.Time{}, s.loadErr
	}
	return s.snap, s.writtenAt, nil
}

func (s *memStore) Save(env string, snap core.Snapshot) error {
	s.mu.Lock()
	s.saves++
	err := s.saveErr
	if err == nil {
		s.snap = snap
		s.writtenAt = time.Now()
		s.loadErr = nil
	}
	s.mu.Unlock()
	select {
	case s.saved <- struct{}{}:
	default:
	}
	return err
}

func (s *memStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

func (s *memStore) awaitSave(d time.Duration) bool {
	select {
	case <-s.saved:
		return true
	case <-time.After(d):
		return false
	}
}

// --- fixtures -------------------------------------------------------------

func boolFlag(key string, on bool) core.Flag {
	return core.Flag{Key: key, Type: core.TypeBool, Enabled: true, DefaultValue: core.Bool(on), OffValue: core.Bool(false)}
}

func stringFlag(key, v string) core.Flag {
	return core.Flag{Key: key, Type: core.TypeString, Enabled: true, DefaultValue: core.String(v), OffValue: core.String("")}
}

func intFlag(key string, v int64) core.Flag {
	return core.Flag{Key: key, Type: core.TypeInt, Enabled: true, DefaultValue: core.Int(v), OffValue: core.Int(0)}
}

func fixture(gen int64) *MemSnapshot {
	return NewMemSnapshot("prod", gen, []core.Flag{
		boolFlag("checkout_v2", true),
		stringFlag("theme", "dark"),
		intFlag("max_items", 42),
		{Key: "disabled_flag", Type: core.TypeBool, Enabled: false, DefaultValue: core.Bool(true), OffValue: core.Bool(false)},
		{
			Key: "targeted", Type: core.TypeString, Enabled: true,
			DefaultValue:    core.String("control"),
			OffValue:        core.String(""),
			EvaluationOrder: core.OrderRulesFirst,
			Rules: []core.Rule{{
				ID:         "r-country-in",
				Combiner:   core.LogicAnd,
				Value:      core.String("treatment"),
				Conditions: []core.Condition{{Attribute: "country", Op: core.OpIn, Values: []core.Value{core.String("IN"), core.String("US")}}},
			}},
		},
		{
			Key: "rolled_out", Type: core.TypeBool, Enabled: true,
			DefaultValue: core.Bool(false), OffValue: core.Bool(false),
			Rollout: &core.Rollout{BasisPoints: 5000, OnValue: core.Bool(true), OffValue: core.Bool(false)},
		},
	})
}

// newTestClient builds a client with no source, so nothing runs in the
// background and the cache can be driven directly.
func newTestClient(t interface{ Helper() }, ev Evaluator, opts ...Option) *Client {
	t.Helper()
	c, err := New(append([]Option{WithEnvironment("prod"), WithEvaluator(ev)}, opts...)...)
	if err != nil {
		panic(err)
	}
	return c
}

func applyFixture(c *Client, gen int64) {
	c.cache.apply(&entry{snap: fixture(gen), gen: gen, instanceID: "inst-1", appliedAt: time.Now()})
	c.sm.set(StateHealthy, "test")
}

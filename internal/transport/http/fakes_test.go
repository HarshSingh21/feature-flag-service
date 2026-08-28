package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	httpx "github.com/HarshSingh21/feature-flag-service/internal/transport/http"
)

// fakeSnapshot is an immutable core.Snapshot. The whole HTTP surface is testable
// against it because the transport package declares its collaborators as interfaces
// at the point of use rather than importing the config package.
type fakeSnapshot struct {
	env   string
	gen   int64
	flags map[string]*core.Flag
}

func (s *fakeSnapshot) Generation() int64 { return s.gen }
func (s *fakeSnapshot) Env() string       { return s.env }
func (s *fakeSnapshot) Len() int          { return len(s.flags) }
func (s *fakeSnapshot) Flag(key string) (*core.Flag, bool) {
	f, ok := s.flags[key]
	return f, ok
}

// Keys makes the fake satisfy httpx.KeyLister, as *config.ResolvedSnapshot does.
func (s *fakeSnapshot) Keys() []string {
	out := make([]string, 0, len(s.flags))
	for k := range s.flags {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func snap(env string, gen int64, keys ...string) *fakeSnapshot {
	m := make(map[string]*core.Flag, len(keys))
	for _, k := range keys {
		m[k] = &core.Flag{Key: k, Type: core.TypeBool, Enabled: true, DefaultValue: core.Bool(true)}
	}
	return &fakeSnapshot{env: env, gen: gen, flags: m}
}

// fakeSource records how many times the snapshot pointer was loaded. That count is
// the assertion behind invariant CACHE-1.
type fakeSource struct {
	mu    sync.Mutex
	snaps map[string]core.Snapshot
	loads atomic.Int64
	// bump makes every load return a NEW generation, simulating a config swap
	// landing between loads. A handler that pins per flag fails visibly against it.
	bump bool
	// applied drives the optional FreshnessReporter.
	applied map[string]time.Time
}

func newSource() *fakeSource {
	return &fakeSource{snaps: map[string]core.Snapshot{}, applied: map[string]time.Time{}}
}

func (s *fakeSource) set(env string, sn core.Snapshot) *fakeSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps[env] = sn
	s.applied[env] = time.Now()
	return s
}

func (s *fakeSource) setApplied(env string, at time.Time) *fakeSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied[env] = at
	return s
}

func (s *fakeSource) Snapshot(env string) (core.Snapshot, bool) {
	s.loads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, ok := s.snaps[env]
	if !ok {
		return nil, false
	}
	if s.bump {
		fs := sn.(*fakeSnapshot)
		next := &fakeSnapshot{env: fs.env, gen: fs.gen + 1, flags: fs.flags}
		s.snaps[env] = next
		return next, true
	}
	return sn, true
}

func (s *fakeSource) Environments() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.snaps))
	for k := range s.snaps {
		out = append(out, k)
	}
	return out
}

func (s *fakeSource) LastAppliedAt(env string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.applied[env]
	return at, ok
}

var _ httpx.SnapshotSource = (*fakeSource)(nil)
var _ httpx.FreshnessReporter = (*fakeSource)(nil)

// fakeEvaluator behaves like the real one: it never errors, and reports faults as
// data on Result.Reason.
type fakeEvaluator struct {
	// panicOn makes Evaluate panic for one flag key, to exercise the boundary.
	panicOn string
	// block, when non-nil, holds Evaluate until closed. Used for the drain test.
	block <-chan struct{}
	calls atomic.Int64
}

func (e *fakeEvaluator) Evaluate(sn core.Snapshot, key string, _ core.EvalContext, want core.ValueType, def core.Value) core.Result {
	e.calls.Add(1)
	if e.block != nil {
		<-e.block
	}
	if key == e.panicOn {
		panic(fmt.Sprintf("nil rule set for %s at /Users/secret/path/eval.go:42", key))
	}
	gen := int64(0)
	if sn != nil {
		gen = sn.Generation()
	}
	if sn == nil {
		return core.Result{Value: def, Reason: core.ReasonFlagNotFound, Bucket: core.NoBucket}
	}
	f, ok := sn.Flag(key)
	if !ok {
		return core.Result{Value: def, Reason: core.ReasonFlagNotFound, Bucket: core.NoBucket, Generation: gen}
	}
	if want != core.TypeUnknown && want != f.Type {
		return core.Result{Value: def, Reason: core.ReasonTypeMismatch, Bucket: core.NoBucket, Generation: gen}
	}
	return core.Result{
		Value:      f.DefaultValue,
		Reason:     core.ReasonFallthrough,
		Bucket:     core.NoBucket,
		Generation: gen,
	}
}

var _ httpx.Evaluator = (*fakeEvaluator)(nil)

// fakeApplier stands in for the config package's write path.
type fakeApplier struct {
	res  httpx.ApplyResult
	err  error
	body atomic.Value // []byte
}

func (a *fakeApplier) ApplyLayer(_ context.Context, body []byte) (httpx.ApplyResult, error) {
	a.body.Store(body)
	return a.res, a.err
}

var _ httpx.LayerApplier = (*fakeApplier)(nil)

var errBoom = errors.New("compiler exploded")

package e2e

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// observation is one recorded batch evaluation by client B.
//
// It records the whole batch, not one flag, because that is the unit the
// consistency guarantees are stated over: G1 is about a result SET sharing one
// generation, and the mid-batch tear it forbids is invisible if you only keep
// one element.
type observation struct {
	at         time.Time
	value      string // the token every flag in the batch resolved to
	generation int64  // the generation every flag in the batch was served from
	reason     core.Reason
	latency    time.Duration
	state      client.State

	// genSplit is A3's violation: the batch returned more than one generation.
	genSplit bool
	// valueSplit is the same tear seen through the values.
	valueSplit bool
	// fallbacks counts elements that returned the CALLER's default.
	fallbacks int
	// errs counts elements that returned core.ReasonError.
	errs int
}

// observationLog is the structure every assertion is made against.
//
// §2.4 forbids sleep-then-assert. The replacement is this: B records what it
// actually saw, with timestamps, and the test interrogates the record. A1, A2,
// A6 and A8 are not expressible any other way — "never a third value" and
// "exactly one transition" are properties of a sequence, not of a sample.
type observationLog struct {
	mu  sync.Mutex
	obs []observation
}

func newObservationLog() *observationLog {
	return &observationLog{obs: make([]observation, 0, 8192)}
}

func (l *observationLog) record(o observation) {
	l.mu.Lock()
	l.obs = append(l.obs, o)
	l.mu.Unlock()
}

// all returns a copy, so analysis never races the recorder.
func (l *observationLog) all() []observation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]observation, len(l.obs))
	copy(out, l.obs)
	return out
}

func (l *observationLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.obs)
}

// transition is one value change in the log.
type transition struct {
	idx      int
	from, to string
	at       time.Time
}

func (t transition) String() string { return fmt.Sprintf("%s->%s at index %d", t.from, t.to, t.idx) }

// transitions is A2: the complete list of value changes, in order.
func (l *observationLog) transitions() []transition {
	obs := l.all()
	var out []transition
	for i := 1; i < len(obs); i++ {
		if obs[i].value != obs[i-1].value {
			out = append(out, transition{idx: i, from: obs[i-1].value, to: obs[i].value, at: obs[i].at})
		}
	}
	return out
}

// uniformBefore is A6: everything strictly before idx carries one value.
// It returns that value and whether the segment was uniform.
func (l *observationLog) uniformBefore(idx int) (string, bool) {
	return uniform(l.all(), 0, idx)
}

// uniformFrom is the mirror: everything from idx to the end carries one value.
// It is what proves a converged client did not flap back.
func (l *observationLog) uniformFrom(idx int) (string, bool) {
	obs := l.all()
	return uniform(obs, idx, len(obs))
}

func uniform(obs []observation, lo, hi int) (string, bool) {
	if lo < 0 {
		lo = 0
	}
	if hi > len(obs) {
		hi = len(obs)
	}
	if lo >= hi {
		return "", false
	}
	want := obs[lo].value
	for i := lo + 1; i < hi; i++ {
		if obs[i].value != want {
			return want, false
		}
	}
	return want, true
}

// monotonic is A8: B's generation never regresses.
func (l *observationLog) monotonic() bool {
	obs := l.all()
	for i := 1; i < len(obs); i++ {
		if obs[i].generation < obs[i-1].generation {
			return false
		}
	}
	return true
}

// generations lists the distinct generations in the order they were first seen.
// A9 asserts this has exactly two entries and that the second is the server's
// post-write generation.
func (l *observationLog) generations() []int64 {
	obs := l.all()
	var out []int64
	for i := range obs {
		if len(out) == 0 || out[len(out)-1] != obs[i].generation {
			out = append(out, obs[i].generation)
		}
	}
	return out
}

// values counts how many observations carried each distinct value. A1 asserts
// the key set is a subset of {OLD, NEW}.
func (l *observationLog) values() map[string]int {
	out := map[string]int{}
	for _, o := range l.all() {
		out[o.value]++
	}
	return out
}

// firstWith returns the first observation carrying value, and its index.
func (l *observationLog) firstWith(value string) (observation, int, bool) {
	obs := l.all()
	for i := range obs {
		if obs[i].value == value {
			return obs[i], i, true
		}
	}
	return observation{}, -1, false
}

// has reports whether any observation carries value. It is the convergence
// predicate the poll loops wait on.
func (l *observationLog) has(value string) bool {
	_, _, ok := l.firstWith(value)
	return ok
}

// countFrom counts observations at or after idx.
func (l *observationLog) countFrom(idx int) int {
	n := l.len()
	if idx < 0 || idx >= n {
		return 0
	}
	return n - idx
}

// faults totals everything A4 requires to be zero.
type faults struct {
	errs       int
	fallbacks  int
	genSplits  int
	valueSplit int
}

func (l *observationLog) faults() faults {
	var f faults
	for _, o := range l.all() {
		f.errs += o.errs
		f.fallbacks += o.fallbacks
		if o.genSplit {
			f.genSplits++
		}
		if o.valueSplit {
			f.valueSplit++
		}
	}
	return f
}

func (f faults) zero() bool {
	return f.errs == 0 && f.fallbacks == 0 && f.genSplits == 0 && f.valueSplit == 0
}

func (f faults) String() string {
	return fmt.Sprintf("errors=%d fallbacks=%d generation-split-batches=%d value-split-batches=%d",
		f.errs, f.fallbacks, f.genSplits, f.valueSplit)
}

// p99 over the whole log.
func (l *observationLog) p99() time.Duration { return percentile(latencies(l.all()), 0.99) }

// p99Between is A7's instrument: the same statistic restricted to a window, so
// "during the swap" and "steady state" are comparable numbers rather than
// impressions.
func (l *observationLog) p99Between(from, to time.Time) (time.Duration, int) {
	var lat []time.Duration
	for _, o := range l.all() {
		if o.at.Before(from) || o.at.After(to) {
			continue
		}
		lat = append(lat, o.latency)
	}
	return percentile(lat, 0.99), len(lat)
}

func (l *observationLog) p50Between(from, to time.Time) time.Duration {
	var lat []time.Duration
	for _, o := range l.all() {
		if o.at.Before(from) || o.at.After(to) {
			continue
		}
		lat = append(lat, o.latency)
	}
	return percentile(lat, 0.50)
}

func latencies(obs []observation) []time.Duration {
	out := make([]time.Duration, len(obs))
	for i := range obs {
		out[i] = obs[i].latency
	}
	return out
}

func percentile(in []time.Duration, q float64) time.Duration {
	if len(in) == 0 {
		return 0
	}
	s := make([]time.Duration, len(in))
	copy(s, in)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(q*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

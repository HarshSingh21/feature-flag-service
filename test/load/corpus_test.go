package load

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/config"
	"github.com/HarshSingh21/feature-flag-service/internal/core"
	"github.com/HarshSingh21/feature-flag-service/pkg/client"
)

// ---------------------------------------------------------------------------
// The corpus.
//
// Shape matches pkg/client/bench_test.go's benchSnapshot so the two suites are
// comparable: 20% percentage rollouts, 20% two-rule targeting, 60% plain
// enabled flags. That mix is the point — a corpus of only plain flags measures
// a map lookup and calls it an evaluation.
// ---------------------------------------------------------------------------

const (
	// corpusSize is the flag count of a realistic environment. docs/03-lld.md
	// §4.2 sizes a pod at 5,000; 1,000 is the read-path corpus because the
	// evaluation cost is a function of the FLAG, not of the corpus size (one
	// map lookup either way). L6 measures the 1k/5k/20k memory curve.
	corpusSize = 1000

	// batchFlags is S7's p99 flags-per-request. The whole design turns on this
	// number, so it is a constant with a name, not a literal 100.
	batchFlags = 100

	loadEnv = "prod"
)

var countryList = []core.Value{
	core.String("DE"), core.String("FR"), core.String("GB"), core.String("ES"),
	core.String("IT"), core.String("NL"), core.String("SE"), core.String("IN"),
}

// typicalFlags builds the realistic corpus.
func typicalFlags(n int) []core.Flag {
	flags := make([]core.Flag, 0, n)
	for i := 0; i < n; i++ {
		key := flagKey(i)
		switch i % 5 {
		case 0: // 20%: percentage rollout
			flags = append(flags, core.Flag{
				Key: key, Type: core.TypeBool, Enabled: true,
				DefaultValue: core.Bool(false), OffValue: core.Bool(false),
				Rollout: &core.Rollout{
					BasisPoints: 5000,
					OnValue:     core.Bool(true),
					OffValue:    core.Bool(false),
				},
			})
		case 1: // 20%: two targeting rules
			flags = append(flags, core.Flag{
				Key: key, Type: core.TypeBool, Enabled: true,
				DefaultValue: core.Bool(false), OffValue: core.Bool(false),
				Rules: []core.Rule{
					{ID: "r1", Combiner: core.LogicAnd, Value: core.Bool(true), Conditions: []core.Condition{
						{Attribute: "country", Op: core.OpIn, Values: []core.Value{core.String("DE"), core.String("FR")}},
					}},
					{ID: "r2", Combiner: core.LogicAnd, Value: core.Bool(true), Conditions: []core.Condition{
						{Attribute: "plan", Op: core.OpEquals, Values: []core.Value{core.String("enterprise")}},
						{Attribute: "country", Op: core.OpIn, Values: []core.Value{core.String("IN"), core.String("US")}},
					}},
				},
			})
		default: // 60%: plain enabled bool
			flags = append(flags, core.Flag{
				Key: key, Type: core.TypeBool, Enabled: true,
				DefaultValue: core.Bool(true), OffValue: core.Bool(false),
			})
		}
	}
	return flags
}

func flagKey(i int) string { return "flag-" + strconv.Itoa(i) }

// plainFlagKey returns a key that is guaranteed to be one of the 60% plain
// flags. L1 is specified as the FLOOR cost, and the floor is a flag with no
// rules and no rollout.
func plainFlagKey() string { return flagKey(2) }

// rolloutFlagKey returns a key guaranteed to carry a percentage rollout. Not a
// pass criterion, but the allocation behaviour of this path is the single most
// load-bearing finding in the suite.
func rolloutFlagKey() string { return flagKey(0) }

// ruleFlagKey returns a key guaranteed to carry targeting rules.
func ruleFlagKey() string { return flagKey(1) }

// ---------------------------------------------------------------------------
// L5: the pathological corner. 20 rules x 4 conditions, none of them matching.
//
// Worst case is not "no rule matches"; it is "every rule evaluates every
// condition and then fails". Under an AND combiner the first false condition
// short-circuits, so a rule whose FIRST condition fails costs a quarter of what
// it looks like it costs. Conditions 1-3 are built to match and condition 4 to
// fail, so all 80 conditions are actually evaluated. The rollout then runs,
// because rules-first means a subject that fell through every rule reaches it.
// ---------------------------------------------------------------------------

func pathologicalFlag(key string) core.Flag {
	rules := make([]core.Rule, 0, 20)
	for r := 0; r < 20; r++ {
		rules = append(rules, core.Rule{
			ID:       "rule-" + strconv.Itoa(r),
			Combiner: core.LogicAnd,
			Value:    core.Bool(true),
			Conditions: []core.Condition{
				// Matches. "IN" is deliberately LAST in the list: OpIn is a
				// linear scan and a hit on the first element is not the corner.
				{Attribute: "country", Op: core.OpIn, Values: countryList},
				// Matches.
				{Attribute: "plan", Op: core.OpEquals, Values: []core.Value{core.String("enterprise")}},
				// Matches. Semver is the most expensive operator in the set.
				{Attribute: "app_version", Op: core.OpSemverGreaterThan, Values: []core.Value{core.String("2.0.0")}},
				// FAILS, and only here, so the three above were all paid for.
				{Attribute: "segment", Op: core.OpEquals, Values: []core.Value{core.String("segment-" + strconv.Itoa(r))}},
			},
		})
	}
	return core.Flag{
		Key: key, Type: core.TypeBool, Enabled: true,
		DefaultValue: core.Bool(false), OffValue: core.Bool(false),
		Rules: rules,
		Rollout: &core.Rollout{
			BasisPoints: 5000,
			OnValue:     core.Bool(true),
			OffValue:    core.Bool(false),
		},
		// Required: a flag carrying both rules and a rollout with no explicit
		// order is rejected at config time (decision O2 / finding M09).
		EvaluationOrder: core.OrderRulesFirst,
	}
}

func pathologicalFlags(n int) []core.Flag {
	out := make([]core.Flag, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, pathologicalFlag("worst-"+strconv.Itoa(i)))
	}
	return out
}

// ---------------------------------------------------------------------------
// Evaluation context.
// ---------------------------------------------------------------------------

func loadContext() core.EvalContext {
	return core.EvalContext{
		UserID:   "user-8f3c1d9e-4a2b-11ee-be56-0242ac120002",
		TenantID: "tenant-42",
		Attributes: map[string]core.Value{
			"country":     core.String("IN"),
			"plan":        core.String("enterprise"),
			"app_version": core.String("3.7.1"),
			"segment":     core.String("segment-none"), // matches no pathological rule
		},
	}
}

// ---------------------------------------------------------------------------
// The batch request set: S7's p99 shape.
// ---------------------------------------------------------------------------

func typicalBatch(n int) []client.Request {
	ec := loadContext()
	reqs := make([]client.Request, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, client.Request{
			Flag:        flagKey(i),
			Default:     core.Bool(false),
			EvalContext: ec,
		})
	}
	return reqs
}

func pathologicalBatch(n int) []client.Request {
	ec := loadContext()
	reqs := make([]client.Request, 0, n)
	for i := 0; i < n; i++ {
		reqs = append(reqs, client.Request{
			Flag:        "worst-" + strconv.Itoa(i),
			Default:     core.Bool(false),
			EvalContext: ec,
		})
	}
	return reqs
}

// ---------------------------------------------------------------------------
// The engine.
//
// pkg/client.Evaluator has exactly core.Evaluator's signature, and client.New
// defaults to core.New(), so the client under load runs the SHIPPED engine with
// no adapter anywhere in this suite. That matters for the honesty of every
// number below: a shim between the client and the engine would be code that
// only exists in the benchmark, and any allocation or indirection it introduced
// would be measured as if it were the design's.
//
// newEngine exists for the scenarios that measure the engine directly, beneath
// the client's entry boundary, to separate engine cost from client cost.
// ---------------------------------------------------------------------------

func newEngine() *core.Evaluator { return core.New() }

// ---------------------------------------------------------------------------
// churnSource: the config plane, as pkg/client consumes it.
//
// Real path, not a poke at an unexported field: the client is fed through
// Source.Fetch and Source.Subscribe exactly as it would be by a transport, so
// L4's swaps go through applyUpdate, the generation check and cache.apply —
// the code that actually has to not degrade readers.
// ---------------------------------------------------------------------------

type churnSource struct {
	env        string
	instanceID string

	mu   sync.Mutex
	cur  core.Snapshot
	gen  int64
	ch   chan client.Update
	once sync.Once
	subd chan struct{}
}

func newChurnSource(env string, snap core.Snapshot) *churnSource {
	return &churnSource{
		env:        env,
		instanceID: "load-instance",
		cur:        snap,
		gen:        snap.Generation(),
		ch:         make(chan client.Update, 8),
		subd:       make(chan struct{}),
	}
}

func (s *churnSource) Fetch(ctx context.Context, env string) (client.Update, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return client.Update{Snapshot: s.cur, Generation: s.gen, InstanceID: s.instanceID}, nil
}

func (s *churnSource) Subscribe(ctx context.Context, env string) (<-chan client.Update, error) {
	s.once.Do(func() { close(s.subd) })
	return s.ch, nil
}

// waitSubscribed blocks until the client's updater has opened the stream, so a
// churn writer cannot start pushing into a channel nobody is reading yet.
func (s *churnSource) waitSubscribed(timeout time.Duration) bool {
	select {
	case <-s.subd:
		return true
	case <-time.After(timeout):
		return false
	}
}

// push publishes a new generation. A dropped frame is acceptable by design —
// snapshots are absolute state, not deltas — so the send does not block the
// churn writer's cadence.
func (s *churnSource) push(snap core.Snapshot) bool {
	s.mu.Lock()
	s.cur = snap
	s.gen = snap.Generation()
	up := client.Update{Snapshot: snap, Generation: s.gen, InstanceID: s.instanceID}
	s.mu.Unlock()

	select {
	case s.ch <- up:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// ---------------------------------------------------------------------------
// Client construction.
// ---------------------------------------------------------------------------

// newLoadClient builds a client over snap and blocks until it is serving.
//
// The dead-stream threshold is set to an hour and the reconcile poll disabled
// so that a multi-second benchmark cannot be perturbed by a background resync.
// That is a MEASUREMENT decision, not a recommendation: both are correctness
// machinery and both belong on in production.
func newLoadClient(tb testing.TB, snap core.Snapshot) (*client.Client, *churnSource) {
	tb.Helper()

	src := newChurnSource(loadEnv, snap)
	c, err := client.New(
		client.WithEnvironment(loadEnv),
		client.WithSource(src),
		client.WithDeadStreamThreshold(time.Hour),
		client.WithReconcileInterval(0),
		client.WithFetchTimeout(5*time.Second),
	)
	if err != nil {
		tb.Fatalf("client.New: %v", err)
	}
	tb.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !c.WaitForReady(ctx) {
		tb.Fatal("client never became ready")
	}
	if !src.waitSubscribed(10 * time.Second) {
		tb.Fatal("client never subscribed")
	}
	return c, src
}

func typicalSnapshot(gen int64) core.Snapshot {
	return client.NewMemSnapshot(loadEnv, gen, typicalFlags(corpusSize))
}

// ---------------------------------------------------------------------------
// L6 support: the REAL server-side snapshot, built through the config store.
//
// client.MemSnapshot is what a client holds after decoding a push frame.
// config.ResolvedSnapshot is what the service builds and publishes. They are
// both core.Snapshot and they cost very different amounts of memory, so L6
// measures both rather than picking whichever number flatters the claim.
// ---------------------------------------------------------------------------

func baseLayer(n int) *config.BaseLayer {
	flags := make([]config.BaseFlag, 0, n)
	for i := 0; i < n; i++ {
		bf := config.BaseFlag{
			Key:          flagKey(i),
			Type:         "bool",
			Owner:        "team-load",
			Enabled:      true,
			DefaultValue: core.Bool(true),
			OffValue:     core.Bool(false),
		}
		switch i % 5 {
		case 0:
			bf.DefaultValue = core.Bool(false)
			bf.Rollout = &config.WireRollout{
				BasisPoints: 5000,
				OnValue:     core.Bool(true),
				OffValue:    core.Bool(false),
			}
		case 1:
			bf.DefaultValue = core.Bool(false)
			bf.Rules = []config.WireRule{
				{ID: "r1", Combiner: "and", Value: core.Bool(true), Conditions: []config.WireCondition{
					{Attribute: "country", Op: "in", Values: []core.Value{core.String("DE"), core.String("FR")}},
				}},
				{ID: "r2", Combiner: "and", Value: core.Bool(true), Conditions: []config.WireCondition{
					{Attribute: "plan", Op: "eq", Values: []core.Value{core.String("enterprise")}},
					{Attribute: "country", Op: "in", Values: []core.Value{core.String("IN"), core.String("US")}},
				}},
			}
		}
		flags = append(flags, bf)
	}
	return &config.BaseLayer{SchemaVersion: 1, Flags: flags}
}

// storeWithSnapshot returns a store holding a published ResolvedSnapshot.
func storeWithSnapshot(tb testing.TB, l *config.BaseLayer) *config.Store {
	tb.Helper()
	s := config.New(config.WithEnvironments(loadEnv))
	rep := s.Set(l)
	if err := rep.Err(); err != nil {
		tb.Fatalf("base layer rejected: %v", err)
	}
	if !rep.PublishedIn(loadEnv) {
		tb.Fatalf("base layer accepted but not published in %s", loadEnv)
	}
	return s
}

// storeWithoutSnapshot returns a store holding the SAME cloned layer but no
// environments, and therefore no resolved snapshot.
//
// This is the baseline that isolates the snapshot: Store.Set clones the layer
// it accepts, so a naive heap delta around Set() charges the snapshot for a
// layer clone it does not own. Subtracting this measurement removes the clone
// and leaves the snapshot.
func storeWithoutSnapshot(l *config.BaseLayer) *config.Store {
	s := config.New(config.WithEnvironments())
	s.Set(l)
	return s
}

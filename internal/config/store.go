package config

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// DefaultEnvironments are the environments a Store resolves when none are named.
var DefaultEnvironments = []string{"dev", "staging", "prod"}

// BuildReport is what Set returns. It is never a bare error.
//
// A rejection that is only a non-nil error is a rejection an operator has to guess
// at. Every finding here names the rule, the flag, the layer and the field path,
// and every environment reports what is ACTUALLY serving right now -- because
// "we shipped the fix and it never took effect" is the worst outcome in this
// subsystem and is otherwise invisible.
type BuildReport struct {
	// Global holds findings whose blast radius is every environment. Only the
	// base layer can produce these, because it is the only shared layer.
	Global Findings `json:"global,omitempty"`
	// PerEnv holds one result per environment the build touched.
	PerEnv map[string]EnvResult `json:"per_env"`
}

// EnvResult is one environment's outcome.
type EnvResult struct {
	Env string `json:"env"`
	// Published reports whether a new snapshot was swapped in.
	Published bool `json:"published"`
	// Generation is the generation now serving. Unchanged when Published is false.
	Generation int64 `json:"generation"`
	// PreviousGeneration is what was serving before this build.
	PreviousGeneration int64     `json:"previous_generation"`
	Rejected           Findings  `json:"rejected,omitempty"`
	Quarantined        Findings  `json:"quarantined,omitempty"`
	Warnings           Findings  `json:"warnings,omitempty"`
	QuarantinedFlags   []string  `json:"quarantined_flags,omitempty"`
	BuiltAt            time.Time `json:"built_at"`
}

// Findings flattens every finding in the report.
func (r *BuildReport) Findings() Findings {
	out := append(Findings(nil), r.Global...)
	for _, env := range r.envsSorted() {
		e := r.PerEnv[env]
		out = append(out, e.Rejected...)
		out = append(out, e.Quarantined...)
		out = append(out, e.Warnings...)
	}
	return out
}

// Err returns a non-nil error when anything was rejected, nil otherwise.
func (r *BuildReport) Err() error { return r.Findings().Err() }

// PublishedIn reports whether the given environment swapped in a new snapshot.
func (r *BuildReport) PublishedIn(env string) bool { return r.PerEnv[env].Published }

func (r *BuildReport) envsSorted() []string {
	out := make([]string, 0, len(r.PerEnv))
	for k := range r.PerEnv {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// String renders the report for a log line or a CLI.
func (r *BuildReport) String() string {
	s := fmt.Sprintf("build report: %d global finding(s)", len(r.Global))
	for _, env := range r.envsSorted() {
		e := r.PerEnv[env]
		s += fmt.Sprintf("\n  %s: published=%t gen=%d (was %d) rejected=%d quarantined=%d warnings=%d",
			env, e.Published, e.Generation, e.PreviousGeneration, len(e.Rejected), len(e.Quarantined), len(e.Warnings))
	}
	return s
}

// -----------------------------------------------------------------------------
// Store
// -----------------------------------------------------------------------------

// envState is one environment's publication slot.
//
// cur is an atomic.Pointer rather than a map behind an RWMutex for two reasons.
// The weak one is that a write under RWMutex stalls every in-flight reader. The
// decisive one is that a concurrent map read and write in Go is a FATAL runtime
// error that recover cannot catch -- it would take the process down and break the
// never-throw contract outright, so the design must make it structurally
// impossible rather than merely unlikely.
type envState struct {
	cur atomic.Pointer[ResolvedSnapshot]

	// gen is written only under Store.mu, by the single build goroutine.
	gen int64

	subMu  sync.Mutex
	subs   map[int64]chan *ResolvedSnapshot
	nextID int64
}

type envTable map[string]*envState

// Store is the in-memory configuration store.
//
// It holds the RAW unmerged layers for forensics -- "what exactly did CI push?"
// is unanswerable from merged output -- plus one resolved snapshot per
// environment. Publication is a single atomic pointer swap per environment, and
// it is the LAST stage of every build: build fully, validate fully, then swap.
// A rejected config is a no-op on the snapshot, not a flush, so the
// last-known-good keeps serving.
type Store struct {
	// mu serialises writers only. Readers never take it.
	mu       sync.Mutex
	base     *BaseLayer
	overlays map[string]*OverlayLayer
	ops      map[string]*OpsLayer

	// lastRejected keeps the most recent layer that failed to be accepted, for
	// forensics. It is deliberately NOT merged into anything.
	lastRejected map[string]Layer

	// envs is copy-on-write so Get needs no lock at all.
	envs atomic.Pointer[envTable]

	now func() time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithClock replaces the build clock. TTLs and build timestamps read from it.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// WithEnvironments names the environments the store resolves. A base-layer write
// rebuilds all of them; an overlay or ops write for an unknown environment
// creates it.
func WithEnvironments(envs ...string) Option {
	return func(s *Store) {
		tbl := make(envTable, len(envs))
		for _, e := range envs {
			if e == "" {
				continue
			}
			tbl[e] = &envState{}
		}
		s.envs.Store(&tbl)
	}
}

// New builds an empty store. Until a base layer is accepted, Get misses for every
// flag and the caller falls back to its own compiled-in L0 default.
func New(opts ...Option) *Store {
	s := &Store{
		overlays:     make(map[string]*OverlayLayer),
		ops:          make(map[string]*OpsLayer),
		lastRejected: make(map[string]Layer),
		now:          time.Now,
	}
	WithEnvironments(DefaultEnvironments...)(s)
	for _, o := range opts {
		o(s)
	}
	return s
}

// Get resolves one flag in one environment.
//
// One atomic load, one map index, one map lookup. No merge, no validation, no
// lock, no allocation, no I/O. A miss is an answer, not a cache miss to fill:
// the caller applies its L0 default and the FLAG_NOT_FOUND rate is monitored,
// which is how a forgotten config push announces itself.
func (s *Store) Get(flagName, env string) (*core.Flag, bool) {
	tbl := s.envs.Load()
	if tbl == nil {
		return nil, false
	}
	st := (*tbl)[env]
	if st == nil {
		return nil, false
	}
	snap := st.cur.Load()
	if snap == nil {
		return nil, false
	}
	return snap.Flag(flagName)
}

// Snapshot returns the environment's current snapshot. The evaluator should call
// this ONCE per request and use the returned pointer for every flag in it, so a
// config swap landing mid-request cannot serve flag A from generation N and flag
// B from generation N+1.
func (s *Store) Snapshot(env string) (*ResolvedSnapshot, bool) {
	tbl := s.envs.Load()
	if tbl == nil {
		return nil, false
	}
	st := (*tbl)[env]
	if st == nil {
		return nil, false
	}
	snap := st.cur.Load()
	return snap, snap != nil
}

// Provenance reports which layer supplied each field of a resolved flag. This is
// the debug-endpoint entry point: during an incident the first question is "what
// did BASE say versus the prod overlay", and a merged object alone cannot answer
// it. Lock-free, like Get.
func (s *Store) Provenance(flagName, env string) (FlagProvenance, bool) {
	snap, ok := s.Snapshot(env)
	if !ok {
		return FlagProvenance{}, false
	}
	return snap.Provenance(flagName)
}

// Environments lists the environments the store knows about, sorted.
func (s *Store) Environments() []string {
	tbl := s.envs.Load()
	if tbl == nil {
		return nil
	}
	out := make([]string, 0, len(*tbl))
	for k := range *tbl {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Set installs a layer and rebuilds every environment it affects.
//
// A base write rebuilds all environments, because the base is shared. An overlay
// or ops write rebuilds only its own environment: per-environment
// transactionality is the point, so a typo in the prod overlay cannot block an
// urgent dev fix. Global atomicity would buy "all environments agree", which is
// worthless when environments are supposed to differ.
func (s *Store) Set(layer Layer) *BuildReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	report := &BuildReport{PerEnv: make(map[string]EnvResult)}
	now := s.now()

	switch l := layer.(type) {
	case *BaseLayer:
		findings := ValidateBase(l)
		if findings.HasRejections() {
			// The base is the only global blast radius in the system, so it gets
			// the strictest posture: nothing publishes anywhere and every
			// environment keeps its last-known-good snapshot. The rejected layer
			// is retained for forensics but is never merged.
			report.Global = findings
			s.lastRejected["base"] = l.CloneLayer()
			for _, env := range s.Environments() {
				report.PerEnv[env] = s.envStatusLocked(env, findings.Warns())
			}
			return report
		}
		report.Global = findings.Warns()
		s.base = l.Clone()
		for _, env := range s.Environments() {
			report.PerEnv[env] = s.buildEnvLocked(env, now)
		}

	case *OverlayLayer:
		if l.Environment == "" {
			report.Global = ValidateOverlay(l).Rejections()
			s.lastRejected["overlay:"] = l.CloneLayer()
			return report
		}
		s.overlays[l.Environment] = l.Clone()
		s.ensureEnvLocked(l.Environment)
		report.PerEnv[l.Environment] = s.buildEnvLocked(l.Environment, now)

	case *OpsLayer:
		if l.Environment == "" {
			report.Global = ValidateOps(l, now).Rejections()
			s.lastRejected["ops:"] = l.CloneLayer()
			return report
		}
		s.ops[l.Environment] = l.Clone()
		s.ensureEnvLocked(l.Environment)
		report.PerEnv[l.Environment] = s.buildEnvLocked(l.Environment, now)

	default:
		report.Global = Findings{{RuleID: "E00", Layer: LayerNone,
			Message: fmt.Sprintf("unknown layer type %T", layer), Severity: SeverityRejectGlobal}}
	}
	return report
}

// Rebuild re-resolves every environment without changing any layer. Useful after
// wall-clock time has moved an ops override past its TTL.
func (s *Store) Rebuild() *BuildReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	report := &BuildReport{PerEnv: make(map[string]EnvResult)}
	for _, env := range s.Environments() {
		report.PerEnv[env] = s.buildEnvLocked(env, now)
	}
	return report
}

// RawLayers returns deep copies of the raw, unmerged layers currently held.
//
// Merged output cannot answer "what did CI actually push?", which is the first
// forensic question after a bad config change. Copies, not the originals: a debug
// endpoint must not be able to mutate live config state.
func (s *Store) RawLayers() (*BaseLayer, map[string]*OverlayLayer, map[string]*OpsLayer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	overlays := make(map[string]*OverlayLayer, len(s.overlays))
	for k, v := range s.overlays {
		overlays[k] = v.Clone()
	}
	ops := make(map[string]*OpsLayer, len(s.ops))
	for k, v := range s.ops {
		ops[k] = v.Clone()
	}
	return s.base.Clone(), overlays, ops
}

// LastRejected returns the most recent layer that was refused, for forensics.
// Key is "base", "overlay:<env>" or "ops:<env>".
func (s *Store) LastRejected(key string) (Layer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lastRejected[key]
	if !ok {
		return nil, false
	}
	return l.CloneLayer(), true
}

// -----------------------------------------------------------------------------
// Build
// -----------------------------------------------------------------------------

func (s *Store) ensureEnvLocked(env string) *envState {
	tbl := s.envs.Load()
	if tbl != nil {
		if st, ok := (*tbl)[env]; ok {
			return st
		}
	}
	next := make(envTable)
	if tbl != nil {
		for k, v := range *tbl {
			next[k] = v
		}
	}
	st := &envState{}
	next[env] = st
	s.envs.Store(&next)
	return st
}

// envStatusLocked reports an environment's current state without rebuilding it.
func (s *Store) envStatusLocked(env string, warns Findings) EnvResult {
	st := s.ensureEnvLocked(env)
	res := EnvResult{Env: env, Warnings: warns}
	if cur := st.cur.Load(); cur != nil {
		res.Generation = cur.Generation()
		res.PreviousGeneration = cur.Generation()
		res.BuiltAt = cur.BuiltAt()
	}
	return res
}

// buildEnvLocked resolves one environment end to end: merge, validate, freeze,
// then -- and only then -- swap the pointer. A failed build simply never reaches
// the swap, so the fail-safe property is structural rather than a compensating
// code path that has to be correct under stress.
func (s *Store) buildEnvLocked(env string, now time.Time) EnvResult {
	st := s.ensureEnvLocked(env)
	prev := st.cur.Load()

	res := EnvResult{Env: env, BuiltAt: now}
	if prev != nil {
		res.PreviousGeneration = prev.Generation()
		res.Generation = prev.Generation()
	}

	if s.base == nil {
		res.Rejected = append(res.Rejected, Finding{RuleID: "E01", Env: env, Layer: LayerBase,
			Message: "no base layer has been accepted; nothing is servable", Severity: SeverityRejectEnv})
		return res
	}

	overlay := s.overlays[env]
	opsLayer := s.ops[env]

	// Pre-merge validation is re-run at build time, not cached from Set time:
	// TTL rules are clock-dependent, so an ops override must be re-judged on every
	// build. That is what lets a kill switch expire on its own.
	ovFindings := ValidateOverlay(overlay)
	opsFindings := ValidateOps(opsLayer, now)

	ovByKey := make(map[string]*OverlayFlag)
	if overlay != nil {
		for i := range overlay.Flags {
			ovByKey[overlay.Flags[i].Key] = &overlay.Flags[i]
		}
	}
	opsByKey := make(map[string]*OpsOverride)
	if opsLayer != nil {
		for i := range opsLayer.Overrides {
			o := &opsLayer.Overrides[i]
			if opsExpired(o, now) {
				continue // M11: self-healing, the expired entry is dropped
			}
			opsByKey[o.Key] = o
		}
	}

	// Layer-scoped rejections (a nameless environment, for instance) stop the
	// environment before anything is merged.
	for _, f := range ovFindings.Rejections() {
		if f.Severity >= SeverityRejectEnv {
			res.Rejected = append(res.Rejected, f)
		}
	}
	for _, f := range opsFindings.Rejections() {
		if f.Severity >= SeverityRejectEnv {
			res.Rejected = append(res.Rejected, f)
		}
	}
	if len(res.Rejected) > 0 {
		return res
	}

	b := newSnapshotBuilder(env, len(s.base.Flags))
	var quarantined Findings
	var warnings Findings

	warnings = append(warnings, ovFindings.Warns()...)
	warnings = append(warnings, opsFindings.Warns()...)

	// carry moves a flag's last-known-good version into the new generation. A
	// quarantined flag with no previous version is simply absent, and evaluation
	// falls through to the caller's L0 default plus a structured error log.
	carry := func(key string) {
		if prev != nil {
			if f, ok := prev.Flag(key); ok {
				p, _ := prev.Provenance(key)
				b.addQuarantined(cloneFlag(f), p)
				return
			}
		}
		b.markQuarantined(key)
	}

	for i := range s.base.Flags {
		bf := &s.base.Flags[i]

		pre := append(Findings(nil), ovFindings.ForFlag(bf.Key).Rejections()...)
		pre = append(pre, opsFindings.ForFlag(bf.Key).Rejections()...)
		if len(pre) > 0 {
			quarantined = append(quarantined, pre...)
			carry(bf.Key)
			continue
		}

		flag, prov := mergeFlag(bf, ovByKey[bf.Key], opsByKey[bf.Key])
		post := validateResolved(env, flag, bf, ovByKey[bf.Key], opsByKey[bf.Key], prov)
		warnings = append(warnings, post.Warns()...)
		if rej := post.Rejections(); len(rej) > 0 {
			quarantined = append(quarantined, rej...)
			carry(bf.Key)
			continue
		}
		b.add(flag, prov)
	}

	// M02 -- orphans: an overlay or ops entry naming a flag with no base entry.
	baseKeys := make(map[string]struct{}, len(s.base.Flags))
	for i := range s.base.Flags {
		baseKeys[s.base.Flags[i].Key] = struct{}{}
	}
	orphans := make(map[string]LayerID)
	for k := range ovByKey {
		if _, ok := baseKeys[k]; !ok {
			orphans[k] = LayerOverlay
		}
	}
	for k := range opsByKey {
		if _, ok := baseKeys[k]; !ok {
			if _, dup := orphans[k]; !dup {
				orphans[k] = LayerOps
			}
		}
	}
	for _, k := range sortedMapKeys(orphans) {
		quarantined = append(quarantined, orphanFinding(env, k, orphans[k]))
		carry(k)
	}

	b.warn(warnings)

	// Environment-scoped guards, evaluated on the finished build.
	if b.len() > MaxFlagsPerEnv {
		res.Rejected = append(res.Rejected, Finding{RuleID: "M13", Env: env, Layer: LayerBase,
			Message:  fmt.Sprintf("resolved flag count %d exceeds the %d per-environment limit", b.len(), MaxFlagsPerEnv),
			Severity: SeverityRejectEnv})
	}
	budget := QuarantineBudget(len(s.base.Flags))
	if b.quarantineCount() > budget {
		// The safety valve. Mass quarantine means the input is systematically
		// broken rather than typo'd, and partially applying a systematically
		// broken input is how you get a half-configured production.
		res.Rejected = append(res.Rejected, Finding{RuleID: "M15", Env: env, Layer: LayerNone,
			Message: fmt.Sprintf("%d quarantined flags exceed the budget of %d (max(%d, %.0f%%) of %d); the input is systematically broken",
				b.quarantineCount(), budget, QuarantineFloor, QuarantineFraction*100, len(s.base.Flags)),
			Severity: SeverityRejectEnv})
	}

	res.Quarantined = quarantined
	res.Warnings = warnings
	if len(res.Rejected) > 0 {
		// Nothing is published. Readers keep loading the same pointer they were
		// already loading; the generation does not move.
		return res
	}

	// ---- Publication. The last stage, and a single atomic swap. --------------
	st.gen++
	snap := b.build(st.gen, now)
	st.cur.Store(snap)

	res.Published = true
	res.Generation = snap.Generation()
	res.QuarantinedFlags = snap.QuarantinedKeys()

	st.notify(snap)
	return res
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// -----------------------------------------------------------------------------
// Subscriptions
// -----------------------------------------------------------------------------

// Subscribe returns a channel that receives every newly published snapshot for
// one environment, plus the current snapshot immediately if one exists. Call the
// returned cancel exactly when you are done; it is idempotent and closes the
// channel.
//
// Drop policy: the channel has a buffer of one, and the publisher never blocks.
// If a subscriber has not drained the previous snapshot, the publisher REPLACES
// it with the newer one. A slow subscriber therefore misses intermediate
// generations but always converges on the latest, because a snapshot is absolute
// state and not a delta -- there is no history to replay and nothing to
// reconstruct. Blocking here instead would let one slow client stall config
// propagation for every other client, which is a far worse failure.
func (s *Store) Subscribe(env string) (<-chan *ResolvedSnapshot, func()) {
	s.mu.Lock()
	st := s.ensureEnvLocked(env)
	s.mu.Unlock()
	return st.subscribe()
}

func (e *envState) subscribe() (<-chan *ResolvedSnapshot, func()) {
	ch := make(chan *ResolvedSnapshot, 1)

	e.subMu.Lock()
	if e.subs == nil {
		e.subs = make(map[int64]chan *ResolvedSnapshot)
	}
	id := e.nextID
	e.nextID++
	e.subs[id] = ch
	if cur := e.cur.Load(); cur != nil {
		ch <- cur // buffer is empty and ours alone; cannot block
	}
	e.subMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			e.subMu.Lock()
			if c, ok := e.subs[id]; ok {
				delete(e.subs, id)
				close(c)
			}
			e.subMu.Unlock()
		})
	}
	return ch, cancel
}

// notify fans a published snapshot out to subscribers without ever blocking.
//
// Publishes are serialised by Store.mu, so this is the only goroutine filling any
// of these buffers: after the drain the send always succeeds, which is what makes
// "every live subscriber ends up holding the latest snapshot" a guarantee rather
// than a hope.
func (e *envState) notify(snap *ResolvedSnapshot) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for _, ch := range e.subs {
		select {
		case ch <- snap:
		default:
			select {
			case <-ch: // discard the superseded snapshot
			default:
			}
			select {
			case ch <- snap:
			default:
			}
		}
	}
}

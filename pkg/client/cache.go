package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshSingh21/feature-flag-service/internal/core"
)

// entry is one published, immutable view of the world: the snapshot plus the
// identity metadata that makes it interpretable.
//
// The snapshot and its generation, source instance and apply time are pinned
// together in a single allocation and published with one atomic store, so a
// reader that loads the pointer gets a coherent set. Publishing them as
// separate atomics would let a reader see generation N+1 next to snapshot N.
//
// Nothing in here is mutated after publication (invariant CACHE-2).
type entry struct {
	snap       core.Snapshot
	gen        int64
	instanceID string
	appliedAt  time.Time
	// fromDisk marks an entry hydrated from L2, so the updater knows this
	// generation has never been confirmed against the source.
	fromDisk bool
}

// cache is the L1/L2 hierarchy of docs/03-lld.md §3.1.
//
// L1 is an atomic pointer, not a mutex around a mutable map, and that choice is
// load bearing rather than a micro-optimisation. A concurrent map read/write in
// Go is a *fatal runtime throw* that recover cannot catch — it terminates the
// process. A mutable shared map would therefore make the never-throw contract
// unenforceable by construction, no matter how careful the locking looked in
// review. Build-then-swap removes the failure mode instead of guarding it.
type cache struct {
	l1 atomic.Pointer[entry]

	l2      SnapshotStore
	writer  *asyncWriter
	env     string
	onWrite func(error) // observability hook for L2 write failures
}

func newCache(env string, l2 SnapshotStore, onWrite func(error)) *cache {
	c := &cache{l2: l2, env: env, onWrite: onWrite}
	if l2 != nil {
		c.writer = newAsyncWriter(func(e *entry) {
			err := l2.Save(env, e.snap)
			if c.onWrite != nil {
				c.onWrite(err)
			}
		})
	}
	return c
}

// load is the entire read path's interaction with the cache: one atomic load,
// no lock, no branch on freshness. A nil return means StateUninitialized.
//
// There is deliberately no age check here. Expiring a snapshot converts a
// freshness problem into an availability outage across the whole fleet at once
// (docs/03-lld.md §6.3), so staleness is reported, never enforced.
func (c *cache) load() *entry { return c.l1.Load() }

// apply publishes a new snapshot and then, asynchronously, persists it to L2.
//
// Ordering is the point. The atomic swap happens first and the disk write is
// queued after it, because disk latency must never sit between a validated
// snapshot and it becoming live. A failed L2 write degrades cold-start recovery
// on the next restart; it does not fail the apply and it does not roll back the
// swap. L2 is a restart optimisation, not a source of truth.
func (c *cache) apply(e *entry) {
	c.l1.Store(e)
	if c.writer != nil && !e.fromDisk {
		c.writer.enqueue(e)
	}
}

// hydrate publishes an entry read from L2 without writing it back out.
func (c *cache) hydrate(e *entry) { c.l1.Store(e) }

func (c *cache) close() {
	if c.writer != nil {
		c.writer.close()
	}
}

// asyncWriter serialises L2 writes onto one goroutine with a single-slot,
// last-write-wins mailbox.
//
// Two properties matter. It never blocks the caller — apply returns as soon as
// the pointer is swapped. And it coalesces: a burst of ten config changes in
// two seconds writes the final snapshot once or twice rather than ten times,
// because only the newest pending entry is kept. Spawning a goroutine per apply
// would give neither property and would let a slow disk accumulate goroutines
// holding whole snapshots alive.
type asyncWriter struct {
	mu      sync.Mutex
	pending *entry
	sig     chan struct{}
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
	write   func(*entry)
}

func newAsyncWriter(write func(*entry)) *asyncWriter {
	w := &asyncWriter{
		sig:   make(chan struct{}, 1),
		done:  make(chan struct{}),
		write: write,
	}
	w.wg.Add(1)
	go w.run()
	return w
}

func (w *asyncWriter) enqueue(e *entry) {
	w.mu.Lock()
	w.pending = e
	w.mu.Unlock()
	select {
	case w.sig <- struct{}{}:
	default: // a wake-up is already queued; the writer will pick up the newest
	}
}

func (w *asyncWriter) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.sig:
			w.drain()
		case <-w.done:
			w.drain() // best effort flush of the last snapshot on shutdown
			return
		}
	}
}

func (w *asyncWriter) drain() {
	w.mu.Lock()
	e := w.pending
	w.pending = nil
	w.mu.Unlock()
	if e == nil {
		return
	}
	// A panic in a user-supplied store must not take the process down with it.
	defer func() { _ = recover() }()
	w.write(e)
}

func (w *asyncWriter) close() {
	w.once.Do(func() { close(w.done) })
	w.wg.Wait()
}

// SnapshotStore is the L2 last-known-good tier. It is an interface so a
// deployment can back it with something other than a file — a tmpfs path, a
// sidecar, a test double — without this package growing a filesystem opinion.
//
// Implementations may block; they are only ever called off the read path.
type SnapshotStore interface {
	// Load returns the persisted snapshot for env and the time it was written.
	// A missing store returns an error, which is a normal cold-start condition
	// and not something to alarm on.
	Load(env string) (snap core.Snapshot, writtenAt time.Time, err error)

	// Save persists snap for env. Failure is logged and counted, never fatal.
	Save(env string, snap core.Snapshot) error
}

// Codec serialises a snapshot for L2. It exists because core.Snapshot is an
// interface: the client cannot know how to rebuild the concrete type the
// transport handed it, so persistence needs an explicit encode/decode pair.
type Codec interface {
	Encode(core.Snapshot) ([]byte, error)
	Decode([]byte) (core.Snapshot, error)
}

// ErrNoSnapshot is returned by SnapshotStore.Load when nothing is persisted.
var ErrNoSnapshot = errors.New("client: no persisted snapshot")

// FileStore is the default L2: one file per environment, written atomically.
type FileStore struct {
	dir   string
	codec Codec
}

// NewFileStore returns an L2 store rooted at dir. A nil codec uses JSONCodec.
func NewFileStore(dir string, codec Codec) *FileStore {
	if codec == nil {
		codec = JSONCodec{}
	}
	return &FileStore{dir: dir, codec: codec}
}

// path resolves the on-disk location of env's snapshot, or reports that the
// resolved path would escape f.dir.
//
// The environment name reaches us from configuration and is interpolated into a
// path, so it is encoded rather than trusted. Two properties are required, and
// the second one is the one that bites.
//
// It must not escape the cache directory: encodeEnvName emits no path
// separators, so containment holds by construction, and is then asserted
// anyway — "unreachable" is one refactor away from "exploitable" when the value
// being interpolated arrived from configuration.
//
// It must be injective. The previous encoding mapped every byte outside
// [A-Za-z0-9-_] to '_', which meant "prod!", "prod?" and "prod/" all resolved to
// the same file. Two clients with genuinely different environments would then
// take turns overwriting each other's last-known-good, and a cold start during
// an outage would hydrate a pod with another environment's flag config —
// silently, with a plausible generation number, and looking exactly like a
// working pod. Cross-environment config bleed is the worst thing this cache can
// do, so distinct names must resolve to distinct files.
func (f *FileStore) path(env string) (string, error) {
	p := filepath.Clean(filepath.Join(f.dir, "flags-"+encodeEnvName(env)+".json"))
	if filepath.Dir(p) != filepath.Clean(f.dir) {
		return "", fmt.Errorf("client: environment %q resolves outside the cache directory %s", env, f.dir)
	}
	return p, nil
}

// envEscape introduces a two-hex-digit byte escape in an encoded environment
// name. It is excluded from the verbatim set below so that an escape sequence
// can never be forged by a literal character, which is what keeps the encoding
// injective.
const envEscape = '~'

// encodeEnvName maps an environment name to a filename component injectively:
// distinct names always produce distinct components, so no two environments can
// share a last-known-good file.
//
// Bytes in [a-z0-9-_] survive verbatim, so ordinary names ("prod", "staging-eu",
// "eu_west_1") stay readable in a directory listing at 3am, which is most of the
// value of naming the file after the environment at all. Every other byte,
// including every uppercase letter, becomes ~XX. Uppercase is escaped rather
// than kept because macOS and Windows fold filename case: left verbatim, "Prod"
// and "prod" would be injective as strings and still collide as files, which is
// the same bug one layer down. Environment names are lowercase by convention, so
// this costs readability only for names that are already unconventional.
//
// Decoding is unambiguous — '~' always begins exactly three bytes — which is the
// proof of injectivity, even though nothing needs to decode today.
func encodeEnvName(env string) string {
	if env == "" {
		// The empty name cannot encode to itself, and must not collapse onto an
		// environment genuinely called "default", as the previous version did.
		// "~empty" is unreachable for every non-empty input because an escape is
		// always '~' plus two hex digits, and 'm' is not a hex digit.
		return "~empty"
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(env))
	for i := 0; i < len(env); i++ {
		c := env[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte(envEscape)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

func (f *FileStore) Load(env string) (core.Snapshot, time.Time, error) {
	p, err := f.path(env)
	if err != nil {
		return nil, time.Time{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, time.Time{}, ErrNoSnapshot
		}
		return nil, time.Time{}, err
	}
	// Cleaned again at the syscall boundary. path has already cleaned this path
	// and checked that it resolves inside f.dir, and Clean is idempotent, so this
	// costs nothing and changes nothing — it keeps the guarantee legible (to a
	// reader and to the security scanner) at the point where a file is actually
	// opened, rather than three frames away in a helper someone may later edit.
	b, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		return nil, time.Time{}, err
	}
	snap, err := f.codec.Decode(b)
	if err != nil {
		// A snapshot this binary cannot parse is refused loudly rather than
		// half-read. Mis-parsing config is worse than having none.
		return nil, time.Time{}, fmt.Errorf("client: decode L2 snapshot %s: %w", p, err)
	}
	return snap, st.ModTime(), nil
}

// Save writes to a temporary file, fsyncs it, and renames it into place, so a
// crash or a full disk mid-write leaves the previous last-known-good intact
// rather than a truncated file that fails to parse on the next cold start.
func (f *FileStore) Save(env string, snap core.Snapshot) error {
	final, err := f.path(env)
	if err != nil {
		return err
	}
	// 0750 rather than 0755: this directory holds a full copy of one
	// environment's flag configuration, including rule attribute names, and
	// nothing about it needs to be world-readable on a shared host.
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		return err
	}
	b, err := f.codec.Encode(snap)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.dir, ".flags-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()

	// Cleanup is conditional on what actually happened, which fixes a real
	// defect rather than just quietening a linter. The previous version closed
	// the file in the defer *and* on the success path, so the deferred Close
	// returned os.ErrClosed on every successful save and discarded it. That made
	// a self-inflicted error indistinguishable from a genuine one — and a Close
	// error is the one that reports data lost between the write and the platter
	// on a delayed-allocation filesystem, which for this cache means a
	// last-known-good file that will fail to parse on the next cold start.
	// Now each branch runs only when it is the one thing left to do, and each
	// discarded error is discarded in writing, with the reason.
	closed, renamed := false, false
	defer func() {
		if !closed {
			// Reached only on a failure path that is already returning the
			// error that matters, for a temp file about to be removed.
			_ = tmp.Close()
		}
		if !renamed {
			// Best effort. A stale .flags-*.tmp is untidy, not harmful, and
			// there is no caller who could act on the failure to remove it.
			_ = os.Remove(name)
		}
	}()

	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(name, final); err != nil {
		return err
	}
	renamed = true
	return nil
}

// MemSnapshot is an immutable, map-backed core.Snapshot.
//
// It is exported because two callers need it: JSONCodec, which must rebuild a
// concrete snapshot when hydrating from L2, and application tests, which need
// to construct a client over fixed config without standing up a transport.
type MemSnapshot struct {
	env   string
	gen   int64
	flags map[string]*core.Flag
}

// NewMemSnapshot copies flags into an immutable snapshot. The caller may reuse
// or mutate the input slice afterwards; the snapshot will not observe it.
func NewMemSnapshot(env string, generation int64, flags []core.Flag) *MemSnapshot {
	m := &MemSnapshot{env: env, gen: generation, flags: make(map[string]*core.Flag, len(flags))}
	for i := range flags {
		f := flags[i]
		m.flags[f.Key] = &f
	}
	return m
}

func (m *MemSnapshot) Generation() int64 { return m.gen }
func (m *MemSnapshot) Env() string       { return m.env }
func (m *MemSnapshot) Len() int          { return len(m.flags) }

func (m *MemSnapshot) Flag(key string) (*core.Flag, bool) {
	f, ok := m.flags[key]
	return f, ok
}

// l2FormatVersion is bumped whenever the on-disk shape changes. A file written
// by a different version is refused, not best-effort parsed: silently
// mis-reading persisted config is the failure mode this field exists to stop.
const l2FormatVersion = 1

// JSONCodec is the default L2 codec.
//
// Enums are written as their string names rather than their numeric values,
// even though numbers would be smaller and faster. The file outlives the
// process that wrote it and is read back by a *different binary version* after
// a deploy; if someone inserts a constant into the middle of core.Operator, a
// numeric encoding turns every persisted "gt" into "contains" with nothing in
// the logs. Strings make that a decode error instead.
type JSONCodec struct{}

type jsonSnapshot struct {
	Format     int        `json:"format"`
	Env        string     `json:"env"`
	Generation int64      `json:"generation"`
	Flags      []jsonFlag `json:"flags"`
}

type jsonFlag struct {
	Key             string     `json:"key"`
	Type            string     `json:"type"`
	Enabled         bool       `json:"enabled"`
	DefaultValue    core.Value `json:"default_value"`
	OffValue        core.Value `json:"off_value"`
	Rules           []jsonRule `json:"rules,omitempty"`
	Rollout         *jsonRoll  `json:"rollout,omitempty"`
	EvaluationOrder string     `json:"evaluation_order,omitempty"`
}

type jsonRule struct {
	ID         string     `json:"id"`
	Conditions []jsonCond `json:"conditions"`
	Combiner   string     `json:"combiner"`
	Value      core.Value `json:"value"`
}

type jsonCond struct {
	Attribute string       `json:"attribute"`
	Op        string       `json:"op"`
	Values    []core.Value `json:"values"`
	Negate    bool         `json:"negate,omitempty"`
}

type jsonRoll struct {
	BasisPoints     int32      `json:"basis_points"`
	BucketNamespace string     `json:"bucket_namespace,omitempty"`
	BucketBy        string     `json:"bucket_by,omitempty"`
	OnValue         core.Value `json:"on_value"`
	OffValue        core.Value `json:"off_value"`
}

func (JSONCodec) Encode(s core.Snapshot) ([]byte, error) {
	if s == nil {
		return nil, errors.New("client: encode nil snapshot")
	}
	ms, ok := s.(*MemSnapshot)
	if !ok {
		// Any core.Snapshot can be encoded, but only via its public surface,
		// and that surface has no iterator. Rather than guess at flag keys, the
		// codec asks the snapshot to be one it can walk.
		return nil, fmt.Errorf("client: JSONCodec cannot encode %T; supply a transport-native Codec", s)
	}
	out := jsonSnapshot{Format: l2FormatVersion, Env: ms.env, Generation: ms.gen, Flags: make([]jsonFlag, 0, len(ms.flags))}
	for _, f := range ms.flags {
		jf := jsonFlag{
			Key:          f.Key,
			Type:         f.Type.String(),
			Enabled:      f.Enabled,
			DefaultValue: f.DefaultValue,
			OffValue:     f.OffValue,
		}
		if f.EvaluationOrder != core.OrderUnspecified {
			jf.EvaluationOrder = f.EvaluationOrder.String()
		}
		for _, r := range f.Rules {
			jr := jsonRule{ID: r.ID, Combiner: r.Combiner.String(), Value: r.Value}
			for _, c := range r.Conditions {
				jr.Conditions = append(jr.Conditions, jsonCond{
					Attribute: c.Attribute, Op: c.Op.String(), Values: c.Values, Negate: c.Negate,
				})
			}
			jf.Rules = append(jf.Rules, jr)
		}
		if f.Rollout != nil {
			jf.Rollout = &jsonRoll{
				BasisPoints:     f.Rollout.BasisPoints,
				BucketNamespace: f.Rollout.BucketNamespace,
				BucketBy:        f.Rollout.BucketBy,
				OnValue:         f.Rollout.OnValue,
				OffValue:        f.Rollout.OffValue,
			}
		}
		out.Flags = append(out.Flags, jf)
	}
	return json.Marshal(out)
}

func (JSONCodec) Decode(b []byte) (core.Snapshot, error) {
	var in jsonSnapshot
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	if in.Format != l2FormatVersion {
		return nil, fmt.Errorf("client: L2 format %d, want %d", in.Format, l2FormatVersion)
	}
	flags := make([]core.Flag, 0, len(in.Flags))
	for _, jf := range in.Flags {
		t, ok := core.ParseValueType(jf.Type)
		if !ok {
			return nil, fmt.Errorf("client: flag %q has unknown type %q", jf.Key, jf.Type)
		}
		f := core.Flag{
			Key:          jf.Key,
			Type:         t,
			Enabled:      jf.Enabled,
			DefaultValue: jf.DefaultValue,
			OffValue:     jf.OffValue,
		}
		switch jf.EvaluationOrder {
		case "", core.OrderUnspecified.String():
			f.EvaluationOrder = core.OrderUnspecified
		case core.OrderRulesFirst.String():
			f.EvaluationOrder = core.OrderRulesFirst
		default:
			return nil, fmt.Errorf("client: flag %q has unknown evaluation order %q", jf.Key, jf.EvaluationOrder)
		}
		for _, jr := range jf.Rules {
			r := core.Rule{ID: jr.ID, Value: jr.Value}
			if jr.Combiner == core.LogicOr.String() {
				r.Combiner = core.LogicOr
			} else {
				r.Combiner = core.LogicAnd
			}
			for _, jc := range jr.Conditions {
				op, ok := core.ParseOperator(jc.Op)
				if !ok {
					return nil, fmt.Errorf("client: rule %q has unknown operator %q", jr.ID, jc.Op)
				}
				r.Conditions = append(r.Conditions, core.Condition{
					Attribute: jc.Attribute, Op: op, Values: jc.Values, Negate: jc.Negate,
				})
			}
			f.Rules = append(f.Rules, r)
		}
		if jf.Rollout != nil {
			f.Rollout = &core.Rollout{
				BasisPoints:     jf.Rollout.BasisPoints,
				BucketNamespace: jf.Rollout.BucketNamespace,
				BucketBy:        jf.Rollout.BucketBy,
				OnValue:         jf.Rollout.OnValue,
				OffValue:        jf.Rollout.OffValue,
			}
		}
		flags = append(flags, f)
	}
	return NewMemSnapshot(in.Env, in.Generation, flags), nil
}

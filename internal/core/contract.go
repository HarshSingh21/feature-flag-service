package core

// Snapshot is an immutable, fully resolved view of one environment's flags.
//
// Implementations must be safe for concurrent use by many goroutines and must
// never be mutated after publication. The evaluator pins one Snapshot at the start
// of a request and uses that same pointer for every flag in it (invariant CACHE-1),
// so that a config swap landing mid-request cannot produce a result set where flag
// A came from generation N and flag B from generation N+1.
type Snapshot interface {
	// Generation is a monotonically increasing counter, per environment, per process.
	// It is only meaningful when paired with an instance identity -- a bare counter
	// resets on restart, and a client at generation 900 meeting a restarted server
	// at generation 3 would otherwise silently conclude it is ahead.
	Generation() int64

	// Env names the environment this snapshot resolves.
	Env() string

	// Flag returns the resolved flag. The returned pointer must be treated as
	// read-only by every caller.
	Flag(key string) (*Flag, bool)

	// Len reports the number of flags, for metrics and sizing.
	Len() int
}

// BucketKeyStrategy derives the string that gets hashed for a percentage rollout.
// This is the single plug point for decision O1.
//
// ok is false when the bucketing subject is absent from the context, which the
// evaluator surfaces as ReasonMissingSubject rather than inventing a bucket. A
// hash of the empty string would be deterministic AND arbitrary: every anonymous
// request would land in one bucket, making the rollout 0% or 100% for all of them
// and flipping the moment the namespace changes.
type BucketKeyStrategy interface {
	Key(flag *Flag, ctx EvalContext) (key string, ok bool)
}

// Hasher maps a bucketing key to a bucket in 0..BucketSpace-1.
//
// Implementations MUST be stable across process restarts, machines, and Go
// versions. This is a wire format, not an implementation detail: changing it
// re-buckets every user in every active rollout simultaneously. hash/maphash is
// specifically disqualified -- its per-process random seed reshuffles every
// rollout on every deploy.
type Hasher interface {
	Bucket(key string) int32
}

// BucketSpace is the number of distinct buckets, giving basis-point granularity.
const BucketSpace int32 = 10000

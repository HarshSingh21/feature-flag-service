package core

// Evaluator is the evaluation engine: a pure function of
// (snapshot, key, context, requested type, caller default).
//
// No I/O, no locks, no goroutines, no clock reads that affect the result, no
// allocation on the resolved paths. That purity is not aesthetic. It is what makes
// the never-throw contract enforceable, and it is what lets a 3am "why did user
// 88231 get true?" be answered by replaying the four inputs.
//
// An Evaluator holds no mutable state after construction and is safe for concurrent
// use by any number of goroutines.
//
// # The goroutine ban
//
// This package spawns no goroutines, ever. recover() only catches panics on the
// goroutine running the deferred function, so a panic in a goroutine started here
// would be uncatchable from the caller and would kill the process -- turning one
// malformed flag into a full outage. The panic boundary below is only sound because
// there is exactly one goroutine beneath it.
type Evaluator struct {
	strategy BucketKeyStrategy
	hasher   Hasher
	observer ConditionObserver
}

// Option configures an Evaluator.
type Option func(*Evaluator)

// WithBucketKeyStrategy overrides the bucket key strategy (decision O1's plug point).
//
// Changing this for a flag with a live rollout is a full reshuffle of that rollout's
// membership. It is a migration, not a configuration change.
func WithBucketKeyStrategy(s BucketKeyStrategy) Option {
	return func(e *Evaluator) {
		if s != nil {
			e.strategy = s
		}
	}
}

// WithHasher overrides the hasher.
//
// Provided for tests and for a future versioned bucket_algo migration. Substituting
// a hasher in production re-buckets every user in every active rollout; the golden
// vector test exists to make that impossible to do by accident.
func WithHasher(h Hasher) Option {
	return func(e *Evaluator) {
		if h != nil {
			e.hasher = h
		}
	}
}

// WithConditionObserver installs the hook that makes undecidable conditions
// countable. Without it, "present but wrong type -> false" is a completely silent
// failure: the rule does not match, the flag returns its default, and nothing
// anywhere says why.
func WithConditionObserver(o ConditionObserver) Option {
	return func(e *Evaluator) { e.observer = o }
}

// New builds an Evaluator. The defaults are the shipped decisions: NamespaceStrategy
// for O1 and XXHasher for bucketing.
func New(opts ...Option) *Evaluator {
	e := &Evaluator{
		strategy: NamespaceStrategy{},
		hasher:   XXHasher{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
}

// Evaluate resolves one flag.
//
// It NEVER returns an error and NEVER panics to the caller. Failure is data: it
// arrives as a Reason plus the caller's default in Result.Value.
//
// want is the type the caller is asserting. Pass TypeUnknown to assert nothing --
// that is the untyped/introspection path (flagctl, an admin UI). The typed
// accessors always pass a concrete type, which is what turns "BoolValue on a string
// flag" from a silent surprise into a TYPE_MISMATCH.
//
// callerDefault is the terminal fallback: the value returned when evaluation cannot
// produce a configured value at all. It is distinct from flag.DefaultValue, which is
// the flag's own fallthrough. Nothing here validates that callerDefault matches want;
// the caller supplied and typed it, and second-guessing it would mean having no
// fallback at all in the case where it is wrong.
//
// Stage order matches HLD C.1. Two placements are load bearing and are not
// negotiable:
//
//   - The requested-type check (S4) runs BEFORE the enabled check (S5). A caller
//     asking for the wrong type is a CODE bug, and it must surface identically
//     whether the flag happens to be on or off -- otherwise it hides until someone
//     flips the kill switch, which is exactly the worst moment to discover it.
//   - The resolved-value type check (S9) runs on EVERY path, including DISABLED. A
//     malformed OffValue is a common config defect; type-checking only the
//     "interesting" paths is how a type error ships to production behind a kill
//     switch and detonates during an incident.
func (e *Evaluator) Evaluate(snap Snapshot, key string, ctx EvalContext, want ValueType, callerDefault Value) (res Result) {
	// S0. The fail-safe answer exists before anything that can fail has run. Under
	// total collapse -- nil snapshot, half-constructed evaluator, corrupt config --
	// this is what leaves the function, because it was decided before we began.
	res = Result{Value: callerDefault, Reason: ReasonError, Bucket: NoBucket}

	// gen is tracked separately so a panic AFTER the generation is known still
	// reports which config was being consulted. "Which snapshot decided this?" is the
	// first question asked during an incident and the recover path should not
	// destroy the answer.
	var gen int64

	// S1. The panic boundary. It must live in this function because recover() only
	// works when called directly from a function deferred by the panicking
	// goroutine's stack, and because a named return is the only way for a deferred
	// function to change what the caller receives.
	//
	// The deferred function does field assignment and nothing else: no map access,
	// no config-pointer dereference, no formatting of config-derived values. A panic
	// inside a recover handler is not recoverable.
	defer func() {
		if r := recover(); r != nil {
			res = Result{Value: callerDefault, Reason: ReasonError, Bucket: NoBucket, Generation: gen}
			return
		}
		// Belt and braces for the "never ReasonUnknown" contract. Every path below
		// returns an explicit reason, so this is unreachable by construction -- which
		// is precisely why it is worth having: it converts a future missing-reason
		// bug from an unlabelled metric into an ERROR and the caller default.
		if res.Reason == ReasonUnknown {
			res = Result{Value: callerDefault, Reason: ReasonError, Bucket: NoBucket, Generation: gen}
		}
	}()

	// S2. Resolve the snapshot.
	//
	// A typed-nil inside a non-nil interface survives this check and panics on the
	// first method call; S1 catches it and produces the same ERROR. Both spellings of
	// "no snapshot" therefore land on the caller default.
	if snap == nil {
		return res // already caller default + ReasonError
	}
	gen = snap.Generation()
	res.Generation = gen

	// S3. Flag lookup. A miss is an ANSWER, not a cache miss -- the read path never
	// consults the config source (invariant CACHE-3), so a flag that was never merged
	// into a snapshot is indistinguishable from one that does not exist. That is why
	// ReasonFlagNotFound has to be a monitored signal rather than a silent default.
	flag, ok := snap.Flag(key)
	if !ok {
		return Result{Value: callerDefault, Reason: ReasonFlagNotFound, Bucket: NoBucket, Generation: gen}
	}
	if flag == nil {
		// Present in the index but nil: a broken snapshot, not a missing flag. Keep
		// the two distinguishable, because they have completely different fixes.
		return Result{Value: callerDefault, Reason: ReasonError, Bucket: NoBucket, Generation: gen}
	}

	// S4. Requested-type check, before the enabled check. See the doc comment.
	if want != TypeUnknown && want != flag.Type {
		return Result{Value: callerDefault, Reason: ReasonTypeMismatch, Bucket: NoBucket, Generation: gen}
	}

	// S5. Enabled check. A disabled flag is a CONFIGURED state, not an error, so it
	// returns the flag's off value and never the caller default.
	if !flag.Enabled {
		return e.finish(flag, Result{Value: flag.OffValue, Reason: ReasonDisabled, Bucket: NoBucket, Generation: gen}, callerDefault)
	}

	// S6a. Rules, in slice order, first match wins (decision O2: RULES FIRST).
	// THE ORDER OF THIS SLICE IS THE SEMANTICS.
	for i := range flag.Rules {
		if MatchRule(flag.Key, &flag.Rules[i], ctx, e.observer) {
			return e.finish(flag, Result{
				Value:      flag.Rules[i].Value,
				Reason:     ReasonRuleMatch,
				RuleID:     flag.Rules[i].ID,
				Bucket:     NoBucket,
				Generation: gen,
			}, callerDefault)
		}
	}

	// S6b. Rollout, for subjects that fell through EVERY rule. A matching rule
	// returned above, so the rollout is never consulted for a targeted subject and
	// no bucket is computed for one. That is decision O2 and it is what keeps the
	// reason model single-valued: RULE_MATCH xor ROLLOUT_IN/OUT, never both.
	//
	// The gate is `Rollout != nil`, deliberately NOT a `BasisPoints > 0` test, which would be
	// defined as BasisPoints > 0. A configured 0% rollout must return the rollout's
	// OFF value with ROLLOUT_OUT. Routing it to FALLTHROUGH instead would return
	// flag.DefaultValue -- which for a flag being ramped from zero is very often the
	// ON value, so "set it to 0%" would turn the feature on for everyone. A rollout
	// at 0 basis points is a configured rollout that includes nobody, not an absent
	// rollout.
	if flag.Rollout != nil {
		bkey, ok := e.strategy.Key(flag, ctx)
		if !ok {
			// Deliberately NOT a hash of the empty string: that would be
			// deterministic AND arbitrary, dropping all anonymous traffic into one
			// bucket so the rollout is 0% or 100% of it, flipping whenever the
			// namespace changes.
			return Result{Value: callerDefault, Reason: ReasonMissingSubject, Bucket: NoBucket, Generation: gen}
		}
		bucket := e.hasher.Bucket(bkey)
		if bucket < 0 || bucket >= BucketSpace {
			// A Hasher is an injectable interface, so an out-of-range bucket is
			// reachable via a third-party implementation. Refuse it rather than
			// returning a rollout decision derived from a broken bucket space.
			return Result{Value: callerDefault, Reason: ReasonError, Bucket: NoBucket, Generation: gen}
		}

		// Strictly less-than. THIS is the monotonicity guarantee: the bucket is fixed
		// per subject, so raising BasisPoints can only ADD subjects. Nobody ever
		// loses the feature during a ramp-up. Any scheme that folds the current
		// percentage into the hash destroys this, and it is a common mistake.
		//
		// No clamping is needed at the edges: buckets live in [0, 9999], so
		// BasisPoints <= 0 admits nobody and BasisPoints >= 10000 admits everybody.
		if bucket < flag.Rollout.BasisPoints {
			return e.finish(flag, Result{Value: flag.Rollout.OnValue, Reason: ReasonRolloutIn, Bucket: bucket, Generation: gen}, callerDefault)
		}
		return e.finish(flag, Result{Value: flag.Rollout.OffValue, Reason: ReasonRolloutOut, Bucket: bucket, Generation: gen}, callerDefault)
	}

	// S6c. Fallthrough: no rule matched and no rollout is configured.
	return e.finish(flag, Result{Value: flag.DefaultValue, Reason: ReasonFallthrough, Bucket: NoBucket, Generation: gen}, callerDefault)
}

// finish is stage S9: the type check on the RESOLVED value, applied on every
// configured path.
//
// A value whose type disagrees with the flag's declared type means the config that
// produced this snapshot is wrong in a way the snapshot builder failed to catch. The
// builder rejects these at config time; this is the belt to that pair of braces,
// because config-time rejection is how you avoid the incident and eval-time checking
// is how you survive the config-time check having a bug.
//
// On mismatch the caller default is returned rather than flag.DefaultValue. The flag
// is demonstrably mistyped, so nothing configured on it has earned any trust; the
// caller default is the only value in scope whose type the caller themselves
// guaranteed.
//
// The bucket is preserved on the mismatch path when a rollout was consulted. It cost
// nothing to compute and it is the difference between "this user was mistyped" and
// "this user was mistyped at bucket 7431 on a 2000 basis-point ramp".
func (e *Evaluator) finish(flag *Flag, res Result, callerDefault Value) Result {
	if res.Value.Type() != flag.Type {
		return Result{
			Value:      callerDefault,
			Reason:     ReasonTypeMismatch,
			Bucket:     res.Bucket,
			Generation: res.Generation,
		}
	}
	return res
}

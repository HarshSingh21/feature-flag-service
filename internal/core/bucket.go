package core

import (
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
)

// defaultBucketSubject is the attribute used when Rollout.BucketBy is empty.
const defaultBucketSubject = "user_id"

// XXHasher maps a bucketing key to a bucket using xxhash64 plus a multiply-shift
// reduction.
//
// This type is a WIRE FORMAT, not an implementation detail. Its output is the
// membership of every live rollout. Changing the hash, the reduction, or the bucket
// space re-buckets every user in every active rollout simultaneously: users silently
// leave experiments mid-flight (invalidating any metric computed over the exposure
// window), users lose a feature mid-session, and a canary that looked healthy at 1%
// is now a different 1% and proved nothing. Guarded by the golden vectors in
// bucket_test.go.
//
// xxhash rather than the alternatives:
//   - hash/maphash is randomly seeded PER PROCESS. It is fast and stdlib and it is a
//     trap: every deploy would reshuffle every rollout. Disqualified outright.
//   - hash/fnv has weak low-bit diffusion, which is exactly where a modulo reduction
//     reads.
//   - md5/sha1 cost 15-30x the entire rest of an evaluation for a property
//     (preimage resistance) that bucketing does not need. Bucketing is not a
//     security boundary.
//
// The zero value is ready to use, and it is stateless, so it is safe for concurrent
// use by any number of goroutines.
type XXHasher struct{}

// Bucket returns a bucket in [0, BucketSpace).
//
// The reduction is Lemire's multiply-shift on the HIGH 32 bits, not h % BucketSpace:
//
//	hi := h >> 32                      // uniform in [0, 2^32)
//	bucket := (hi * BucketSpace) >> 32 // uniform in [0, BucketSpace)
//
// Two reasons, in order of importance:
//
//  1. It reads the high bits. A modulo reads the LOW bits, so the uniformity of the
//     whole scheme would rest on the low-bit avalanche of whatever hash is wired in
//     today. xxhash's final avalanche is strong across all 64 bits, but the next
//     person to touch this file may not check that before swapping the hash, and a
//     bucketing bias does not announce itself -- it shows up as a rollout that says
//     10% and delivers 13%.
//  2. It costs a 64-bit multiply and a shift instead of a 64-bit division
//     (~20-40 cycles). At 2.4M evaluations/sec that is real.
//
// The residual non-uniformity is the multiply-shift's own: buckets differ in width by
// at most one out of 2^32 hash values, i.e. a relative bias below 2.4e-9. Modulo's
// bias at 2^64 is smaller still (~5e-16) but neither is measurable against any real
// population; reason 1 is what decides it.
func (XXHasher) Bucket(key string) int32 {
	h := xxhash.Sum64String(key)
	return int32((h >> 32) * uint64(BucketSpace) >> 32)
}

// NamespaceStrategy is the decision-O1 bucket key strategy: independent buckets per
// flag by default, shared bucket spaces on explicit opt-in.
//
// The salt is Rollout.BucketNamespace, falling back to the flag key. Two flags with
// default namespaces therefore bucket the same user independently, which is the
// property that stops one unlucky cohort from being the guinea pig for every risky
// change in the company. Two flags that set the SAME literal namespace share a bucket
// space deliberately -- that is the brief's opt-in sharing requirement, and it is
// opt-in precisely because it is the more dangerous of the two behaviours.
//
// The subject is Rollout.BucketBy, defaulting to user_id, resolved through
// EvalContext.Attribute (so user_id and tenant_id resolve from the shorthand fields
// and can be overridden by an explicit attribute entry).
//
// NO NORMALISATION. The subject is used as raw bytes: no case folding, no whitespace
// trimming, no Unicode NFC/NFD. "User123" and "user123" are different subjects and
// land in different buckets. That is deliberate -- normalisation is itself a
// semantic, and adding or changing one later is a reshuffle. Lint for it at the SDK
// boundary, not here.
//
// Stateless; the zero value is ready to use and safe for concurrent use.
type NamespaceStrategy struct{}

// Key composes the bucketing key.
//
// # Format
//
//	<len(namespace)> ':' <namespace> ':' <subject>
//
// e.g. namespace "checkout-v2", subject "u-42" -> "11:checkout-v2:u-42"
//
// # Why the length prefix, and not just namespace + ":" + subject
//
// The obvious format is ambiguous, and the ambiguity is a correctness bug rather
// than an aesthetic one. Neither a flag key nor an operator-authored namespace is
// guaranteed to exclude ':' -- flag keys in the wild look like "billing:invoice:v2",
// and nothing in the schema forbids it. Under the naive format:
//
//	namespace "a:b", subject "c"   -> "a:b:c"
//	namespace "a",   subject "b:c" -> "a:b:c"
//
// Two DIFFERENT flags produce the SAME key, so they silently share a bucket space.
// That is the exact failure mode decision O1 exists to prevent, and it is invisible:
// nothing errors, no counter moves, the two flags just correlate. Their canaries hit
// the same cohort, and when one of them causes an incident the other one's ramp is a
// confounder nobody thought to check.
//
// The alternatives considered:
//
//   - A separator "that cannot appear", e.g. 0x1F (ASCII unit separator). This is the
//     usual answer and it is only probably true. The namespace is operator-authored
//     free text and the subject is an arbitrary attribute value that arrives over
//     JSON, where a \u001f escape is a perfectly legal string. It reduces the
//     collision probability; it does not eliminate it. A safety property that holds
//     "unless someone sends a weird byte" is not a safety property.
//   - Escaping ':' in both parts. Injective, but it makes the key a function of an
//     escaping routine -- and any future change to that routine (or a disagreement
//     between the service and a debugging tool that reimplements it) reshuffles every
//     live rollout.
//   - Length prefixing. Injective for ALL byte strings with no exceptions and no
//     escaping: the decimal length is unambiguously terminated by the first ':'
//     because a decimal number contains no ':'; the namespace is then exactly that
//     many bytes; everything after the following ':' is the subject. The key can also
//     be pulled apart by eye in a log line during an incident, which the 0x1F form
//     cannot.
//
// Length prefixing wins. The second ':' is redundant given the prefix and is kept
// anyway because it makes the key readable.
//
// # Subject rendering
//
// The subject is Value.String(): the raw bytes for a string, decimal for an int,
// "true"/"false" for a bool. Note the one consequence: an int subject 42 and a string
// subject "42" produce the same key. That is accepted -- a given attribute has one
// type in practice, and the alternative (tagging the type into the key) would make
// the bucket depend on the attribute's declared type, so a config change from int to
// string would reshuffle a live rollout. A stable bucket is worth more than a
// distinction nobody can construct.
//
// ok is false when the subject is absent or renders empty. The evaluator surfaces
// that as ReasonMissingSubject rather than inventing a bucket: hashing the empty
// string would be deterministic AND arbitrary, landing all anonymous traffic in one
// bucket so the rollout is 0% or 100% of it -- a cliff, not a ramp.
func (NamespaceStrategy) Key(flag *Flag, ctx EvalContext) (string, bool) {
	if flag == nil {
		return "", false
	}

	namespace := flag.Key
	subjectAttr := defaultBucketSubject
	if flag.Rollout != nil {
		if flag.Rollout.BucketNamespace != "" {
			namespace = flag.Rollout.BucketNamespace
		}
		if flag.Rollout.BucketBy != "" {
			subjectAttr = flag.Rollout.BucketBy
		}
	}

	v, ok := ctx.Attribute(subjectAttr)
	if !ok || v.IsUnknown() {
		return "", false
	}
	subject := v.String()
	if subject == "" {
		return "", false
	}

	// One allocation: the Builder's backing array becomes the returned string.
	// lenBuf never escapes, so rendering the length is stack-only.
	var lenBuf [20]byte
	prefix := strconv.AppendInt(lenBuf[:0], int64(len(namespace)), 10)

	var b strings.Builder
	b.Grow(len(prefix) + 2 + len(namespace) + len(subject))
	b.Write(prefix)
	b.WriteByte(':')
	b.WriteString(namespace)
	b.WriteByte(':')
	b.WriteString(subject)
	return b.String(), true
}

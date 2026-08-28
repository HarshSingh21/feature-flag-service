package core

import (
	"strconv"
	"strings"
)

// semver is a minimal, allocation-free semantic version, sufficient for the three
// semver operators and nothing more.
//
// Why hand-rolled rather than a dependency: this package is ring 0 and imports
// nothing that performs I/O. A semver library is ~600 lines of surface for the
// ~120 we need, and every dependency added here is a dependency that can change
// comparison semantics under us -- which, for a targeting rule, is a silent
// behavioural migration across every flag that uses one.
//
// Build metadata is parsed for validity and then discarded: SemVer 2.0.0 §10 says
// build metadata MUST be ignored when determining version precedence.
type semver struct {
	major, minor, patch uint64

	// pre is the raw prerelease string with the leading '-' removed. It is compared
	// identifier-by-identifier, never as a whole string, because "alpha.10" must
	// sort above "alpha.9" and a plain string compare gets that backwards.
	pre string

	// hasPre distinguishes "1.0.0" from "1.0.0-" (the latter is invalid) and, more
	// importantly, from a version whose prerelease is present. A prerelease sorts
	// BELOW the release it precedes: 1.0.0-rc1 < 1.0.0.
	hasPre bool
}

// parseSemver parses MAJOR.MINOR.PATCH[-prerelease][+build].
//
// It returns ok=false rather than an error: malformed input in an attribute is a
// caller data-quality problem, not a service fault, and it must degrade exactly one
// condition to false rather than collapsing the flag (see HLD C.3).
//
// Deliberately strict, each rejection for a reason:
//   - No "v" prefix. Accepting "v1.2.3" as 1.2.3 is a coercion, and this package
//     does not coerce anywhere else. Config-side values are validated at build time,
//     so a "v" in a rule is caught before it ships.
//   - Exactly three numeric components. "1.2" is not a version, it is a truncation,
//     and guessing that the patch is 0 is guessing.
//   - No leading zeros on numeric identifiers ("01.2.3" is invalid), per spec.
//   - No empty identifiers ("1.0.0-alpha..1" is invalid), per spec.
func parseSemver(s string) (semver, bool) {
	if s == "" {
		return semver{}, false
	}

	// Build metadata first: '+' cannot appear anywhere else, so the first '+' ends
	// the precedence-bearing part of the string.
	if i := strings.IndexByte(s, '+'); i >= 0 {
		if !validDotIdentifiers(s[i+1:], false) {
			return semver{}, false
		}
		s = s[:i]
	}

	var v semver
	// Prerelease next. The first '-' after the core triple starts it; a '-' inside
	// the prerelease itself (1.0.0-alpha-1) is a legal identifier character and is
	// therefore kept.
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre = s[i+1:]
		v.hasPre = true
		if !validDotIdentifiers(v.pre, true) {
			return semver{}, false
		}
		s = s[:i]
	}

	var ok bool
	if v.major, s, ok = takeNumericField(s, '.'); !ok {
		return semver{}, false
	}
	if v.minor, s, ok = takeNumericField(s, '.'); !ok {
		return semver{}, false
	}
	if v.patch, s, ok = takeNumericField(s, 0); !ok {
		return semver{}, false
	}
	return v, true
}

// takeNumericField consumes one numeric identifier from the head of s. When sep is
// non-zero the field must be terminated by sep; when sep is 0 the field is the whole
// remainder.
func takeNumericField(s string, sep byte) (n uint64, rest string, ok bool) {
	field := s
	if sep != 0 {
		i := strings.IndexByte(s, sep)
		if i < 0 {
			return 0, "", false
		}
		field, rest = s[:i], s[i+1:]
	}
	if !isNumericIdentifier(field) {
		return 0, "", false
	}
	n, err := strconv.ParseUint(field, 10, 64)
	if err != nil { // overflow past 2^64-1
		return 0, "", false
	}
	return n, rest, true
}

// isNumericIdentifier reports whether s is a spec-legal numeric identifier:
// non-empty, all ASCII digits, and without a leading zero unless it is exactly "0".
func isNumericIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) == 1 || s[0] != '0'
}

// validDotIdentifiers validates a dot-separated identifier list. When numericStrict
// is true (prerelease), all-digit identifiers must not carry a leading zero; build
// metadata has no such rule because it never participates in comparison.
func validDotIdentifiers(s string, numericStrict bool) bool {
	if s == "" {
		return false
	}
	for {
		id, rest := nextIdentifier(s)
		if id == "" {
			return false
		}
		allDigits := true
		for i := 0; i < len(id); i++ {
			c := id[i]
			switch {
			case c >= '0' && c <= '9':
			case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '-':
				allDigits = false
			default:
				return false
			}
		}
		if numericStrict && allDigits && !isNumericIdentifier(id) {
			return false
		}
		if strings.IndexByte(s, '.') < 0 {
			return true
		}
		s = rest
	}
}

// nextIdentifier splits off the leading dot-separated identifier.
func nextIdentifier(s string) (id, rest string) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// compare returns -1, 0 or +1 for a<b, a==b, a>b, per SemVer 2.0.0 §11.
func (a semver) compare(b semver) int {
	if c := cmpUint64(a.major, b.major); c != 0 {
		return c
	}
	if c := cmpUint64(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmpUint64(a.patch, b.patch); c != 0 {
		return c
	}
	switch {
	case !a.hasPre && !b.hasPre:
		return 0
	case !a.hasPre:
		return 1 // a release outranks any prerelease of the same triple
	case !b.hasPre:
		return -1
	}
	return comparePrerelease(a.pre, b.pre)
}

func cmpUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePrerelease compares two validated prerelease strings identifier by
// identifier. A shorter prerelease that is a prefix of a longer one sorts lower:
// 1.0.0-alpha < 1.0.0-alpha.1.
func comparePrerelease(x, y string) int {
	for {
		switch {
		case x == "" && y == "":
			return 0
		case x == "":
			return -1
		case y == "":
			return 1
		}
		xi, xr := nextIdentifier(x)
		yi, yr := nextIdentifier(y)
		if c := compareIdentifier(xi, yi); c != 0 {
			return c
		}
		x, y = xr, yr
	}
}

// compareIdentifier applies SemVer 2.0.0 §11.4:
// numeric identifiers compare numerically; alphanumeric identifiers compare by ASCII
// order; a numeric identifier always sorts BELOW an alphanumeric one.
func compareIdentifier(a, b string) int {
	an, bn := isNumericIdentifier(a), isNumericIdentifier(b)
	switch {
	case an && bn:
		// Both are canonical (no leading zeros), so longer means larger. Comparing by
		// length then lexically avoids parsing, and therefore avoids overflowing on a
		// prerelease identifier longer than 20 digits -- which is legal input.
		if len(a) != len(b) {
			return cmpInt(len(a), len(b))
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

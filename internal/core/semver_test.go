package core

import "testing"

func TestParseSemverAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in                  string
		major, minor, patch uint64
		pre                 string
		hasPre              bool
	}{
		{in: "0.0.0"},
		{in: "1.2.3", major: 1, minor: 2, patch: 3},
		{in: "10.20.30", major: 10, minor: 20, patch: 30},
		{in: "1.0.0-alpha", major: 1, pre: "alpha", hasPre: true},
		{in: "1.0.0-alpha.1", major: 1, pre: "alpha.1", hasPre: true},
		{in: "1.0.0-0.3.7", major: 1, pre: "0.3.7", hasPre: true},
		{in: "1.0.0-x.7.z.92", major: 1, pre: "x.7.z.92", hasPre: true},
		{in: "1.0.0-alpha-1", major: 1, pre: "alpha-1", hasPre: true}, // hyphen is a legal identifier char
		{in: "1.0.0+build.1", major: 1},                               // build metadata parsed then discarded
		{in: "1.0.0-rc.1+exp.sha.5114f85", major: 1, pre: "rc.1", hasPre: true},
		{in: "1.0.0+0.build.1-rc", major: 1}, // '-' inside build metadata is not a prerelease
		{in: "18446744073709551615.0.0", major: 1<<64 - 1},
	}
	for _, tc := range tests {
		got, ok := parseSemver(tc.in)
		if !ok {
			t.Errorf("parseSemver(%q) = not ok, want ok", tc.in)
			continue
		}
		if got.major != tc.major || got.minor != tc.minor || got.patch != tc.patch {
			t.Errorf("parseSemver(%q) = %d.%d.%d, want %d.%d.%d",
				tc.in, got.major, got.minor, got.patch, tc.major, tc.minor, tc.patch)
		}
		if got.pre != tc.pre || got.hasPre != tc.hasPre {
			t.Errorf("parseSemver(%q) prerelease = %q/%v, want %q/%v", tc.in, got.pre, got.hasPre, tc.pre, tc.hasPre)
		}
	}
}

func TestParseSemverRejects(t *testing.T) {
	t.Parallel()
	// Malformed input is never an error and never a panic. It is simply not a
	// version, so the condition using it is false.
	bad := []string{
		"",
		"1",
		"1.2",
		"1.2.3.4",
		"1.2.x",
		"a.b.c",
		"1..3",
		"1.2.",
		".2.3",
		"01.2.3",     // leading zero, spec-illegal
		"1.02.3",     // leading zero
		"1.2.03",     // leading zero
		"v1.2.3",     // no "v" prefix: accepting it would be a coercion
		"1.2.3 ",     // no trimming
		" 1.2.3",     //
		"1.2.3-",     // empty prerelease
		"1.2.3+",     // empty build
		"1.2.3-01",   // numeric prerelease identifier with leading zero
		"1.2.3-a..b", // empty prerelease identifier
		"1.2.3-a.",   // trailing empty identifier
		"1.2.3-a$b",  // illegal identifier character
		"1.2.3+a..b",
		"-1.2.3",
		"1.-2.3",
		"18446744073709551616.0.0", // overflows uint64
		"1.2.3-alpha+",             // empty build after prerelease
	}
	for _, in := range bad {
		if v, ok := parseSemver(in); ok {
			t.Errorf("parseSemver(%q) = %+v, ok; want not ok", in, v)
		}
	}
}

func TestSemverOrdering(t *testing.T) {
	t.Parallel()
	// The canonical SemVer 2.0.0 precedence chain, plus the two edge cases that are
	// most often implemented wrong: prerelease sorts BELOW the release, and numeric
	// prerelease identifiers compare numerically (so alpha.10 > alpha.9, which a
	// plain string compare gets backwards).
	ascending := []string{
		"0.0.4",
		"1.0.0-0",
		"1.0.0-1",
		"1.0.0-9",
		"1.0.0-10", // numeric: 10 > 9. A lexical compare would put this before "9".
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11", // 11 > 2 numerically
		"1.0.0-rc.1",
		"1.0.0", // the release outranks every prerelease of the same triple
		"1.0.1",
		"1.1.0",
		"2.0.0",
		"2.1.0",
		"2.1.1",
		"10.0.0", // 10 > 2 numerically, not lexically
	}
	parsed := make([]semver, len(ascending))
	for i, s := range ascending {
		v, ok := parseSemver(s)
		if !ok {
			t.Fatalf("parseSemver(%q) failed", s)
		}
		parsed[i] = v
	}
	for i := range parsed {
		for j := range parsed {
			got := parsed[i].compare(parsed[j])
			want := cmpInt(i, j)
			if got != want {
				t.Errorf("compare(%q, %q) = %d, want %d", ascending[i], ascending[j], got, want)
			}
		}
	}
}

func TestSemverNumericSortsBelowAlphanumeric(t *testing.T) {
	t.Parallel()
	// SemVer 2.0.0 section 11.4.3: "Numeric identifiers always have lower precedence
	// than non-numeric identifiers."
	lower, _ := parseSemver("1.0.0-1")
	upper, _ := parseSemver("1.0.0-alpha")
	if lower.compare(upper) >= 0 {
		t.Fatal("1.0.0-1 must sort below 1.0.0-alpha")
	}
	// And "9" below "a" even though '9' < 'a' lexically too -- check the reverse
	// direction where lexical and spec order disagree.
	nine, _ := parseSemver("1.0.0-99999999999999999999999")
	a, _ := parseSemver("1.0.0-A")
	if nine.compare(a) >= 0 {
		t.Fatal("a very long numeric identifier must still sort below an alphanumeric one")
	}
}

func TestSemverHugeNumericPrereleaseDoesNotOverflow(t *testing.T) {
	t.Parallel()
	// Prerelease numeric identifiers have no width limit in the spec, so they are
	// compared by length-then-lexically rather than parsed. Parsing them would
	// overflow and silently invert the comparison.
	small, ok1 := parseSemver("1.0.0-99999999999999999999999999")
	big, ok2 := parseSemver("1.0.0-999999999999999999999999999")
	if !ok1 || !ok2 {
		t.Fatal("long numeric prerelease identifiers must parse")
	}
	if small.compare(big) != -1 {
		t.Fatal("longer canonical numeric identifier must sort higher")
	}
}

func TestSemverBuildMetadataIgnored(t *testing.T) {
	t.Parallel()
	// SemVer 2.0.0 section 10: build metadata MUST be ignored when determining
	// version precedence.
	a, _ := parseSemver("1.0.0+alpha")
	b, _ := parseSemver("1.0.0+zulu")
	c, _ := parseSemver("1.0.0")
	if a.compare(b) != 0 || a.compare(c) != 0 {
		t.Fatal("build metadata must not affect precedence")
	}
	d, _ := parseSemver("1.0.0-rc.1+aaa")
	e, _ := parseSemver("1.0.0-rc.1+zzz")
	if d.compare(e) != 0 {
		t.Fatal("build metadata must not affect precedence on a prerelease either")
	}
}

func TestSemverCompareIsAntisymmetric(t *testing.T) {
	t.Parallel()
	versions := []string{"1.0.0", "1.0.1", "1.0.0-rc1", "2.0.0", "1.1.0", "1.0.0-rc1.2", "0.0.1"}
	parsed := make([]semver, len(versions))
	for i, s := range versions {
		parsed[i], _ = parseSemver(s)
	}
	for i := range parsed {
		for j := range parsed {
			if parsed[i].compare(parsed[j]) != -parsed[j].compare(parsed[i]) {
				t.Fatalf("compare is not antisymmetric for %q vs %q", versions[i], versions[j])
			}
		}
	}
}

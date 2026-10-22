package changelog

import (
	"regexp"
	"strings"
)

// A version is a semantic version, MAJOR.MINOR.PATCH with an optional
// -PRERELEASE and +BUILD, as semver 2.0.0 defines them, written with or
// without a leading v.

// version is a parsed semantic version. Its numbers stay text, so a
// number of any length compares without overflow.
type version struct {
	major, minor, patch string
	pre                 []string
}

// parse reads v, or reports it is not a version.
func parse(v string) (version, bool) {
	v = strings.TrimPrefix(v, "v")
	v, build, hasBuild := strings.Cut(v, "+")
	if hasBuild && !identifiers(build, false) {
		return version{}, false
	}
	core, pre, hasPre := strings.Cut(v, "-")
	if hasPre && !identifiers(pre, true) {
		return version{}, false
	}
	nums := strings.Split(core, ".")
	if len(nums) != 3 {
		return version{}, false
	}
	for _, n := range nums {
		if !numeric(n) {
			return version{}, false
		}
	}
	p := version{major: nums[0], minor: nums[1], patch: nums[2]}
	if hasPre {
		p.pre = strings.Split(pre, ".")
	}
	return p, true
}

// numeric reports whether s is a number with no leading zero.
func numeric(s string) bool {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// identifiers reports whether s is dot-separated identifiers of ASCII
// letters, digits, and hyphens. A prerelease's numeric identifiers may
// not have a leading zero.
func identifiers(s string, pre bool) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" || strings.Trim(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
			return false
		}
		if pre && strings.Trim(id, "0123456789") == "" && !numeric(id) {
			return false
		}
	}
	return true
}

// Canonical returns v as "vMAJOR.MINOR.PATCH[-PRERELEASE]", without its
// build metadata, which does not count in precedence. It returns "" when
// v is not a version.
func Canonical(v string) string {
	p, ok := parse(v)
	if !ok {
		return ""
	}
	out := "v" + p.major + "." + p.minor + "." + p.patch
	if len(p.pre) > 0 {
		out += "-" + strings.Join(p.pre, ".")
	}
	return out
}

// Compare returns -1, 0, or +1 as a is below, equal to, or above b in
// semver precedence. Something that is not a version is below every
// version and equal to anything else that is not one.
func Compare(a, b string) int {
	pa, okA := parse(a)
	pb, okB := parse(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for _, c := range [][2]string{{pa.major, pb.major}, {pa.minor, pb.minor}, {pa.patch, pb.patch}} {
		if n := compareNum(c[0], c[1]); n != 0 {
			return n
		}
	}
	// A prerelease is below its release.
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0
	case len(pa.pre) == 0:
		return 1
	case len(pb.pre) == 0:
		return -1
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		x, y := pa.pre[i], pb.pre[i]
		xn, yn := numeric(x), numeric(y)
		var n int
		switch {
		case xn && yn:
			n = compareNum(x, y)
		case xn:
			n = -1 // a numeric identifier is below an alphanumeric one
		case yn:
			n = 1
		default:
			n = strings.Compare(x, y)
		}
		if n != 0 {
			return n
		}
	}
	return compareInt(len(pa.pre), len(pb.pre))
}

// compareNum compares two numbers without leading zeros, as text.
func compareNum(a, b string) int {
	if n := compareInt(len(a), len(b)); n != 0 {
		return n
	}
	return strings.Compare(a, b)
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// describeRE matches what git describe adds to a tag for a build that is
// not the tag itself: the commits since it and the commit, and -dirty
// for uncommitted changes, as in "v1.3.0-4-gabc1234-dirty".
var describeRE = regexp.MustCompile(`(-\d+-g[0-9a-f]{4,})?(-dirty)?$`)

// IsRelease reports whether v is a release tag rather than a build: a
// version, a prerelease such as v1.3.0-rc.1 included, without the
// suffix git describe adds to a build after or beside a tag. Semver reads
// "v1.3.0-4-gabc1234" as a prerelease, so this is the check that tells
// them apart.
func IsRelease(v string) bool {
	if _, ok := parse(v); !ok {
		return false
	}
	return describeRE.FindString(v) == ""
}

// Base returns the release a git describe build descends from, in
// canonical form: "v1.3.0" for "v1.3.0-4-gabc1234-dirty", and v itself
// for a release. It returns "" when v is not a version.
func Base(v string) string {
	if _, ok := parse(v); !ok {
		return ""
	}
	return Canonical(strings.TrimSuffix(v, describeRE.FindString(v)))
}

package changelog_test

import (
	"reflect"
	"strings"
	"testing"

	"terva.sh/airut/changelog"
)

const keepAChangelog = `# Changelog

All notable changes, for the people who use the bot.

## [Unreleased]

- Something not out yet.

## [1.10.0] - 2026-10-22

### Added

- A dice tray.

## [1.2.0] - 2026-09-01
- Rolls are faster.

` + "```" + `
## not a heading, inside a fence
` + "```" + `

## [1.2.0-rc.1] - 2026-08-20

- A first look.

[unreleased]: https://example.com/compare/v1.10.0...HEAD
[1.10.0]: https://example.com/compare/v1.2.0...v1.10.0
`

const plain = "## v0.2.0\nSecond.\r\n\r\n## 0.1.0 ##\nFirst.\n"

func versions(ss []changelog.Section) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Version)
	}
	return out
}

func TestParseKeepAChangelog(t *testing.T) {
	l, err := changelog.Parse(keepAChangelog)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first by semver, not by text: 1.10.0 is above 1.2.0, and a
	// prerelease below its release. Unreleased is not a section.
	if got, want := versions(l.Sections()), []string{"v1.10.0", "v1.2.0", "v1.2.0-rc.1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %q, want %q", got, want)
	}
	s, ok := l.Section("1.10.0")
	if !ok || s.Notes != "### Added\n\n- A dice tray." {
		t.Errorf("1.10.0 = %q, %v", s.Notes, ok)
	}
	// The fence is part of the notes, and its "## " line did not end
	// the section.
	s, _ = l.Section("v1.2.0")
	if want := "- Rolls are faster.\n\n```\n## not a heading, inside a fence\n```"; s.Notes != want {
		t.Errorf("1.2.0 = %q, want %q", s.Notes, want)
	}
	// The link definitions at the end are dropped.
	s, _ = l.Section("v1.2.0-rc.1")
	if s.Notes != "- A first look." {
		t.Errorf("1.2.0-rc.1 = %q", s.Notes)
	}
	for _, s := range l.Sections() {
		if strings.Contains(s.Notes, "Something not out yet") || strings.Contains(s.Notes, "notable changes") {
			t.Errorf("%s holds Unreleased or the preamble: %q", s.Version, s.Notes)
		}
	}
}

func TestParsePlainHeadings(t *testing.T) {
	l, err := changelog.Parse(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Sections(); !reflect.DeepEqual(got, []changelog.Section{{"v0.2.0", "Second."}, {"v0.1.0", "First."}}) {
		t.Fatalf("sections = %q", got)
	}
	// An oldest-first file sorts the same.
	l, err = changelog.Parse("## v0.1.0\nFirst.\n## Unreleased\nSoon.\n## v0.2.0\nSecond.\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(l.Sections()); !reflect.DeepEqual(got, []string{"v0.2.0", "v0.1.0"}) {
		t.Fatalf("versions = %q", got)
	}
}

func TestParseRefusesWhatItCannotPlace(t *testing.T) {
	for _, notes := range []string{
		"## v1.0.0\nok\n## Fixes\nlost",
		"## v1.2\nnot three numbers",
		"## [1.0.0\nunclosed bracket",
		"## v1.0.0\none\n## 1.0.0\ntwo",
	} {
		if _, err := changelog.Parse(notes); err == nil || !strings.Contains(err.Error(), "line ") {
			t.Errorf("Parse(%q) = %v, want an error naming the line", notes, err)
		}
	}
	// A level-3 heading and a ## without a space are notes, not headings.
	l, err := changelog.Parse("## v1.0.0\n### Fixes\n##not-a-heading\n")
	if err != nil || len(l.Sections()) != 1 {
		t.Fatalf("Parse = %v, %v", l, err)
	}
	if l, err := changelog.Parse(""); err != nil || len(l.Sections()) != 0 {
		t.Fatalf("empty notes = %v, %v", l, err)
	}
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		notes, version string
		want           bool
	}{
		{keepAChangelog, "v1.10.0", true},
		{keepAChangelog, "1.10.0", true},
		{keepAChangelog, "v1.2.0-rc.1", true},
		{keepAChangelog, "v1.3.0", false},
		{keepAChangelog, "Unreleased", false},
		{"## v1.0.0\n\n## v0.9.0\nold", "v1.0.0", false}, // a section with no text
		{"## nonsense", "v1.0.0", false},
	} {
		if got := changelog.Covers(c.notes, c.version); got != c.want {
			t.Errorf("Covers(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestBetween(t *testing.T) {
	l, err := changelog.Parse("## v1.5.0\n5\n## v1.4.0\n4\n## v1.3.0\n3\n## v1.2.0\n2\n## v1.1.0\n1\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		after, upTo string
		want        []string
	}{
		{"v1.2.0", "v1.5.0", []string{"v1.5.0", "v1.4.0", "v1.3.0"}},
		{"v1.2.0", "v1.2.0", nil},
		{"v1.5.0", "v1.2.0", nil}, // a rollback
		{"v1.2.5", "v1.4.0", []string{"v1.4.0", "v1.3.0"}},
		{"", "v1.2.0", []string{"v1.2.0", "v1.1.0"}},
		{"v1.2.0", "not a version", nil},
		{"v1.3.0", "v1.4.0-rc.1", nil},
	} {
		if got := versions(l.Between(c.after, c.upTo)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Between(%q, %q) = %q, want %q", c.after, c.upTo, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	// In ascending order, from the semver 2.0.0 specification's example.
	order := []string{
		"not a version",
		"v1.0.0-alpha", "1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta",
		"v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0",
		"v1.0.1", "v1.9.0", "v1.10.0", "v2.0.0", "v100000000000000000000.0.0",
	}
	for i := range order {
		for j := range order {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := changelog.Compare(order[i], order[j]); got != want {
				t.Errorf("Compare(%q, %q) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	if changelog.Compare("v1.0.0+build.1", "1.0.0") != 0 {
		t.Error("build metadata counted in precedence")
	}
	if changelog.Compare("junk", "") != 0 {
		t.Error("two things that are not versions are not equal")
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.0": "v1.2.0", "v1.2.0": "v1.2.0", "v1.2.0-rc.1+build.5": "v1.2.0-rc.1",
		"v1.2": "", "v01.2.0": "", "v1.2.0-01": "", "v1.2.0-": "", "v1.2.0-a..b": "",
		"v1.2.0+": "", "": "", "Unreleased": "", "v1.2.0-x_y": "",
	} {
		if got := changelog.Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReleaseOrBuild(t *testing.T) {
	for v, release := range map[string]bool{
		"v1.3.0":                       true,
		"1.3.0":                        true,
		"v1.3.0-rc.1":                  true,
		"v1.3.0-4-gabc1234":            false,
		"v1.3.0-4-gabc1234-dirty":      false,
		"v1.3.0-dirty":                 false,
		"v1.3.0-rc.1-12-g0123abcd":     false,
		"abc1234":                      false, // git describe --always with no tag
		"":                             false,
		"v1.3.0-4-gabc1234-dirty-more": true, // not git describe's shape
	} {
		if got := changelog.IsRelease(v); got != release {
			t.Errorf("IsRelease(%q) = %v, want %v", v, got, release)
		}
	}
	for v, want := range map[string]string{
		"v1.3.0": "v1.3.0", "1.3.0": "v1.3.0", "v1.3.0-4-gabc1234-dirty": "v1.3.0",
		"v1.3.0-dirty": "v1.3.0", "v1.3.0-rc.1-2-gabcdef0": "v1.3.0-rc.1", "abc1234": "",
	} {
		if got := changelog.Base(v); got != want {
			t.Errorf("Base(%q) = %q, want %q", v, got, want)
		}
	}
}

// A marker indented four spaces is code, not the end of the fence, so
// the "## " line after it is still inside it.
func TestIndentedMarkerDoesNotCloseAFence(t *testing.T) {
	notes := "## v1.0.0\n\n```\n    ```\n## example\n```\n"
	l, err := changelog.Parse(notes)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s, ok := l.Section("v1.0.0")
	if want := "```\n    ```\n## example\n```"; !ok || s.Notes != want {
		t.Errorf("notes = %q, want %q", s.Notes, want)
	}
}

// A line of backticks holding more backticks is an inline code span, not
// a fence, so the next heading still starts a section.
func TestInlineCodeSpanIsNotAFence(t *testing.T) {
	l, err := changelog.Parse("## v1.0.0\n```inline```\n## v2.0.0\nNew notes\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s, ok := l.Section("v2.0.0"); !ok || s.Notes != "New notes" {
		t.Errorf("v2.0.0 = %q, %v; want its own section", s.Notes, ok)
	}
	if s, _ := l.Section("v1.0.0"); s.Notes != "```inline```" {
		t.Errorf("v1.0.0 notes = %q", s.Notes)
	}
}

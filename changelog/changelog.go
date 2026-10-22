// Package changelog reads a bot's user-facing changelog: Markdown with a
// level-2 heading for each version, newest or oldest first, as
// Keep a Changelog writes it or as plain version headings.
//
//	## [1.2.0] - 2026-10-22
//	## v1.2.0
//	## 1.2.0
//
// A version's notes run from its heading to the next level-2 heading.
// An Unreleased section is skipped. Versions are semantic versions, the
// leading v optional, and compare by semver precedence.
//
// It imports only the standard library, so a bot's release check or
// test can parse its notes without the rest of airut.
package changelog

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Section is one version's notes.
type Section struct {
	// Version is the heading's version in canonical form, as "v1.2.0".
	Version string
	// Notes is the Markdown beneath the heading, without it, and with
	// blank lines at either end trimmed.
	Notes string
}

// Log is a parsed changelog.
type Log struct {
	sections []Section // newest first
}

// Parse reads a changelog. Text before the first level-2 heading, such
// as the document's title, is dropped, and so are Keep a Changelog's
// link definitions for a version or Unreleased, as
// "[1.2.0]: https://example.com/compare/v1.1.0...v1.2.0", which chat would
// show as they are. A "## " line inside a code fence is not a heading.
//
// Parse fails on a level-2 heading that names neither a version nor
// Unreleased, and on a version with two sections, so a release check
// catches a typo rather than losing the section. The error names the
// line.
func Parse(notes string) (*Log, error) {
	notes = strings.ReplaceAll(notes, "\r\n", "\n")
	var (
		l       Log
		cur     *Section // nil before the first heading and in Unreleased
		body    []string
		fence   string // the open fence's marker, or ""
		seen    = map[string]int{}
		closeUp = func() {
			if cur != nil {
				cur.Notes = trimBlank(body)
				l.sections = append(l.sections, *cur)
			}
			cur, body = nil, nil
		}
	)
	for i, line := range strings.Split(notes, "\n") {
		n := i + 1
		if fence != "" {
			if closesFence(line, fence) {
				fence = ""
			}
			body = append(body, line)
			continue
		}
		if m := opensFence(line); m != "" {
			fence = m
			body = append(body, line)
			continue
		}
		text, ok := heading(line)
		if !ok {
			if !linkDefRE.MatchString(line) {
				body = append(body, line)
			}
			continue
		}
		closeUp()
		v, unreleased := headingVersion(text)
		switch {
		case unreleased:
		case v == "":
			return nil, fmt.Errorf("changelog: line %d: a level-2 heading must name a version, such as v1.2.0, or Unreleased", n)
		case seen[v] != 0:
			return nil, fmt.Errorf("changelog: line %d: %s already has a section, at line %d", n, v, seen[v])
		default:
			seen[v] = n
			cur = &Section{Version: v}
		}
	}
	closeUp()
	slices.SortStableFunc(l.sections, func(a, b Section) int { return Compare(b.Version, a.Version) })
	return &l, nil
}

// Sections returns every version's section, newest first.
func (l *Log) Sections() []Section { return slices.Clone(l.sections) }

// Section returns version's section, in any form Canonical accepts.
func (l *Log) Section(version string) (Section, bool) {
	v := Canonical(version)
	if v == "" {
		return Section{}, false
	}
	for _, s := range l.sections {
		if Compare(s.Version, v) == 0 {
			return s, true
		}
	}
	return Section{}, false
}

// Between returns the sections of the versions above after, up to and
// including upTo, newest first: what changed in an update from after to
// upTo. An after that is empty or not a version is below every version,
// so every section up to upTo is returned. Nothing is returned when upTo
// is not a version or is not above after.
func (l *Log) Between(after, upTo string) []Section {
	var out []Section
	for _, s := range l.sections {
		if Compare(s.Version, after) > 0 && Compare(s.Version, upTo) <= 0 {
			out = append(out, s)
		}
	}
	return out
}

// Covers reports whether notes parse and hold a section for version
// with some text in it, so a bot's release check or test can refuse to
// tag a release that has no notes.
func Covers(notes, version string) bool {
	l, err := Parse(notes)
	if err != nil {
		return false
	}
	s, ok := l.Section(version)
	return ok && s.Notes != ""
}

// heading returns the text of a level-2 ATX heading, which may be
// indented by up to three spaces and closed by a run of #.
func heading(line string) (string, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || !strings.HasPrefix(t, "##") {
		return "", false
	}
	t = t[2:]
	if t != "" && t[0] != ' ' && t[0] != '\t' {
		return "", false
	}
	t = strings.TrimSpace(t)
	if c := strings.TrimRight(t, "#"); c != t && (c == "" || strings.HasSuffix(c, " ")) {
		t = strings.TrimSpace(c)
	}
	return t, true
}

// headingVersion reads the version a heading's text opens with, bare or
// in brackets, as "[1.2.0] - 2026-10-22" or "v1.2.0". It reports
// unreleased for an Unreleased heading, and "" for anything else.
func headingVersion(text string) (v string, unreleased bool) {
	var tok string
	if rest, ok := strings.CutPrefix(text, "["); ok {
		tok, _, ok = strings.Cut(rest, "]")
		if !ok {
			return "", false
		}
	} else {
		tok, _, _ = strings.Cut(text, " ")
	}
	if strings.EqualFold(tok, "unreleased") {
		return "", true
	}
	return Canonical(tok), false
}

// linkDefRE matches a link reference definition for a version or for
// Unreleased.
var linkDefRE = regexp.MustCompile(`(?i)^ {0,3}\[(unreleased|v?\d+\.\d+\.\d+[^\]]*)\]:\s`)

// opensFence returns the marker of a code fence the line opens, or "".
func opensFence(line string) string {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || len(t) < 3 || t[0] != '`' && t[0] != '~' {
		return ""
	}
	n := len(t) - len(strings.TrimLeft(t, t[:1]))
	if n < 3 {
		return ""
	}
	// A backtick fence's info string cannot hold a backtick: such a line
	// is an inline code span.
	if t[0] == '`' && strings.Contains(t[n:], "`") {
		return ""
	}
	return t[:n]
}

// closesFence reports whether line closes the fence opened with marker:
// indented at most three spaces, the same character, at least as many,
// and nothing after.
func closesFence(line, marker string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	t = strings.TrimRight(t, " \t")
	return strings.HasPrefix(t, marker) && strings.Trim(t, marker[:1]) == ""
}

// trimBlank joins lines, dropping blank lines at either end and trailing
// space.
func trimBlank(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}

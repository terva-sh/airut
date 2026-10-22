package airut

import (
	"fmt"
	"strings"

	"terva.sh/airut/changelog"
)

// Changelog is a bot's user-facing release notes, which the built-in
// changelog command shows.
type Changelog struct {
	// Notes is the changelog's Markdown, usually a CHANGELOG.md the bot
	// embeds with go:embed, in the form package changelog reads. It is
	// posted as it is written, so write it for the people in the rooms.
	// Run fails if it does not parse.
	Notes string
	// Heading opens each version's notes, with {version} replaced by the
	// version, as "📚 New in the archive ({version}):". It defaults to
	// "New in {version}:".
	Heading string
}

// defaultChangelogHeading is the Heading of a Changelog that sets none.
const defaultChangelogHeading = "New in {version}:"

// heading returns the line that opens version's notes.
func (c *Changelog) heading(version string) string {
	h := c.Heading
	if h == "" {
		h = defaultChangelogHeading
	}
	return strings.ReplaceAll(h, "{version}", version)
}

// parseChangelog parses Config.Changelog, for Run, which fails on an
// error.
func (b *Bot) parseChangelog() error {
	if b.cfg.Changelog == nil {
		return nil
	}
	l, err := changelog.Parse(b.cfg.Changelog.Notes)
	if err != nil {
		return fmt.Errorf("airut: Config.Changelog: %w", err)
	}
	b.changelog = l
	return nil
}

// versionCommand is the built-in version, which is a tool too.
func versionCommand(b *Bot) *Command {
	v := strings.TrimSpace(b.cfg.Version)
	c, err := newCommand(b, "version", "Show the version of the bot that is running", func(c *Ctx) error {
		_, err := c.Reply("Running " + v + ".")
		return err
	})
	if err != nil {
		panic("airut: the built-in version command: " + err.Error())
	}
	c.tool = true
	return c
}

type changelogArgs struct {
	Version string `airut:",optional" desc:"A version, such as v1.2.0; the running one by default"`
}

// changelogCommand is the built-in changelog, which is a tool too.
func changelogCommand(b *Bot) *Command {
	c, err := newCommand(b, "changelog", "Show what changed in a version of the bot", func(c *Ctx, a changelogArgs) error {
		want := strings.TrimSpace(a.Version)
		if want == "" {
			want = b.defaultChangelogVersion()
			if want == "" {
				return Errorf("The changelog has no versions yet.")
			}
		} else if changelog.Canonical(want) == "" {
			return Errorf("%s is not a version, such as v1.2.0.", want)
		}
		s, ok := b.changelog.Section(want)
		if !ok {
			msg := fmt.Sprintf("There are no notes for %s.", changelog.Canonical(want))
			if all := b.changelog.Sections(); len(all) > 0 {
				msg += fmt.Sprintf(" The newest are for %s.", all[0].Version)
			}
			return Errorf("%s", msg)
		}
		text := b.cfg.Changelog.heading(s.Version)
		if s.Notes != "" {
			text += "\n" + s.Notes
		}
		_, err := c.ReplyPrivate(text)
		return err
	})
	if err != nil {
		panic("airut: the built-in changelog command: " + err.Error())
	}
	c.tool = true
	return c
}

// defaultChangelogVersion is the version changelog shows when it names
// none: the release the running version is or was built from, or the
// newest in the changelog when the bot's version is not set or is not a
// version.
func (b *Bot) defaultChangelogVersion() string {
	if v := changelog.Base(strings.TrimSpace(b.cfg.Version)); v != "" {
		return v
	}
	if all := b.changelog.Sections(); len(all) > 0 {
		return all[0].Version
	}
	return ""
}

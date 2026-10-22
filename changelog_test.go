package airut_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

const notes = `# Changelog

## [Unreleased]

- Not out yet.

## [1.3.0] - 2026-10-22

- The archive keeps your place.

## [1.2.0] - 2026-09-01

- Searches are faster.
`

func specNames(a *airuttest.Adapter) []string {
	var out []string
	for _, s := range a.Specs() {
		out = append(out, s.Name)
	}
	return out
}

func TestVersionCommand(t *testing.T) {
	b, _ := newBot(airut.Config{Version: "v1.3.0-4-gabc1234-dirty"})
	a := airuttest.Start(t, b)
	if got := specNames(a); !slices.Equal(got, []string{"roll", "status", "version"}) {
		t.Fatalf("specs = %q; a Version brings a version command and nothing else", got)
	}
	wantEvents(t, a.Text("!version"), "reply:Running v1.3.0-4-gabc1234-dirty.")
	if tools := b.Tools("version"); len(tools) != 1 {
		t.Fatal("the built-in version is not a tool")
	}

	b, _ = newBot(airut.Config{})
	if got := specNames(airuttest.Start(t, b)); !slices.Equal(got, []string{"roll", "status"}) {
		t.Fatalf("specs = %q; with no Version and no Changelog there are no such commands", got)
	}
}

func TestChangelogCommand(t *testing.T) {
	b, logs := newBot(airut.Config{Version: "v1.3.0", Changelog: &airut.Changelog{Notes: notes, Heading: "📚 New in the archive ({version}):"}})
	a := airuttest.Start(t, b)
	if got := specNames(a); !slices.Equal(got, []string{"changelog", "roll", "status", "version"}) {
		t.Fatalf("specs = %q", got)
	}

	evs := a.Text("!changelog")
	wantEvents(t, evs, "reply:📚 New in the archive (v1.3.0):\n- The archive keeps your place.")
	if len(evs) == 1 && !evs[0].Private {
		t.Error("the changelog was not private")
	}
	evs = a.Slash("changelog", map[string]any{"version": "1.2.0"})
	wantEvents(t, evs, "reply:📚 New in the archive (v1.2.0):\n- Searches are faster.")
	if len(evs) == 1 && !evs[0].Private {
		t.Error("the changelog for a named version was not private")
	}
	wantEvents(t, a.Text("!changelog v9.0.0"), "reply:There are no notes for v9.0.0. The newest are for v1.3.0.")
	wantEvents(t, a.Text("!changelog Unreleased"), "reply:Unreleased is not a version, such as v1.2.0.")
	if tools := b.Tools("changelog"); len(tools) != 1 {
		t.Fatal("the built-in changelog is not a tool")
	}
	if strings.Contains(logs.String(), "Searches are faster") || strings.Contains(logs.String(), "keeps your place") {
		t.Errorf("the log holds the notes:\n%s", logs)
	}
}

func TestChangelogCommandDefaults(t *testing.T) {
	// A build shows the notes of the release it was built from, under
	// the default heading.
	b, _ := newBot(airut.Config{Version: "v1.2.0-3-gabc1234-dirty", Changelog: &airut.Changelog{Notes: notes}})
	wantEvents(t, airuttest.Start(t, b).Text("!changelog"), "reply:New in v1.2.0:\n- Searches are faster.")

	// Without a Version, the newest; and no version command.
	b, _ = newBot(airut.Config{Changelog: &airut.Changelog{Notes: notes}})
	a := airuttest.Start(t, b)
	if got := specNames(a); !slices.Equal(got, []string{"changelog", "roll", "status"}) {
		t.Fatalf("specs = %q", got)
	}
	wantEvents(t, a.Text("!changelog"), "reply:New in v1.3.0:\n- The archive keeps your place.")

	// A running version the notes do not cover says so.
	b, _ = newBot(airut.Config{Version: "v1.4.0", Changelog: &airut.Changelog{Notes: notes}})
	wantEvents(t, airuttest.Start(t, b).Text("!changelog"), "reply:There are no notes for v1.4.0. The newest are for v1.3.0.")

	b, _ = newBot(airut.Config{Changelog: &airut.Changelog{Notes: "# Changelog\n\n## Unreleased\n\n- Soon.\n"}})
	wantEvents(t, airuttest.Start(t, b).Text("!changelog"), "reply:The changelog has no versions yet.")
}

func TestRunFailsOnNotesThatDoNotParse(t *testing.T) {
	b, _ := newBot(airut.Config{Changelog: &airut.Changelog{Notes: "## v1.0.0\n- ok\n## Fixes\n- lost\n"}})
	err := b.Run(context.Background(), airuttest.New(""))
	if err == nil || !strings.Contains(err.Error(), "Config.Changelog") || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("Run = %v, want the notes' error naming the line", err)
	}
	// Nothing started, so the bot can run once the mistake is fixed.
	if err := b.Run(context.Background(), airuttest.New("")); err == nil || strings.Contains(err.Error(), "twice") {
		t.Fatalf("a second Run = %v, want the same notes error again", err)
	}
}

func TestAppVersionAndChangelogCommandsWin(t *testing.T) {
	b, _ := newBot(airut.Config{Version: "v1.3.0", Changelog: &airut.Changelog{Notes: notes}})
	b.Command("version", "The bot's own version", func(c *airut.Ctx) error {
		_, err := c.Reply("the bot's own version")
		return err
	})
	b.Command("changelog", "The bot's own changelog", func(c *airut.Ctx) error {
		_, err := c.Reply("the bot's own changelog")
		return err
	})
	a := airuttest.Start(t, b)
	wantEvents(t, a.Text("!version"), "reply:the bot's own version")
	wantEvents(t, a.Text("!changelog"), "reply:the bot's own changelog")
}

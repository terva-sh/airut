package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func TestAgentIsAVersionStore(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	airuttest.VersionStoreContract(t, e.agent)
}

// A bot whose Agent is a VersionStore needs no Store of its own: its
// first run records the version in the session database, and a later
// one on a newer release announces what changed.
func TestChangelogRecordedInTheSessionDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	ch := airut.Channel{Platform: airuttest.Platform, ID: "news"}
	notes := "## v1.1.0\n- Faster.\n\n## v1.0.0\n- First.\n"
	run := func(version string) (sends []airuttest.Event) {
		// Each run is a subtest, so its bot stops before the next starts.
		t.Run(version, func(t *testing.T) {
			sends = runOnce(t, path, version, notes, ch)
		})
		return sends
	}
	if s := run("v1.0.0"); len(s) != 0 {
		t.Errorf("the first run posted %+v", s)
	}
	if s := run("v1.1.0"); len(s) != 1 || s[0].Channel != "news" || s[0].Text != "New in v1.1.0:\n- Faster." {
		t.Errorf("the update posted %+v", s)
	}
}

// runOnce runs a bot at version on the session database at path until
// its announcement in ch is recorded, and returns what it posted.
func runOnce(t *testing.T, path, version, notes string, ch airut.Channel) []airuttest.Event {
	db := openDB(t, path)
	e := newEnv(t, db, nil, airut.Config{Version: version, Changelog: &airut.Changelog{Notes: notes, Channels: []airut.Channel{ch}}})
	a := airuttest.Start(t, e.bot)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, _ := e.agent.AnnouncedVersion(context.Background(), ch); v == version {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never recorded", version)
		}
		time.Sleep(time.Millisecond)
	}
	var sends []airuttest.Event
	for _, ev := range a.Events() {
		if ev.Kind == "send" {
			sends = append(sends, ev)
		}
	}
	return sends
}

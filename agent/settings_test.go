package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func lastReply(evs []airuttest.Event) string {
	if len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].Text
}

// A setting changed from chat is kept in the session database, so a bot
// started again on it has the override and its history.
func TestSettingsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	owner := airut.User{Platform: airuttest.Platform, ID: "user-1", Name: "Olive"}
	cfg := airut.Config{Settings: airut.Settings{Owner: func(c *airut.Ctx) (bool, error) { return c.Caller().ID == "user-1", nil }}}
	start := func() (*airuttest.Adapter, *airut.Setting[time.Duration]) {
		e := newEnv(t, openDB(t, path), nil, cfg)
		s := e.bot.DeclareDuration(airut.DurationSetting{Name: "wait", Description: "How long to wait",
			Default: time.Minute, Min: time.Second, Max: time.Hour})
		return airuttest.Start(t, e.bot), s
	}

	a, wait := start()
	if got := lastReply(a.Text("!set wait 5m", airuttest.As(owner))); got != "wait: 1m → 5m." {
		t.Fatalf("set: %q", got)
	}
	if wait.Get() != 5*time.Minute {
		t.Fatalf("wait = %v", wait.Get())
	}

	a, wait = start()
	if wait.Get() != 5*time.Minute {
		t.Errorf("wait = %v after a restart, want the stored 5m", wait.Get())
	}
	got := lastReply(a.Text("!settings wait"))
	if !strings.Contains(got, "wait = 5m, from the override.") || !strings.Contains(got, " by Olive: 1m → 5m") {
		t.Errorf("settings wait after a restart:\n%s", got)
	}
}

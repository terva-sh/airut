package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// botSettings is a bot's own settings, as a program built on airut
// declares them.
type botSettings struct {
	MainRoom string `toml:"main_room"`
	ModRoom  string `toml:"mod_room"`
	Booru    struct {
		URL   string `toml:"url"`
		User  string `toml:"user"`
		Token Secret `toml:"token"`
	} `toml:"booru"`
	Tagger *struct {
		URL string `toml:"url"`
		Key Secret `toml:"key"`
	} `toml:"tagger"`
	Backfill struct {
		OpenReviews int     `toml:"open_reviews"`
		Spacing     float64 `toml:"spacing_seconds"`
		Enabled     bool    `toml:"enabled"`
	} `toml:"backfill"`
	Watched  []string           `toml:"watched"`
	Mirrors  []mirror           `toml:"mirrors"`
	Pair     [2]mirror          `toml:"pair"`
	Limits   map[string]int     `toml:"limits"`
	Accounts map[string]account `toml:"accounts"`
	ignored  string
}

type mirror struct {
	URL string `toml:"url"`
	Key Secret `toml:"key"`
}

type account struct {
	Password Secret `toml:"password"`
}

const matrixOnly = "[matrix]\nhomeserver = \"https://matrix.example.org\"\nuser_id = \"@bot:example.org\"\nstate_dir = \"/var/lib/bot\"\n"

func loadApp(t *testing.T, src string, app any) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "airut.toml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mirror-key"), []byte(sentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadApp(path, app)
}

func TestLoadApp(t *testing.T) {
	credentials(t, "booru-token", "tagger-key")
	t.Setenv("ALICE_PW", sentinel)
	var s botSettings
	c, err := loadApp(t, matrixOnly+`
[app]
main_room = "!main:example.org"
mod_room = "!mods:example.org"
watched = ["!a:example.org", "!b:example.org"]
mirrors = [{ url = "https://m.example.org", key = { file = "mirror-key" } }]
pair = [{ url = "https://p.example.org", key = { file = "mirror-key" } }]
limits = { per_hour = 30 }

[app.booru]
url = "https://booru.example.org"
user = "Bot"
token = { credential = "booru-token" }

[app.tagger]
url = "http://tagger.example.org:8765"
key = { credential = "tagger-key" }

[app.backfill]
open_reviews = 10
spacing_seconds = 2.5
enabled = true

[app.accounts.alice]
password = { env = "ALICE_PW" }
`, &s)
	if err != nil {
		t.Fatal(err)
	}
	if c.Matrix == nil || c.Matrix.UserID != "@bot:example.org" {
		t.Errorf("the airut part of the file was not read: %+v", c.Matrix)
	}
	if s.MainRoom != "!main:example.org" || s.ModRoom != "!mods:example.org" || s.Booru.URL != "https://booru.example.org" || s.Booru.User != "Bot" {
		t.Errorf("settings = %+v", s)
	}
	if s.Tagger == nil || s.Tagger.URL != "http://tagger.example.org:8765" {
		t.Errorf("tagger = %+v", s.Tagger)
	}
	if s.Backfill.OpenReviews != 10 || s.Backfill.Spacing != 2.5 || !s.Backfill.Enabled {
		t.Errorf("backfill = %+v", s.Backfill)
	}
	if !reflect.DeepEqual(s.Watched, []string{"!a:example.org", "!b:example.org"}) || s.Limits["per_hour"] != 30 {
		t.Errorf("watched %v, limits %v", s.Watched, s.Limits)
	}
	for name, sec := range map[string]Secret{
		"booru token": s.Booru.Token, "tagger key": s.Tagger.Key,
		"mirror key": s.Mirrors[0].Key, "alice's password": s.Accounts["alice"].Password,
		"pair key": s.Pair[0].Key,
	} {
		if sec.Value() != sentinel {
			t.Errorf("the %s read as %q", name, sec.Value())
		}
	}
	if out := fmt.Sprintf("%v %+v %#v", s, s, s); strings.Contains(out, sentinel) {
		t.Errorf("printing the settings showed a secret: %s", out)
	}
}

func TestLoadAppProblems(t *testing.T) {
	t.Setenv("PLAIN", sentinel)
	var s botSettings
	_, err := loadApp(t, `
[bot]
prefix = "! "
[app]
main_room = 7
colour = "blue"
watched = "!a:example.org"
mirrors = [{ url = "https://m.example.org", key = "`+sentinel+`" }, { key = { env = "UNSET_FOR_THIS_TEST" } }]
limits = { per_hour = "many" }
pair = [{ url = "https://p.example.org", typo = true }, {}, {}]
[app.booru]
token = "`+sentinel+`"
extra = 1
[app.backfill]
open_reviews = 1.5
enabled = "yes"
`, &s)
	got := problemKeys(t, err)
	want := []string{
		"(file)", // no platform
		"app.backfill.enabled", "app.backfill.open_reviews",
		"app.booru.extra", "app.booru.token",
		"app.colour", "app.limits.per_hour", "app.main_room",
		"app.mirrors.0.key", "app.mirrors.1.key", "app.pair",
		"app.watched", "bot.prefix",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("problems at\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("a problem showed a secret: %v", err)
	}
	if !strings.Contains(err.Error(), "app.booru.token: a secret is never written in the file") {
		t.Errorf("a plain-string secret was not refused with the reason:\n%v", err)
	}
}

func TestLoadAppWithoutAnAppTable(t *testing.T) {
	s := botSettings{MainRoom: "unchanged"}
	if _, err := loadApp(t, matrixOnly, &s); err != nil || s.MainRoom != "unchanged" {
		t.Errorf("LoadApp without [app] = %v, settings %+v", err, s)
	}
	for _, bad := range []any{nil, s, new(int), (*botSettings)(nil)} {
		if _, err := LoadApp("unused.toml", bad); err == nil || !strings.Contains(err.Error(), "pointer to a struct") {
			t.Errorf("LoadApp(%T) = %v", bad, err)
		}
	}
}

func TestLoadRefusesAnAppTable(t *testing.T) {
	_, err := load(t, matrixOnly+"[app]\nmain_room = \"!main:example.org\"\n")
	if got := problemKeys(t, err); !reflect.DeepEqual(got, []string{"app"}) {
		t.Errorf("problems at %q, want app", got)
	}
}

func TestAnAppThatIsNotATableIsReportedWithTheRest(t *testing.T) {
	var s botSettings
	_, err := loadApp(t, "app = 7\n", &s)
	if got := problemKeys(t, err); !reflect.DeepEqual(got, []string{"(file)", "app"}) {
		t.Errorf("problems at %q, want the missing platform and app together", got)
	}
}

func TestFixedSizeArrays(t *testing.T) {
	var s botSettings
	_, err := loadApp(t, matrixOnly+"[app]\npair = [{ url = \"https://p.example.org\", typo = true }]\n", &s)
	if got := problemKeys(t, err); !reflect.DeepEqual(got, []string{"app.pair.0.typo"}) {
		t.Errorf("problems at %q, want the unknown key inside the array", got)
	}
	_, err = loadApp(t, matrixOnly+"[app]\npair = [{}, {}, {}]\n", &s)
	if got := problemKeys(t, err); !reflect.DeepEqual(got, []string{"app.pair"}) {
		t.Errorf("problems at %q, want the overlong array", got)
	}
}

package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadOwners(t *testing.T, src string) (*Owners, string, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "airut.toml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := LoadOwners(path)
	return o, dir, err
}

// LoadOwners reads what airut claim needs from a file Load would refuse
// here: one with an [app] table and secrets nobody can read, as on a
// host that cannot read the bot's.
func TestLoadOwners(t *testing.T) {
	o, dir, err := loadOwners(t, `
[bot]
owners = { matrix = ["@you:example.org"], discord = ["123"] }

[discord]
token = { file = "/nonexistent/token" }

[secrets]
key = { file = "/nonexistent/age.key" }

[agent]
model = "local/m"

[store]
path = "sessions.db"
retention = "48h"

[app]
anything = "goes"
`)
	if err != nil {
		t.Fatalf("LoadOwners: %v", err)
	}
	want := map[string][]string{"matrix": {"@you:example.org"}, "discord": {"123"}}
	if !reflect.DeepEqual(o.Config, want) {
		t.Errorf("Config = %q, want %q", o.Config, want)
	}
	if o.Store.Path != filepath.Join(dir, "sessions.db") || o.Store.Retention.Hours() != 48 {
		t.Errorf("Store = %+v", o.Store)
	}

	// Without an [agent] there is no store.
	o, _, err = loadOwners(t, minimal)
	if err != nil || o.Store.Path != "" || o.Config != nil {
		t.Errorf("a file with no [agent] = %+v, %v", o, err)
	}
}

func TestLoadOwnersRefuses(t *testing.T) {
	for _, c := range []struct {
		name, src string
		keys      []string
	}{
		{"a bad owner", minimal + "[bot]\nowners = { matrix = [\"you\"], irc = [\"x\"] }\n", []string{"bot.owners.irc", "bot.owners.matrix.0"}},
		{"a store with no agent", minimal + "[store]\npath = \"s.db\"\n", []string{"store"}},
		{"an agent with no store", minimal + "[agent]\nmodel = \"local/m\"\n", []string{"store.path"}},
	} {
		_, _, err := loadOwners(t, c.src)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%s: %v, want an *Error", c.name, err)
			continue
		}
		var keys []string
		for _, p := range e.Problems {
			keys = append(keys, p.Key)
		}
		if !reflect.DeepEqual(keys, c.keys) {
			t.Errorf("%s: problems at %q, want %q", c.name, keys, c.keys)
		}
		if strings.Contains(err.Error(), "\"you\"") {
			t.Errorf("%s: the error quotes an id: %v", c.name, err)
		}
	}
	if _, err := LoadOwners(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Error("a missing file loaded")
	}
}

func TestCheckOwnerID(t *testing.T) {
	for _, c := range []struct {
		platform, id string
		ok           bool
	}{
		{"matrix", "@you:example.org", true},
		{"matrix", "you", false},
		{"matrix", "123", false},
		{"discord", "123456789012345678", true},
		{"discord", "@you:example.org", false},
		{"irc", "you", false},
	} {
		if err := CheckOwnerID(c.platform, c.id); (err == nil) != c.ok {
			t.Errorf("CheckOwnerID(%s, %s) = %v, want ok %v", c.platform, c.id, err, c.ok)
		}
	}
}

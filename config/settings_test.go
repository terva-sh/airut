package config

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestSettings(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	c, err := load(t, minimal+`
[settings]
quiet = true
greeting = "good day"
search.mode = "all"
# Two keys named as a reference's are settings, not a secret.
deploy = { env = "prod", file = "big" }
[settings.proposals]
max_open = 12
expire_after = "72h"
`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"quiet": "true", "greeting": "good day", "search.mode": "all",
		"proposals.max_open": "12", "proposals.expire_after": "72h", "deploy.env": "prod", "deploy.file": "big",
	}
	if !maps.Equal(c.Settings, want) {
		t.Errorf("settings = %v, want %v", c.Settings, want)
	}
	if c, err := load(t, minimal); err != nil || c.Settings != nil {
		t.Errorf("without [settings]: %v, %v", c.Settings, err)
	}
}

func TestSettingsProblems(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	_, err := load(t, minimal+`
[settings]
ratio = 1.5
rooms = ["a"]
token = { env = "AIRUT_TEST_TOKEN" }
nested.key = { file = "`+sentinel+`" }
`)
	keys := problemKeys(t, err)
	if want := []string{"settings.nested.key", "settings.ratio", "settings.rooms", "settings.token"}; !slices.Equal(keys, want) {
		t.Errorf("problems under %v, want %v", keys, want)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("a problem quoted the file: %v", err)
	}
	if !strings.Contains(err.Error(), "a setting never holds a secret") {
		t.Errorf("a secret reference was not refused as one: %v", err)
	}
	if _, err := load(t, "settings = 3\n"+minimal); !slices.Equal(problemKeys(t, err), []string{"settings"}) {
		t.Errorf("a [settings] that is not a table: %v", err)
	}
}

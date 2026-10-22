package claim_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terva.sh/airut/agent/claim"
	"terva.sh/airut/agent/sessions"
)

// botConfig writes a config file with an [agent], a [store] beside it,
// an [app] table, and secrets that cannot be read, which claim must not
// need. It returns the file and the store's path.
func botConfig(t *testing.T, extra string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.toml")
	src := `
[bot]
owners = { matrix = ["@listed:example.org"] }

[matrix]
homeserver = "https://matrix.example.org"
user_id = "@bot:example.org"
password = { file = "/nonexistent/password" }
state_dir = "state"

[agent]
model = "local/m"

[store]
path = "sessions.db"

[app]
greeting = "hello"
` + extra
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, "sessions.db")
}

type result struct {
	code           int
	stdout, stderr string
}

func run(c claim.Command, args ...string) result {
	var out, errb bytes.Buffer
	code := c.Run(args, &out, &errb)
	return result{code, out.String(), errb.String()}
}

func want(t *testing.T, what string, r result, code int, stdout, stderr string) {
	t.Helper()
	if r.code != code || !strings.Contains(r.stdout, stdout) || !strings.Contains(r.stderr, stderr) ||
		stdout == "" && r.stdout != "" || stderr == "" && r.stderr != "" {
		t.Errorf("%s = %d\nstdout: %q\nstderr: %q\nwant %d, stdout with %q, stderr with %q", what, r.code, r.stdout, r.stderr, code, stdout, stderr)
	}
}

func claims(t *testing.T, store string) []string {
	t.Helper()
	db, err := sessions.Open(context.Background(), store, sessions.Options{NoSweep: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, err := db.OwnerClaims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range list {
		out = append(out, c.Platform+" "+c.ID+" by "+c.Actor)
	}
	return out
}

func TestClaimAndUnclaim(t *testing.T) {
	path, store := botConfig(t, "")
	c := claim.Command{}

	want(t, "claim", run(c, "claim", "-config", path, "-actor", "ops", "matrix", "@you:example.org"), 0,
		"claimed matrix @you:example.org in "+store, "")
	// The platform may be left out when the id names it.
	want(t, "claim by the id's form", run(c, "claim", "-config", path, "-actor", "ops", "123456789012345678"), 0,
		"claimed discord 123456789012345678", "")
	want(t, "claim again", run(c, "claim", "-config", path, "@you:example.org"), 0,
		"matrix @you:example.org was already claimed at ", "")
	want(t, "claim a listed owner", run(c, "claim", "-config", path, "@listed:example.org"), 0,
		"matrix @listed:example.org is already an owner in [bot] owners; nothing to claim", "")
	if got := strings.Join(claims(t, store), "; "); got != "discord 123456789012345678 by ops; matrix @you:example.org by ops" {
		t.Errorf("claims = %q", got)
	}

	want(t, "unclaim", run(c, "unclaim", "-config", path, "matrix", "@you:example.org"), 0,
		"unclaimed matrix @you:example.org; the running bot drops it within a minute", "")
	want(t, "unclaim again", run(c, "unclaim", "-config", path, "@you:example.org"), 0,
		"matrix @you:example.org has no claim; nothing changed", "")
	want(t, "unclaim a listed owner", run(c, "unclaim", "-config", path, "@listed:example.org"), 1,
		"", "@listed:example.org is an owner in [bot] owners; edit [bot] owners to remove them")
	if got := strings.Join(claims(t, store), "; "); got != "discord 123456789012345678 by ops" {
		t.Errorf("claims after unclaiming = %q", got)
	}
}

// A claim made before its owner was listed in [bot] owners is removed
// by unclaim, which still refuses, since the list keeps them an owner.
func TestUnclaimAListedOwnerRemovesTheirClaim(t *testing.T) {
	path, store := botConfig(t, "")
	db, err := sessions.Open(context.Background(), store, sessions.Options{NoSweep: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimOwner(context.Background(), "matrix", "@listed:example.org", "ops"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	want(t, "unclaim", run(claim.Command{}, "unclaim", "-config", path, "@listed:example.org"), 1,
		"removed the claim on matrix @listed:example.org", "edit [bot] owners to remove them")
	if got := claims(t, store); len(got) != 0 {
		t.Errorf("claims = %q", got)
	}
}

func TestClaimWithoutAStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.toml")
	src := "[bot]\nowners = { discord = [\"42\"] }\n[discord]\ntoken = { env = \"NO_SUCH_TOKEN\" }\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	c := claim.Command{}
	want(t, "claim", run(c, "claim", "-config", path, "@you:example.org"), 1, "",
		"airut claim: this bot has no [store]; list owners in [bot] owners")
	want(t, "unclaim", run(c, "unclaim", "-config", path, "@you:example.org"), 1, "",
		"airut unclaim: this bot has no [store]; list owners in [bot] owners")
	want(t, "unclaim a listed owner", run(c, "unclaim", "-config", path, "42"), 1, "", "edit [bot] owners")
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the commands left files beside the config: %v", entries)
	}
}

// unclaim against a store that is not there yet makes none.
func TestUnclaimMakesNoStore(t *testing.T) {
	path, store := botConfig(t, "")
	want(t, "unclaim", run(claim.Command{}, "unclaim", "-config", path, "@you:example.org"), 0,
		"has no claim; nothing changed", "")
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("unclaim made the store: %v", err)
	}
}

func TestClaimRefusesBadArguments(t *testing.T) {
	path, store := botConfig(t, "")
	c := claim.Command{}
	for _, tc := range []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"no command", nil, 2, "usage: airut claim"},
		{"another command", []string{"seal"}, 2, "usage: airut claim"},
		{"no -config", []string{"claim", "@you:example.org"}, 2, "-config is required"},
		{"no user", []string{"claim", "-config", path}, 2, "USER is required"},
		{"a bad matrix id", []string{"claim", "-config", path, "matrix", "you"}, 2, "USER is not a Matrix user id"},
		{"a bad discord id", []string{"claim", "-config", path, "discord", "@you:example.org"}, 2, "USER is not a Discord user id"},
		{"an id of no form", []string{"claim", "-config", path, "you"}, 2, "neither a Matrix id"},
		{"a bad platform", []string{"claim", "-config", path, "irc", "you"}, 2, "PLATFORM is not a platform"},
		{"too many", []string{"claim", "-config", path, "matrix", "@a:example.org", "@b:example.org"}, 2, "too many arguments"},
		{"an unknown flag", []string{"claim", "-config", path, "-x", "@you:example.org"}, 2, "not defined"},
		{"unclaim takes no actor", []string{"unclaim", "-config", path, "-actor", "ops", "@you:example.org"}, 2, "not defined"},
		{"a missing config", []string{"claim", "-config", path + ".missing", "@you:example.org"}, 1, "no such file"},
	} {
		want(t, tc.name, run(c, tc.args...), tc.code, "", tc.stderr)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("a refused command made the store: %v", err)
	}

	// A bad config names its problem.
	bad, _ := botConfig(t, "")
	src, _ := os.ReadFile(bad)
	os.WriteFile(bad, bytes.Replace(src, []byte(`["@listed:example.org"]`), []byte(`["listed"]`), 1), 0o600)
	want(t, "a bad config", run(c, "claim", "-config", bad, "@you:example.org"), 1, "", "bot.owners.matrix.0")
}

// A mounting program names itself, and its config file is the default.
func TestAMountedCommand(t *testing.T) {
	path, store := botConfig(t, "")
	c := claim.Command{Prog: "mybot", Config: path}
	want(t, "claim", run(c, "claim", "@you:example.org"), 0, "claimed matrix @you:example.org", "")
	want(t, "usage", run(c, "claim"), 2, "", "usage: mybot claim")
	want(t, "usage", run(c, "claim"), 2, "", "-config defaults to "+path)
	want(t, "help", run(c, "claim", "-h"), 0, "usage: mybot claim", "")
	if got := claims(t, store); len(got) != 1 {
		t.Errorf("claims = %q", got)
	}
}

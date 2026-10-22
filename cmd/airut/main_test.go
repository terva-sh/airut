package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"terva.sh/airut/config"
)

// sentinel stands for a secret. No output but the sealed field may hold
// it, and that only encrypted.
const sentinel = "sk-SENTINEL-do-not-print"

func runSeal(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(append([]string{"seal"}, args...), strings.NewReader(stdin), &out, &errOut)
	if strings.Contains(out.String()+errOut.String(), sentinel) {
		t.Errorf("airut seal printed the value: %q %q", out.String(), errOut.String())
	}
	return code, out.String(), errOut.String()
}

// TestSealRoundTrip seals a value as an operator would and loads it back
// from a configuration that names the key.
func TestSealRoundTrip(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	old, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runSeal(t, sentinel+"\n", "--recipient", id.Recipient().String(), "--recipient", old.Recipient().String(), "--at", "discord.token")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	field := strings.TrimSuffix(out, "\n")
	if strings.ContainsAny(field, " \n") || !strings.HasPrefix(field, "enc:age:v2:") {
		t.Fatalf("not one sealed field: %q", out)
	}

	dir := t.TempDir()
	for _, k := range []*age.X25519Identity{id, old} {
		if err := os.WriteFile(filepath.Join(dir, "age.key"), []byte(k.String()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "airut.toml")
		src := "[secrets]\nkey = { file = \"age.key\" }\n[discord]\ntoken = { sealed = \"" + field + "\" }\n"
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.Discord.Token.Value() != sentinel {
			t.Error("the sealed value did not open to what was piped in, without its line ending")
		}
	}
}

func TestSealRefuses(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r := id.Recipient().String()
	for _, c := range []struct {
		name, stdin string
		args        []string
		code        int
		want        string
	}{
		{"no input", "", []string{"--recipient", r, "--at", "discord.token"}, 1, "no value on stdin"},
		{"only a line ending", "\n", []string{"--recipient", r, "--at", "discord.token"}, 1, "no value on stdin"},
		{"too long", strings.Repeat("x", maxValue+1), []string{"--recipient", r, "--at", "discord.token"}, 1, "longer than"},
		{"not text", "\xff\xfe", []string{"--recipient", r, "--at", "discord.token"}, 1, "not valid UTF-8"},
		{"already sealed", "enc:age:v2:abc", []string{"--recipient", r, "--at", "discord.token"}, 1, "already sealed"},
		{"no recipient", sentinel, []string{"--at", "discord.token"}, 2, "--recipient is required"},
		{"no key path", sentinel, []string{"--recipient", r}, 2, "--at is required"},
		{"a key where the recipient goes", sentinel, []string{"--recipient", id.String(), "--at", "discord.token"}, 2, "--recipient 1 is not"},
		{"a value as an argument", "", []string{"--recipient", r, "--at", "discord.token", sentinel}, 2, "read from stdin"},
		{"an unknown key path", sentinel, []string{"--recipient", r, "--at", "discord.tokn"}, 2, "no secret is read from"},
		{"the key itself", sentinel, []string{"--recipient", r, "--at", "secrets.key"}, 2, "cannot itself be sealed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := runSeal(t, c.stdin, c.args...)
			if code != c.code || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, stderr %q; want exit %d and %q", code, errOut, c.code, c.want)
			}
			if out != "" {
				t.Errorf("printed %q", out)
			}
			if strings.Contains(errOut, id.String()) {
				t.Error("stderr carries the key")
			}
		})
	}

	var errOut bytes.Buffer
	if code := run(nil, strings.NewReader(""), &bytes.Buffer{}, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage: airut seal") {
		t.Errorf("no command: exit %d, %q", code, errOut.String())
	}
}

// TestSealHidesBadArguments passes key material where a flag goes. The
// flag package would name it; airut seal names only its position.
func TestSealHidesBadArguments(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	key, r := id.String(), id.Recipient().String()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-" + key}, "argument 1 is not a flag"},
		{[]string{"--recipient", r, "--" + key, "--at", "discord.token"}, "argument 3 is not a flag"},
		{[]string{"--at", "discord.token", "-" + key + "=x"}, "argument 3 is not a flag"},
		{[]string{"--" + sentinel}, "argument 1 is not a flag"},
		{[]string{"--at", "discord.token", "--recipient"}, "argument 3 is a flag with no value"},
	} {
		code, out, errOut := runSeal(t, sentinel, c.args...)
		if code != 2 || !strings.Contains(errOut, c.want) {
			t.Errorf("exit %d, stderr %q; want exit 2 and %q", code, errOut, c.want)
		}
		if strings.Contains(out+errOut, key) || strings.Contains(out+errOut, key[len("AGE-SECRET-KEY-1"):]) {
			t.Errorf("the key reached the output: %q %q", out, errOut)
		}
	}
	if code, out, errOut := runSeal(t, "", "-h"); code != 0 || out != "" || !strings.Contains(errOut, "usage: airut seal") {
		t.Errorf("-h: exit %d, %q %q", code, out, errOut)
	}
}

// TestSealQuotedKeyRoundTrip seals for a map key with a dot in it, which
// the key path quotes, and opens it there and nowhere else.
func TestSealQuotedKeyRoundTrip(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	at := `app.tokens."a.b".key`
	code, out, errOut := runSeal(t, sentinel, "--recipient", id.Recipient().String(), "--at", at)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	field := strings.TrimSpace(out)
	var app struct {
		Tokens map[string]map[string]config.Secret `toml:"tokens"`
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "age.key"), []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := "[secrets]\nkey = { file = \"age.key\" }\n[discord]\ntoken = { file = \"age.key\" }\n"
	load := func(table, key string) error {
		path := filepath.Join(dir, "airut.toml")
		if err := os.WriteFile(path, []byte(head+"[app.tokens."+table+"]\n"+key+" = { sealed = \""+field+"\" }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := config.LoadApp(path, &app)
		return err
	}
	if err := load(`"a.b"`, "key"); err != nil {
		t.Fatal(err)
	}
	if app.Tokens["a.b"]["key"].Value() != sentinel {
		t.Error("the value did not open at the quoted key")
	}
	// tokens.a."b.key" joins to the same dotted text and is another place.
	if err := load("a", `"b.key"`); err == nil || !strings.Contains(err.Error(), "moved from another key") || strings.Contains(err.Error(), sentinel) {
		t.Errorf(`at tokens.a."b.key": %v`, err)
	}
}

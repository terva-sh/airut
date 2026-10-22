package config

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"terva.sh/seal"
)

// newIdentity makes an age identity for one test. No key is checked in.
func newIdentity(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// sealFor seals sentinel for key to id's recipient, as airut seal does.
func sealFor(t *testing.T, id *age.X25519Identity, key string) string {
	t.Helper()
	field, err := Seal(key, sentinel, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	return field
}

// keyEnv puts id in an environment variable of its own, as an
// age-keygen file would hold it, and returns the [secrets] table that
// names it.
func keyEnv(t *testing.T, id *age.X25519Identity) string {
	t.Helper()
	name := "AIRUT_TEST_AGE_KEY_" + strings.ToUpper(id.Recipient().String()[4:16])
	t.Setenv(name, "# created: for a test\n# public key: "+id.Recipient().String()+"\n"+id.String()+"\n")
	return "[secrets]\nkey = { env = \"" + name + "\" }\n"
}

// noLeak fails if text carries the sealed value or any part of the key.
func noLeak(t *testing.T, what, text string, ids ...*age.X25519Identity) {
	t.Helper()
	if strings.Contains(text, sentinel) {
		t.Errorf("%s carries the sealed value: %s", what, text)
	}
	for _, id := range ids {
		if s := id.String(); strings.Contains(text, s[len("AGE-SECRET-KEY-1"):len("AGE-SECRET-KEY-1")+16]) {
			t.Errorf("%s carries part of the key: %s", what, text)
		}
	}
}

func sealedField(field string) string { return `{ sealed = "` + field + `" }` }

func TestSealedReferences(t *testing.T) {
	id := newIdentity(t)
	src := keyEnv(t, id) + `
[providers.cpa]
base_url = "https://llm.example.org/v1"
api_key = ` + sealedField(sealFor(t, id, "providers.cpa.api_key")) + `
models = [{ id = "m" }]

[discord]
token = ` + sealedField(sealFor(t, id, "discord.token")) + `

[matrix]
homeserver = "https://matrix.example.org"
user_id = "@bot:example.org"
state_dir = "m"
password = ` + sealedField(sealFor(t, id, "matrix.password")) + `
recovery_key = ` + sealedField(sealFor(t, id, "matrix.recovery_key")) + "\n"
	c, err := load(t, src)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Secret{
		"provider": c.Providers[0].APIKey, "discord": c.Discord.Token,
		"matrix password": c.Matrix.Password, "recovery key": c.Matrix.RecoveryKey,
	} {
		if s.Value() != sentinel {
			t.Errorf("%s did not open to the value sealed for it", name)
		}
	}

	// An opened value is a Secret like any other: it prints redacted.
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("loaded", "config", c, "token", c.Discord.Token)
	noLeak(t, "the log", logged.String()+fmt.Sprintf("%v %+v %#v", c, *c, c.Discord), id)
}

func TestSealedKeyKinds(t *testing.T) {
	id := newIdentity(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "age.key"), []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := t.TempDir()
	if err := os.WriteFile(filepath.Join(creds, "airut-age-key"), []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsDirEnv, creds)
	token := "[discord]\ntoken = " + sealedField(sealFor(t, id, "discord.token")) + "\n"
	for _, ref := range []string{`{ file = "age.key" }`, `{ credential = "airut-age-key" }`} {
		path := filepath.Join(dir, "airut.toml")
		if err := os.WriteFile(path, []byte("[secrets]\nkey = "+ref+"\n"+token), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil || c.Discord.Token.Value() != sentinel {
			t.Errorf("key %s: %v", ref, err)
		}
	}
}

func TestSealedAppTable(t *testing.T) {
	id := newIdentity(t)
	src := keyEnv(t, id) + matrixOnly + `
[app.booru]
token = ` + sealedField(sealFor(t, id, "app.booru.token")) + `

[[app.mirrors]]
url = "https://m.example.org"
key = ` + sealedField(sealFor(t, id, "app.mirrors[0].key")) + `

[app.accounts.alice]
password = ` + sealedField(sealFor(t, id, "app.accounts.alice.password")) + "\n"
	var s botSettings
	if _, err := loadApp(t, src, &s); err != nil {
		t.Fatal(err)
	}
	if s.Booru.Token.Value() != sentinel || s.Mirrors[0].Key.Value() != sentinel || s.Accounts["alice"].Password.Value() != sentinel {
		t.Error("an [app] value did not open at its own key path")
	}

	// The same value one index or one map key over does not open.
	moved := keyEnv(t, id) + matrixOnly + `
[[app.mirrors]]
url = "https://a.example.org"
[[app.mirrors]]
url = "https://b.example.org"
key = ` + sealedField(sealFor(t, id, "app.mirrors[0].key")) + `

[app.accounts.bob]
password = ` + sealedField(sealFor(t, id, "app.accounts.alice.password")) + "\n"
	var s2 botSettings
	_, err := loadApp(t, moved, &s2)
	if got := strings.Join(problemKeys(t, err), " "); got != "app.accounts.bob.password app.mirrors[1].key" {
		t.Errorf("problems at %s", got)
	}
	noLeak(t, "the error", err.Error(), id)
}

// TestSealedProblems checks each way a sealed value fails to open: each
// is a problem at the key, saying how, and none carries the value or the
// key that opens it.
func TestSealedProblems(t *testing.T) {
	id, other := newIdentity(t), newIdentity(t)
	tokenFor := func(field string) string { return "[discord]\ntoken = " + sealedField(field) + "\n" }
	foreign, err := seal.Seal(seal.Binding("terva", "discord.token"), sentinel, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	setting, err := seal.Seal(SealBinding("settings.x"), sentinel, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIRUT_TEST_NOT_A_KEY", sentinel)

	for _, c := range []struct {
		name, src string
		keys      []string
		want      string
	}{
		{"moved from another key", keyEnv(t, id) + tokenFor(sealFor(t, id, "matrix.password")),
			[]string{"discord.token"}, `moved from another key: it was sealed for "matrix.password"`},
		{"moved between providers", keyEnv(t, id) + minimal + `
[providers.cpa]
base_url = "https://llm.example.org/v1"
api_key = ` + sealedField(sealFor(t, id, "providers.other.api_key")) + `
models = [{ id = "m" }]
`, []string{"providers.cpa.api_key"}, `sealed for "providers.other.api_key"`},
		{"sealed by another program", keyEnv(t, id) + tokenFor(foreign),
			[]string{"discord.token"}, "sealed for a program other than airut"},
		{"no key", minimal + `
[providers.cpa]
base_url = "https://llm.example.org/v1"
api_key = ` + sealedField(sealFor(t, id, "providers.cpa.api_key")) + `
models = [{ id = "m" }]
[matrix]
homeserver = "https://matrix.example.org"
user_id = "@bot:example.org"
state_dir = "m"
password = ` + sealedField(sealFor(t, id, "matrix.password")) + "\n",
			[]string{"matrix.password", "providers.cpa.api_key"}, "needs the key that opens it, named as [secrets] key"},
		{"a wrong key", keyEnv(t, other) + tokenFor(sealFor(t, id, "discord.token")),
			[]string{"discord.token"}, "secrets.key does not open"},
		{"an unreadable key", "[secrets]\nkey = { env = \"AIRUT_TEST_UNSET\" }\n" + tokenFor(sealFor(t, id, "discord.token")),
			[]string{"discord.token", "secrets.key"}, "secrets.key could not be read"},
		{"a key that is not one", "[secrets]\nkey = { env = \"AIRUT_TEST_NOT_A_KEY\" }\n" + tokenFor(sealFor(t, id, "discord.token")),
			[]string{"discord.token", "secrets.key"}, "not an age identity"},
		{"a sealed key", "[secrets]\nkey = " + sealedField(sealFor(t, id, "discord.token")) + "\n" + tokenFor(sealFor(t, id, "discord.token")),
			[]string{"discord.token", "secrets.key"}, "cannot itself be sealed"},
		{"a configured key unread with nothing sealed", "[secrets]\nkey = { env = \"AIRUT_TEST_UNSET\" }\n" + minimal,
			[]string{"secrets.key"}, "not set"},
		{"plaintext where a sealed value belongs", keyEnv(t, id) + tokenFor(sentinel),
			[]string{"discord.token.sealed"}, "not a sealed value"},
		{"an old version", keyEnv(t, id) + tokenFor("enc:age:v1:"+sentinel),
			[]string{"discord.token.sealed"}, "a version airut does not open"},
		{"a damaged field", keyEnv(t, id) + tokenFor(seal.Prefix+"!!"+sentinel),
			[]string{"discord.token"}, "damaged"},
		{"a bare sealed string", keyEnv(t, id) + "[discord]\ntoken = \"" + sealFor(t, id, "discord.token") + "\"\n",
			[]string{"discord.token"}, `written as { sealed = "enc:age:v2:..." }`},
		{"a sealed setting", keyEnv(t, id) + minimal + "[settings]\nx = " + sealedField(setting) + "\n",
			[]string{"settings.x"}, "never holds a secret"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AIRUT_TEST_TOKEN", "t")
			_, err := load(t, c.src)
			if err == nil {
				t.Fatal("loaded")
			}
			if got := problemKeys(t, err); strings.Join(got, " ") != strings.Join(c.keys, " ") {
				t.Errorf("problems at %q, want %q: %v", got, c.keys, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
			noLeak(t, "the error", err.Error(), id, other)
		})
	}
}

func TestSealChecksKey(t *testing.T) {
	id := newIdentity(t)
	for _, key := range []string{"providers.cpa.api_key", "discord.token", "matrix.password", "matrix.recovery_key", "app.booru.token", "app.mirrors[0].key",
		`app.vault."a.b".c`, "app.by_name.0.key", "app.grid[1][2]"} {
		if err := CheckSealKey(key); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	for _, key := range []string{"", "secrets.key", "provider.cpa.api_key", "providers.cpa.base_url", "discord.tokens", "app", "app.", "app..x", "app.a b",
		"settings.x", `providers."a.b".api_key`, `providers."cpa".api_key`, "app[0].x", "discord.token[0]", `"discord".token`, `app."plain".x`} {
		if err := CheckSealKey(key); err == nil {
			t.Errorf("%q: sealable", key)
		}
	}
	if _, err := Seal("discord.token", "", id.Recipient()); err == nil {
		t.Error("sealed an empty value")
	}
	if _, err := Seal("discord.token", sentinel); err == nil {
		t.Error("sealed for no recipient")
	}
	if b := SealBinding("app.booru.token"); b != "airut|app.booru.token" {
		t.Errorf("binding = %q", b)
	}
	// What Seal writes, seal.Open reads at the same binding.
	field := sealFor(t, id, "app.booru.token")
	if v, err := seal.Open(SealBinding("app.booru.token"), field, id); err != nil || v != sentinel {
		t.Errorf("seal.Open: %v", err)
	}
	if _, err := seal.Open(SealBinding("discord.token"), field, id); !errors.Is(err, seal.ErrMismatch) {
		t.Errorf("seal.Open at another key: %v", err)
	}
}

// nested holds secrets under keys that may need quoting, and an array
// beside a map whose keys look like indexes.
type nested struct {
	Vault  map[string]map[string]Secret `toml:"vault"`
	List   []mirror                     `toml:"list"`
	ByName map[string]mirror            `toml:"by_name"`
}

// TestSealedBindingsAreUnambiguous checks that two places in the file
// never share a binding: keys with dots in them are quoted, and an
// index is written apart from a table key that looks like one.
func TestSealedBindingsAreUnambiguous(t *testing.T) {
	id := newIdentity(t)
	// ["a.b"]["c"] and ["a"]["b.c"] would both be vault.a.b.c if keys
	// were joined as they are.
	abC, aBC := `app.vault."a.b".c`, `app.vault.a."b.c"`
	ok := keyEnv(t, id) + matrixOnly + `
[app.vault."a.b"]
c = ` + sealedField(sealFor(t, id, abC)) + `
[app.vault.a]
"b.c" = ` + sealedField(sealFor(t, id, aBC)) + `
[[app.list]]
key = ` + sealedField(sealFor(t, id, "app.list[0].key")) + `
[app.by_name."0"]
key = ` + sealedField(sealFor(t, id, "app.by_name.0.key")) + `
[app.by_name."q\"\\x\n"]
key = ` + sealedField(sealFor(t, id, `app.by_name."q\"\\x\n".key`)) + "\n"
	var n nested
	if _, err := loadApp(t, ok, &n); err != nil {
		t.Fatal(err)
	}
	if n.Vault["a.b"]["c"].Value() != sentinel || n.Vault["a"]["b.c"].Value() != sentinel ||
		n.List[0].Key.Value() != sentinel || n.ByName["0"].Key.Value() != sentinel || n.ByName["q\"\\x\n"].Key.Value() != sentinel {
		t.Error("a value did not open at the key path it was sealed for")
	}

	// Swap each pair: every one is refused as moved.
	swapped := keyEnv(t, id) + matrixOnly + `
[app.vault."a.b"]
c = ` + sealedField(sealFor(t, id, aBC)) + `
[app.vault.a]
"b.c" = ` + sealedField(sealFor(t, id, abC)) + `
[[app.list]]
key = ` + sealedField(sealFor(t, id, "app.by_name.0.key")) + `
[app.by_name."0"]
key = ` + sealedField(sealFor(t, id, "app.list[0].key")) + "\n"
	var n2 nested
	_, err := loadApp(t, swapped, &n2)
	want := []string{`app.by_name.0.key`, `app.list[0].key`, `app.vault."a.b".c`, `app.vault.a."b.c"`}
	if got := problemKeys(t, err); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("problems at %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), "moved from another key") {
		t.Errorf("err = %v", err)
	}
	noLeak(t, "the error", err.Error(), id)
}

func TestKeyPaths(t *testing.T) {
	for k, want := range map[string]string{
		"token": "token", "a-b_C9": "a-b_C9", "a.b": `"a.b"`, "": `""`, "0": "0",
		"sp ace": `"sp ace"`, `q"\`: `"q\"\\"`, "\n\t\x01\x7f": `"\n\t\u0001\u007F"`, "café": `"café"`,
	} {
		if got := keyPart(k); got != want {
			t.Errorf("keyPart(%q) = %s, want %s", k, got, want)
		}
		parts, err := parseKeyPath("app." + want + "[2]")
		if err != nil || len(parts) != 3 || parts[1].key != k || !parts[2].isIndex || parts[2].index != 2 {
			t.Errorf("parseKeyPath(app.%s[2]) = %+v, %v", want, parts, err)
		}
	}
	for _, bad := range []string{"", "app.", "app..x", "app.a b", `app."a`, `app."abc"`, `app."\x"`, `app."\u00e9"`, "app.x[", "app.x[01]", "app.x[-1]", "app.x[a]", "app.x]", `app."a"b`, "app.x[0]y"} {
		if _, err := parseKeyPath(bad); err == nil {
			t.Errorf("parseKeyPath(%q) accepted", bad)
		} else if strings.Contains(err.Error(), bad) && bad != "" {
			t.Errorf("parseKeyPath(%q) quoted its input: %v", bad, err)
		}
	}
}

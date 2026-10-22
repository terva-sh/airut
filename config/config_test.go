package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// sentinel stands for a secret. No error or printed config may contain it.
const sentinel = "sk-SENTINEL-do-not-print"

// credentials sets up $CREDENTIALS_DIRECTORY with the named credentials,
// each holding sentinel and a trailing newline.
func credentials(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(sentinel+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(credentialsDirEnv, dir)
}

// load parses src as if read from a file in a fresh directory.
func load(t *testing.T, src string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "airut.toml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func problemKeys(t *testing.T, err error) []string {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *config.Error", err)
	}
	return e.Keys()
}

const minimal = `
[discord]
token = { env = "AIRUT_TEST_TOKEN" }
`

// TestExample loads docs/config.example.toml, which documents every key,
// so the example and the loader cannot drift apart.
func TestExample(t *testing.T) {
	credentials(t, "proxy-api-key", "discord-token", "matrix-password", "matrix-recovery-key")
	c, err := Load("../docs/config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	secret := NewSecret(sentinel)
	zero := 0.0
	judgeCaps := Capabilities{ContextWindow: 8192, MaxOutput: 8192, Output: "json_schema", Slots: 2, CachePrompt: true, Temperature: &zero}
	judgeCapsAt20s := judgeCaps
	judgeCapsAt20s.Timeout = 20 * time.Second
	want := &Config{
		Bot: Bot{Prefix: "!", ReplyDepth: 3, Owners: map[string][]string{
			"discord": {"123456789012345678"}, "matrix": {"@owner:example.org"},
		}},
		Providers: []Provider{
			{ID: "local", BaseURL: "http://127.0.0.1:8080/v1", Kind: KindLlamaSwap, Load: LoadWhenIdle, Models: []Model{
				{ID: "qwen3-32b", Served: "Qwen3-32B-Q4_K_M", Capabilities: Capabilities{ContextWindow: 32768, MaxOutput: 8192, Reasoning: true}},
				{ID: "qwen3-judge", Served: "Qwen3-32B-Q4_K_M", Base: "qwen3-32b", Capabilities: judgeCaps},
			}},
			{ID: "proxy", Kind: KindOpenAI, BaseURL: "https://llm.example.net/v1", APIKey: secret, Models: []Model{
				{ID: "gpt-5.5", Served: "gpt-5.5", Capabilities: Capabilities{ContextWindow: 128000, Output: "response_format"}}}},
		},
		Agent: &Agent{
			Models:  Chain{{Provider: "local", Model: "qwen3-32b", Served: "Qwen3-32B-Q4_K_M", Capabilities: Capabilities{ContextWindow: 32768, MaxOutput: 8192, Reasoning: true}}},
			Model:   ModelRef{Provider: "local", Model: "qwen3-32b"},
			Budget:  Budget{MaxTurns: 8, MaxTokens: 200000},
			Rate:    Rate{PerUser: 5, PerChannel: 20, Window: 10 * time.Minute, Owners: &GroupRate{PerUser: 30, PerChannel: 20}},
			History: History{MaxMessages: 200, MaxTokens: 60000, MaxAge: 168 * time.Hour, Compaction: "summarize", RoomMessages: 10, FollowWithin: 15 * time.Minute},
			Access: map[string]Access{
				"discord": {Users: []string{"123456789012345678"}, Roles: []string{"234567890123456789"}},
				"matrix":  {Users: []string{"@owner:example.org"}, Rooms: []string{"!members:example.org"}},
			},
			Judge: &Judge{
				Models: Chain{
					{Provider: "local", Model: "qwen3-judge", Served: "Qwen3-32B-Q4_K_M", Capabilities: judgeCapsAt20s, MaxInputTokens: 8000},
					{Provider: "proxy", Model: "gpt-5.5", Served: "gpt-5.5", MaxInputTokens: 16000,
						Capabilities: Capabilities{ContextWindow: 128000, Output: "response_format", Timeout: 40 * time.Second}},
				},
				Model: ModelRef{Provider: "local", Model: "qwen3-judge"}, Concurrency: 2, Output: "json_schema",
				Slots: 2, CachePrompt: true, Timeout: 20 * time.Second, MaxInputTokens: 8000, Rate: JudgeRate{PerChannel: 60, Window: time.Hour}},
			Memory: Memory{ModelWrites: map[string]string{"user": "ask", "channel": "auto", "bot": "off"},
				MaxUser: 20, MaxChannel: 50, MaxBot: 50},
		},
		Store:   Store{Path: "/var/lib/airut-example/sessions.db", Retention: 720 * time.Hour},
		Discord: &Discord{Token: secret, Guilds: []string{"345678901234567890"}},
		Matrix: &Matrix{Homeserver: "https://matrix.example.org", UserID: "@sample:example.org", Password: secret,
			StateDir: "/var/lib/airut-example/matrix", DeviceName: "airut sample"},
		Metrics:  Metrics{Listen: "127.0.0.1:9464", SummaryEvery: 15 * time.Minute},
		Settings: map[string]string{"roll.max_dice": "10"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("example:\n got %+v\nwant %+v", c, want)
	}
	if c.Agent.Model.String() != "local/qwen3-32b" {
		t.Errorf("ModelRef.String = %q", c.Agent.Model)
	}
	if got := c.Providers[0].RunningURL(); got != "http://127.0.0.1:8080/running" {
		t.Errorf("RunningURL = %q", got)
	}
}

func TestDefaults(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	c, err := load(t, minimal+`
[providers.local]
base_url = "http://127.0.0.1:8000/v1"
models = [{ id = "m" }]

[agent]
model = "local/m"
budget = { max_turns = 1, max_tokens = 1 }

[store]
path = "sessions.db"
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.Rate != (Rate{PerUser: DefaultPerUser, PerChannel: DefaultPerChannel, Window: DefaultWindow}) {
		t.Errorf("rate = %+v", c.Agent.Rate)
	}
	if c.Store.Retention != DefaultRetention || !filepath.IsAbs(c.Store.Path) || filepath.Base(c.Store.Path) != "sessions.db" {
		t.Errorf("store = %+v", c.Store)
	}
	if c.Agent.History != (History{}) {
		t.Errorf("history = %+v, want no bounds", c.Agent.History)
	}
	if len(c.Agent.Access) != 0 {
		t.Errorf("access = %v, want none: nobody may start a turn", c.Agent.Access)
	}
	if !c.Providers[0].APIKey.IsZero() {
		t.Error("a keyless provider has a key")
	}

	// A file with no [agent] runs plain commands only.
	c, err = load(t, minimal)
	if err != nil || c.Agent != nil || c.Store.Path != "" || c.Matrix != nil || c.Metrics != (Metrics{}) {
		t.Fatalf("minimal = %+v, %v", c, err)
	}
}

// mismatched is a format whose verbs do not suit a struct. It is a
// variable so that vet, which rightly rejects it in real code, lets the
// test make the mistake on purpose.
var mismatched = "%d %x"

// TestSecretsNeverPrint covers every route by which a config reaches a
// log: fmt verbs, JSON, text encoders, and slog.
func TestSecretsNeverPrint(t *testing.T) {
	credentials(t, "proxy-api-key", "discord-token", "matrix-password", "matrix-recovery-key")
	c, err := Load("../docs/config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("loaded", "token", c.Discord.Token, "config", c)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("loaded", "token", c.Discord.Token)
	js, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := toml.Marshal(c.Discord)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", c), "%+v": fmt.Sprintf("%+v", *c), "%#v": fmt.Sprintf("%#v", *c),
		"%s": fmt.Sprintf("%s", c.Discord.Token), "%q": fmt.Sprintf("%q", c.Discord.Token),
		"%+v of a platform": fmt.Sprintf("%+v %#v", *c.Discord, *c.Matrix),
		// fmt skips String for verbs that do not take a string, and would
		// format the struct's field instead.
		"other verbs":           fmt.Sprintf("%d %x %X %o %b %e %t %c %U %8.3s %-q %+v", c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token, c.Discord.Token),
		"other verbs, nested":   fmt.Sprintf(mismatched+" %v", *c.Discord, *c.Matrix, c.Providers),
		"other verbs, pointers": fmt.Sprintf(mismatched, c.Discord, c),
		"json":                  string(js), "toml": string(tm), "slog": logged.String(),
	} {
		if strings.Contains(out, sentinel) {
			t.Errorf("%s printed the secret: %s", name, out)
		}
	}
	if c.Discord.Token.Value() != sentinel {
		t.Error("Value does not return the secret")
	}
	if NewSecret("").String() != "" || NewSecret("x").String() != Redacted {
		t.Error("String is wrong")
	}
}

func TestSecretReferences(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("token", sentinel+"\r\n")
	write("empty", "\n")
	src := func(ref string) string { return "[discord]\ntoken = " + ref + "\n" }
	loadIn := func(s string) (*Config, error) {
		path := filepath.Join(dir, "airut.toml")
		write("airut.toml", s)
		return Load(path)
	}

	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	for _, ref := range []string{`{ env = "AIRUT_TEST_TOKEN" }`, `{ file = "token" }`, `{ file = "` + filepath.Join(dir, "token") + `" }`} {
		c, err := loadIn(src(ref))
		if err != nil || c.Discord.Token.Value() != sentinel {
			t.Errorf("%s: %v", ref, err)
		}
	}
	credentials(t, "discord-token")
	if c, err := loadIn(src(`{ credential = "discord-token" }`)); err != nil || c.Discord.Token.Value() != sentinel {
		t.Errorf("credential: %v", err)
	}

	for _, c := range []struct{ ref, want string }{
		{`"` + sentinel + `"`, "a secret is never written in the file"},
		{`42`, "not a secret reference"},
		{`{ env = "AIRUT_TEST_UNSET" }`, "the environment variable it names is not set"},
		{`{ env = "A", file = "B" }`, "exactly one of"},
		{`{}`, "exactly one of"},
		{`{ vault = "x" }`, "unknown key"},
		{`{ env = "" }`, "not a non-empty string"},
		{`{ file = "missing" }`, "does not exist"},
		{`{ file = "empty" }`, "is empty"},
		{`{ credential = "../token" }`, "not a credential name"},
		{`{ credential = "absent" }`, "does not exist"},
	} {
		_, err := loadIn(src(c.ref))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.ref, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), sentinel) {
			t.Errorf("%s: the error quotes the secret: %v", c.ref, err)
		}
	}

	// A secret pasted where a name or path belongs is not echoed.
	for _, ref := range []string{`{ env = "` + sentinel + `" }`, `{ file = "` + sentinel + `" }`, `{ credential = "` + sentinel + `" }`, `{ credential = "x/` + sentinel + `" }`} {
		if _, err := loadIn(src(ref)); err == nil || strings.Contains(err.Error(), sentinel) {
			t.Errorf("%s: err = %v", ref, err)
		}
	}

	// Exactly one line ending is trimmed: a secret may end in one.
	for content, want := range map[string]string{
		"abc\n": "abc", "abc\r\n": "abc", "abc\n\n": "abc\n", "abc\r\n\r\n": "abc\r\n", "abc\r": "abc\r", "abc": "abc",
	} {
		write("ending", content)
		if c, err := loadIn(src(`{ file = "ending" }`)); err != nil || c.Discord.Token.Value() != want {
			t.Errorf("file %q: got %q, %v; want %q", content, c.Discord.Token.Value(), err, want)
		}
	}

	t.Setenv(credentialsDirEnv, "")
	if _, err := loadIn(src(`{ credential = "discord-token" }`)); err == nil || !strings.Contains(err.Error(), "LoadCredential=") {
		t.Errorf("no credentials directory: %v", err)
	}
}

// TestProblems checks that each mistake is reported under its key, all
// at once, and that no message quotes the file.
func TestProblems(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	for _, c := range []struct {
		name, src string
		keys      []string
	}{
		{"no platform", ``, []string{"(file)"}},
		{"unknown keys", "colour = \"blue\"\n" + minimal + "[discord.extra]\nx = 1\n", []string{"colour", "discord.extra"}},
		{"a wrong type", minimal + "[bot]\nprefix = 3\n", []string{"bot.prefix"}},
		{"a deep reply chain", minimal + "[bot]\nreply_depth = 11\n", []string{"bot.reply_depth"}},
		{"bad owners", minimal + "[bot]\nowners = { matrix = [\"@ok:example.org\", \"" + sentinel + "\"], discord = [\"12\", \"@x:example.org\"], irc = [\"x\"] }\n",
			[]string{"bot.owners.discord.1", "bot.owners.irc", "bot.owners.matrix.1"}},
		{"owners not a table", minimal + "[bot]\nowners = [\"@a:example.org\"]\n", []string{"bot.owners"}},
		{"a plain recovery key", minimal + "[matrix]\nhomeserver = \"https://m.example.org\"\nuser_id = \"@b:example.org\"\nstate_dir = \"m\"\nrecovery_key = \"" + sentinel + "\"\n",
			[]string{"matrix.recovery_key"}},
		{"bad syntax", minimal + "[bot]\nprefix = \"" + sentinel + "\n", []string{"(file)"}},
		{"bad providers", minimal + `
[providers.Bad]
base_url = "ftp://` + sentinel + `@host"
models = [{ id = "a" }, { id = "a", context_window = -1 }, { max_output = -2 }]
[providers.empty]
base_url = "https://user:` + sentinel + `@example.net"
`, []string{
			"providers.Bad", "providers.Bad.base_url", "providers.Bad.models.1.context_window", "providers.Bad.models.1.id",
			"providers.Bad.models.2.id", "providers.Bad.models.2.max_output", "providers.empty.base_url", "providers.empty.models",
		}},
		{"an incomplete agent", minimal + "[agent]\nrate = { per_user = 0, window = \"soon\" }\n" +
			"history = { max_messages = -1, max_tokens = -2, max_age = \"1d\", compaction = \"trim\", room_messages = 51, follow_within = \"-1m\" }\n", []string{
			"agent.budget", "agent.history.compaction", "agent.history.follow_within", "agent.history.max_age", "agent.history.max_messages",
			"agent.history.max_tokens", "agent.history.room_messages", "agent.model", "agent.rate.per_user", "agent.rate.window", "store.path",
		}},
		{"an agent naming what is not there", minimal + `
[providers.local]
base_url = "http://localhost/v1"
models = [{ id = "m" }]
[agent]
model = "local/other"
budget = { max_turns = 0, max_tokens = 0 }
[store]
path = "s.db"
retention = "-1h"
`, []string{"agent.budget.max_tokens", "agent.budget.max_turns", "agent.model", "store.retention"}},
		{"a model without a provider", minimal + "[agent]\nmodel = \"m\"\nbudget = { max_turns = 1, max_tokens = 1 }\n[store]\npath = \"s\"\n", []string{"agent.model"}},
		{"a bad judge", minimal + `
[providers.local]
base_url = "http://localhost/v1"
models = [{ id = "m" }]
[agent]
model = "local/m"
budget = { max_turns = 1, max_tokens = 1 }
[agent.judge]
model = "elsewhere/m"
concurrency = -1
output = "xml"
slots = -2
timeout = "soon"
max_input_tokens = -1
bytes_per_token = -0.5
rate = { per_channel = 5 }
[store]
path = "s.db"
`, []string{"agent.judge.bytes_per_token", "agent.judge.concurrency", "agent.judge.max_input_tokens", "agent.judge.model", "agent.judge.output", "agent.judge.rate.window", "agent.judge.slots", "agent.judge.timeout"}},
		{"a bad memory table", minimal + `
[providers.local]
base_url = "http://localhost/v1"
models = [{ id = "m" }]
[agent]
model = "local/m"
budget = { max_turns = 1, max_tokens = 1 }
[agent.memory]
model_writes = { user = "always", room = "auto" }
max_user = -1
max_channel = -2
max_bot = -3
[store]
path = "s.db"
`, []string{"agent.memory.max_bot", "agent.memory.max_channel", "agent.memory.max_user", "agent.memory.model_writes.room", "agent.memory.model_writes.user"}},
		{"bad access lists", minimal + `
[providers.p]
base_url = "http://localhost/v1"
models = [{ id = "m" }]
[agent]
model = "p/m"
budget = { max_turns = 1, max_tokens = 1 }
access.discord = { users = ["@x:y"], roles = ["admins"] }
access.matrix = { users = ["owner"], roles = ["admins"], rooms = ["!ok:example.org", "#alias:example.org", "!nohost"] }
[store]
path = "s"
`, []string{"agent.access.discord.roles.0", "agent.access.discord.users.0", "agent.access.matrix.roles",
			"agent.access.matrix.rooms.1", "agent.access.matrix.rooms.2", "agent.access.matrix.users.0"}},
		{"secrets pasted into checked settings", `
[providers.p]
base_url = "http://localhost/v1"
models = [{ id = "` + sentinel + `" }, { id = "` + sentinel + `" }]
[agent]
model = "` + sentinel + `/m"
budget = { max_turns = 1, max_tokens = 1 }
access.discord = { users = ["` + sentinel + `"], roles = ["` + sentinel + `"] }
access.matrix = { users = ["` + sentinel + `"] }
[store]
path = "s"
[discord]
token = { env = "AIRUT_TEST_TOKEN" }
guilds = ["` + sentinel + `"]
`, []string{
			"agent.access.discord.roles.0", "agent.access.discord.users.0", "agent.access.matrix.users.0",
			"agent.model", "discord.guilds.0", "providers.p.models.1.id",
		}},
		{"a secret pasted as a model name", minimal + `
[providers.p]
base_url = "http://localhost/v1"
models = [{ id = "m" }]
[agent]
model = "p/` + sentinel + `"
budget = { max_turns = 1, max_tokens = 1 }
[store]
path = "s"
`, []string{"agent.model"}},
		{"a rate for another group", minimal + `
[providers.p]
base_url = "http://localhost/v1"
models = [{ id = "m" }]
[agent]
model = "p/m"
budget = { max_turns = 1, max_tokens = 1 }
[agent.rate.for.owners]
per_user = -1
per_channel = -2
window = "1m"
[agent.rate.for.mods]
per_user = 10
users = ["@mod:example.org"]
[store]
path = "s"
`, []string{"agent.rate.for.mods", "agent.rate.for.owners.per_channel", "agent.rate.for.owners.per_user", "agent.rate.for.owners.window"}},
		{"a rate for that is not a table", minimal + `
[providers.p]
base_url = "http://localhost/v1"
models = [{ id = "m" }]
[agent]
model = "p/m"
budget = { max_turns = 1, max_tokens = 1 }
rate = { for = 3 }
[store]
path = "s"
`, []string{"agent.rate.for"}},
		{"bad metrics", minimal + "[metrics]\nlisten = \"" + sentinel + "\"\nsummary_every = \"0s\"\n", []string{"metrics.listen", "metrics.summary_every"}},
		{"a store without an agent", minimal + "[store]\npath = \"s\"\n", []string{"store"}},
		{"bad platforms", "[discord]\nguilds = [\"general\"]\n[bot]\nprefix = \"! \"\n[matrix]\nhomeserver = \"example.org\"\n", []string{
			"bot.prefix", "discord.guilds.0", "discord.token", "matrix.homeserver", "matrix.state_dir", "matrix.user_id",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, c.src)
			if got := problemKeys(t, err); !slices.Equal(got, c.keys) {
				t.Errorf("keys = %q, want %q\n%v", got, c.keys, err)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("the error quotes the file: %v", err)
			}
		})
	}
}

func TestErrorText(t *testing.T) {
	// Problems found after decoding are reported together.
	_, err := load(t, "[bot]\nprefix = \"! \"\n[matrix]\n")
	var e *Error
	if !errors.As(err, &e) {
		t.Fatal(err)
	}
	msg := err.Error()
	for _, want := range []string{"airut.toml has 4 problems:", "\n  bot.prefix: a prefix cannot contain spaces", "\n  matrix.homeserver: required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error text lacks %q:\n%s", want, msg)
		}
	}

	// A decoding error stops the parse, so it is reported alone, at its
	// position.
	_, err = load(t, "[bot]\nprefix = 3\n")
	msg = err.Error()
	for _, want := range []string{"airut.toml has 1 problem:", "\n  bot.prefix: the value is not a string (line 2, column"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error text lacks %q:\n%s", want, msg)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err == nil || errors.As(err, &e) {
		t.Errorf("a missing file = %v, want a plain error", err)
	}
}

// TestSyntaxErrorInMultilineString is a secret pasted into the file as a
// multiline string, with a line inside it shaped like a key and an
// invalid escape. The error is reported by position, never by text read
// from around it.
func TestSyntaxErrorInMultilineString(t *testing.T) {
	_, err := load(t, minimal+"[bot]\nprefix = \"\"\"\n"+sentinel+"_x = \\q\n\"\"\"\n")
	if got := problemKeys(t, err); !slices.Equal(got, []string{"(file)"}) || !strings.Contains(err.Error(), "not valid TOML at line ") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the error quotes the secret: %v", err)
	}
}

func TestOwners(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	c, err := load(t, minimal+"[bot]\nowners = { matrix = [\"@you:example.org\"], discord = [\"123456789012345678\", \"234567890123456789\"] }\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"matrix": {"@you:example.org"}, "discord": {"123456789012345678", "234567890123456789"}}
	if !reflect.DeepEqual(c.Bot.Owners, want) {
		t.Errorf("owners = %q, want %q", c.Bot.Owners, want)
	}
	// Owners on a platform the file has no table for are kept, and a
	// file without owners has none.
	c, err = load(t, minimal+"[bot]\nowners = { matrix = [\"@you:example.org\"] }\n")
	if err != nil || !reflect.DeepEqual(c.Bot.Owners, map[string][]string{"matrix": {"@you:example.org"}}) {
		t.Errorf("matrix owners on a Discord bot = %q, %v", c.Bot.Owners, err)
	}
	if c, err := load(t, minimal); err != nil || c.Bot.Owners != nil {
		t.Errorf("no owners = %q, %v", c.Bot.Owners, err)
	}
}

// TestOwnersRate: [agent.rate.for.owners] takes the owners' caps, 0 for
// none, and a field it leaves out inherits [agent] rate's.
func TestOwnersRate(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	const agent = `
[providers.local]
base_url = "http://127.0.0.1:8000/v1"
models = [{ id = "m" }]
[store]
path = "sessions.db"
[agent]
model = "local/m"
budget = { max_turns = 1, max_tokens = 1 }
`
	for _, c := range []struct {
		name, src string
		want      *GroupRate
	}{
		{"none", "", nil},
		{"both set", "[agent.rate]\nper_user = 3\n[agent.rate.for.owners]\nper_user = 30\nper_channel = 0\n", &GroupRate{PerUser: 30, PerChannel: 0}},
		{"per_channel inherited", "[agent.rate]\nper_channel = 7\n[agent.rate.for.owners]\nper_user = 0\n", &GroupRate{PerUser: 0, PerChannel: 7}},
		{"both inherited from the defaults", "[agent.rate.for.owners]\n", &GroupRate{PerUser: DefaultPerUser, PerChannel: DefaultPerChannel}},
		{"written inline", "[agent.rate]\nfor = { owners = { per_user = 12 } }\n", &GroupRate{PerUser: 12, PerChannel: DefaultPerChannel}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := load(t, minimal+agent+c.src)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Agent.Rate.Owners; !reflect.DeepEqual(got, c.want) {
				t.Errorf("owners' rate = %+v, want %+v", got, c.want)
			}
			// The owners' rate never changes the members'.
			if r := cfg.Agent.Rate; r.Window != DefaultWindow {
				t.Errorf("window = %v, want the members' %v", r.Window, DefaultWindow)
			}
		})
	}
}

func TestOwnersRateOtherGroup(t *testing.T) {
	t.Setenv("AIRUT_TEST_TOKEN", sentinel)
	_, err := load(t, minimal+`
[providers.local]
base_url = "http://127.0.0.1:8000/v1"
models = [{ id = "m" }]
[store]
path = "sessions.db"
[agent]
model = "local/m"
budget = { max_turns = 1, max_tokens = 1 }
[agent.rate.for.mods]
per_user = 10
`)
	if err == nil || !strings.Contains(err.Error(), "agent.rate.for.mods") || !strings.Contains(err.Error(), "only owners has its own rate") {
		t.Errorf("another group under for: %v", err)
	}
}

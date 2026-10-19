package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

func load(t *testing.T, src string) *config.Config {
	t.Helper()
	t.Setenv("AIRUT_TEST_TOKEN", "token")
	path := filepath.Join(t.TempDir(), "airut.toml")
	if err := os.WriteFile(path, []byte(src+"\n[discord]\ntoken = { env = \"AIRUT_TEST_TOKEN\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGate(t *testing.T) {
	c := load(t, `
[providers.local]
base_url = "https://models.example.com/v1"
models = [{ id = "m" }]

[agent]
model = "local/m"
budget = { max_turns = 8, max_tokens = 200000 }
rate = { per_user = 2, window = "1m" }

[agent.access.discord]
users = ["1"]
roles = ["2"]

[agent.access.matrix]
users = ["@owner:example.org"]
rooms = ["!members:example.org"]

[store]
path = "/var/lib/example/sessions.db"
`)
	want := airut.Gate{
		Access: map[airut.Platform]airut.Access{
			airut.Discord: {Users: []string{"1"}, Roles: []string{"2"}},
			airut.Matrix:  {Users: []string{"@owner:example.org"}, Channels: []string{"!members:example.org"}},
		},
		PerUser:    2,
		PerChannel: config.DefaultPerChannel,
		Window:     time.Minute,
		Budget:     airut.Budget{Turns: 8, Tokens: 200000},
	}
	if got := Gate(c); !reflect.DeepEqual(got, want) {
		t.Fatalf("Gate =\n %+v\nwant\n %+v", got, want)
	}
	// The gate it builds is one the core accepts.
	airut.New(airut.Config{Gate: Gate(c)})
}

func TestGateWithoutAgent(t *testing.T) {
	if g := Gate(load(t, "")); !reflect.DeepEqual(g, airut.Gate{}) {
		t.Fatalf("a file with no [agent] gave %+v, want the zero Gate, which refuses every turn", g)
	}
}

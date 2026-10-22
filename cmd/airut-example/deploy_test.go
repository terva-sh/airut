package main

import (
	"bufio"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// unitLines returns the reference unit's lines that set key, and those
// a comment holds, without the key.
func unitLines(t *testing.T, key string) (set, commented []string) {
	t.Helper()
	f, err := os.Open("../../deploy/systemd/airut-example.service")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			set = append(set, v)
		} else if v, ok := strings.CutPrefix(line, "#"+key+"="); ok {
			commented = append(commented, v)
		}
	}
	return set, commented
}

// The reference unit loads, by id, exactly the credentials the example
// configuration names. The recovery key is commented out in both, so it
// stays off the host, and so are the optional fleet secrets, so a
// configuration copied from the example never names a credential the
// unit does not load.
func TestUnitCredentialsMatchTheExample(t *testing.T) {
	src, err := os.ReadFile("../../docs/config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	refs := func(re string) []string {
		var out []string
		for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(string(src), -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return out
	}
	named := refs(`(?m)^[^#\n]*\{ credential = "([^"]+)" \}`)
	namedOff := refs(`(?m)^#\s*[a-z_]+ = \{ credential = "([^"]+)" \}`)
	ids := func(lines []string) []string {
		var out []string
		for _, l := range lines {
			id, file, ok := strings.Cut(l, ":")
			if !ok || file != "/etc/credstore.encrypted/airut-example-"+id {
				t.Errorf("LoadCredentialEncrypted=%s does not load /etc/credstore.encrypted/airut-example-ID as ID", l)
			}
			out = append(out, id)
		}
		slices.Sort(out)
		return out
	}
	set, commented := unitLines(t, "LoadCredentialEncrypted")
	if loaded := ids(set); !slices.Equal(loaded, named) {
		t.Errorf("the unit loads %v, but the example configuration names %v", loaded, named)
	}
	want := []string{"matrix-fleet-secret", "matrix-fleet-secret-previous", "matrix-recovery-key"}
	if off := ids(commented); !slices.Equal(off, want) || !slices.Equal(namedOff, off) {
		t.Errorf("commented out: the unit's %v and the example's %v, want %v in both", off, namedOff, want)
	}
}

// The unit starts the bot on the default configuration path, and the
// example configuration keeps its state in the unit's StateDirectory.
func TestUnitPaths(t *testing.T) {
	exec, _ := unitLines(t, "ExecStart")
	if len(exec) != 1 || exec[0] != "/usr/local/lib/airut-example/airut-example -config /etc/airut-example/airut.toml" {
		t.Errorf("ExecStart = %q", exec)
	}
	state, _ := unitLines(t, "StateDirectory")
	if !slices.Equal(state, []string{"airut-example"}) {
		t.Fatalf("StateDirectory = %q", state)
	}
	src, err := os.ReadFile("../../docs/config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"path", "state_dir"} {
		m := regexp.MustCompile(`(?m)^` + key + ` = "([^"]+)"`).FindStringSubmatch(string(src))
		if m == nil || !strings.HasPrefix(m[1], "/var/lib/airut-example/") {
			t.Errorf("the example's %s = %v, want a path under the unit's /var/lib/airut-example", key, m)
		}
	}
}

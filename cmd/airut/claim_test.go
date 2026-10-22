package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// airut claim and airut unclaim reach the claim package; its own tests
// cover the cases.
func TestClaimAndUnclaim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot.toml")
	src := "[discord]\ntoken = { env = \"NO_SUCH_TOKEN\" }\n[agent]\nmodel = \"local/m\"\n[store]\npath = \"sessions.db\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		out  string
	}{
		{[]string{"claim", "-config", path, "-actor", "ops", "123"}, "claimed discord 123"},
		{[]string{"unclaim", "-config", path, "discord", "123"}, "unclaimed discord 123"},
	} {
		var out, errOut bytes.Buffer
		if code := run(c.args, strings.NewReader(""), &out, &errOut); code != 0 || !strings.Contains(out.String(), c.out) {
			t.Errorf("airut %s = %d, %q %q; want 0 and %q", c.args[0], code, out.String(), errOut.String(), c.out)
		}
	}
	var out, errOut bytes.Buffer
	if code := run(nil, strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "airut claim -config FILE") {
		t.Errorf("airut with no command = %d, %q", code, errOut.String())
	}
}

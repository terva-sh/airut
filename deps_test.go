package airut

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const modulePath = "terva.sh/airut"

// A family is a set of import paths that only some airut packages may
// reach, directly or through any dependency.
type family struct {
	name  string
	roots []string
}

var families = []family{
	{"terva", []string{"terva.sh/terva"}},
	{"discord", []string{"github.com/bwmarrin/discordgo"}},
	{"matrix", []string{"terva.sh/rihma", "maunium.net/go/mautrix"}},
}

// within reports whether path is root or a package below it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// allowedFamilies is the layering rule. agent/ may reach terva, and each
// adapter its own platform. Programs under cmd/ wire the layers together
// and may reach all of them. Every other package, the core included,
// may reach none, so a bot with only plain commands builds neither
// terva nor a platform it does not use.
func allowedFamilies(pkg string) []string {
	switch {
	case within(pkg, modulePath+"/cmd"):
		return []string{"terva", "discord", "matrix"}
	case within(pkg, modulePath+"/agent"):
		return []string{"terva"}
	case within(pkg, modulePath+"/discord"):
		return []string{"discord"}
	case within(pkg, modulePath+"/matrix"):
		return []string{"matrix"}
	}
	return nil
}

// layerViolations returns one message for each of deps that pkg may not
// reach.
func layerViolations(pkg string, deps []string) []string {
	allowed := allowedFamilies(pkg)
	var out []string
	for _, dep := range deps {
		for _, f := range families {
			if slices.Contains(allowed, f.name) {
				continue
			}
			if slices.ContainsFunc(f.roots, func(root string) bool { return within(dep, root) }) {
				out = append(out, pkg+" depends on "+dep+", a "+f.name+" package")
			}
		}
	}
	return out
}

func TestLayerViolations(t *testing.T) {
	cases := []struct {
		pkg  string
		deps []string
		want int
	}{
		{modulePath, []string{"context", "strings"}, 0},
		{modulePath, []string{"terva.sh/terva/packages/core"}, 1},
		{modulePath, []string{"terva.sh/terva"}, 1},
		{modulePath, []string{"github.com/bwmarrin/discordgo"}, 1},
		{modulePath, []string{"terva.sh/rihma", "maunium.net/go/mautrix/event"}, 2},
		{modulePath + "/airuttest", []string{"terva.sh/terva/packages/provider"}, 1},
		{modulePath + "/config", []string{"terva.sh/terva/packages/provider"}, 1},
		{modulePath + "/agent", []string{"terva.sh/terva/packages/core", "terva.sh/terva/packages/provider"}, 0},
		{modulePath + "/agent", []string{"github.com/bwmarrin/discordgo"}, 1},
		{modulePath + "/discord", []string{"github.com/bwmarrin/discordgo"}, 0},
		{modulePath + "/discord", []string{"maunium.net/go/mautrix"}, 1},
		{modulePath + "/matrix", []string{"terva.sh/rihma", "maunium.net/go/mautrix/id"}, 0},
		{modulePath + "/matrix", []string{"terva.sh/terva/packages/core"}, 1},
		{modulePath + "/cmd/airut-example", []string{"terva.sh/terva", "github.com/bwmarrin/discordgo", "terva.sh/rihma"}, 0},
		// A shared prefix is not a subpackage.
		{modulePath, []string{"terva.sh/tervax", "terva.sh/rihmalike"}, 0},
		{modulePath + "/agentish", []string{"terva.sh/terva"}, 1},
	}
	for _, c := range cases {
		if got := layerViolations(c.pkg, c.deps); len(got) != c.want {
			t.Errorf("layerViolations(%s, %v) = %q, want %d violations", c.pkg, c.deps, got, c.want)
		}
	}
}

// TestImportLayering applies the layering rule to every package in the
// module, as the cgo-free goolm build sees it. A package added later
// falls under the rule without a change here.
func TestImportLayering(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go command")
	}
	readPackageDirs(t)
	cmd := exec.Command("go", "list", "-tags", "goolm", "-f", `{{.ImportPath}} {{join .Deps " "}}`, "./...")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var sawRoot bool
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sawRoot = sawRoot || fields[0] == modulePath
		for _, v := range layerViolations(fields[0], fields[1:]) {
			t.Error(v)
		}
	}
	if !sawRoot {
		t.Fatalf("go list did not report %s", modulePath)
	}
}

// readPackageDirs lists every directory go list reads. go test caches a
// result by the test binary, which here holds only the root package's
// dependencies, so a new import in another package would otherwise
// replay a stale pass. It also records each directory the test opens,
// keyed on its entries' names, sizes, and modification times, so
// listing them ties the cached result to every file in the module.
func readPackageDirs(t *testing.T) {
	t.Helper()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() && path != "." && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the module tree: %v", err)
	}
}

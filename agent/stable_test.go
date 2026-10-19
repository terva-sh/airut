package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const tervaModule = "terva.sh/terva"

// TestImportsOnlyStableTerva fails when a package under agent/, or its
// tests, imports a terva package that terva's .api/packages.txt does not
// mark stable. A stable package can still hold a symbol whose doc opens
// with "Unstable:"; review catches those, and this catches the rest.
func TestImportsOnlyStableTerva(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go command")
	}
	goList := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("go", append([]string{"list"}, args...)...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %v: %v", args, err)
		}
		return string(out)
	}

	dir := strings.TrimSpace(goList("-m", "-f", "{{.Dir}}", tervaModule))
	list, err := os.ReadFile(filepath.Join(dir, ".api", "packages.txt"))
	if err != nil {
		t.Fatal(err)
	}
	stable := map[string]bool{}
	for line := range strings.Lines(string(list)) {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "stable" {
			stable[tervaModule+"/"+f[1]] = true
		}
	}
	if !stable[tervaModule+"/packages/core"] {
		t.Fatalf("read no stable packages from %s/.api/packages.txt", dir)
	}

	out := goList("-tags", "goolm", "-f", `{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`, "./...")
	var checked int
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		for _, imp := range f[1:] {
			if imp != tervaModule && !strings.HasPrefix(imp, tervaModule+"/") {
				continue
			}
			checked++
			if !stable[imp] {
				t.Errorf("%s imports %s, which terva does not mark stable", f[0], imp)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no terva import was checked; the listing is not seeing the package")
	}
}

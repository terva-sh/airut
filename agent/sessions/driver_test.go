package sessions

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sqliteDrivers are the import paths that register a SQLite driver.
var sqliteDrivers = []string{
	"modernc.org/sqlite",
	"github.com/ncruces/go-sqlite3",
	"github.com/mattn/go-sqlite3",
}

// TestOnlyDriverGoImportsADriver walks the whole module, so a driver
// imported anywhere but driver.go fails here.
func TestOnlyDriverGoImportsADriver(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(root, "agent", "sessions", "driver.go")
	var sawAllowed bool
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, drv := range sqliteDrivers {
				if p != drv && !strings.HasPrefix(p, drv+"/") {
					continue
				}
				if path == allowed {
					sawAllowed = true
				} else {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %s; only agent/sessions/driver.go may import a SQLite driver", rel, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAllowed {
		t.Fatal("driver.go imports no SQLite driver; the walk is not seeing the module")
	}
}

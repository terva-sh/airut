package sessions

import (
	"database/sql"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// This file is the only one in airut that may import a SQLite driver;
// TestOnlyDriverGoImportsADriver enforces it. Everything else goes
// through database/sql, so the driver can be swapped by editing this file.

// openPool opens the database at path. The pragmas are ketju's:
// busy_timeout first, so the others wait out a lock rather than fail;
// WAL; foreign keys, which the log's cascade relies on; FULL sync,
// because in WAL mode NORMAL can lose the latest commits on power loss;
// and immediate transactions, so a write never upgrades into a deadlock.
func openPool(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(wal)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(full)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: path, OmitHost: true, RawQuery: q.Encode()}
	return sql.Open("sqlite", u.String())
}

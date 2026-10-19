package sessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"

	"terva.sh/airut/agent/sessions/dbq"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrSchemaNewer means the database has migrations this build does not
// know: it was opened by a newer airut, and an older one must not write
// to it.
var ErrSchemaNewer = errors.New("sessions: the database was migrated by a newer airut")

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int64
	name     string
	sql      string
	checksum string
}

// loadMigrations reads the embedded migrations, numbered 1 to n with no
// gaps.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	files, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, f := range files {
		m := migrationName.FindStringSubmatch(f.Name())
		if m == nil {
			return nil, fmt.Errorf("sessions: migration %s is not named NNNN_name.sql", f.Name())
		}
		v, _ := strconv.ParseInt(m[1], 10, 64)
		b, err := fs.ReadFile(fsys, "migrations/"+f.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, migration{version: v, name: m[2], sql: string(b), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != int64(i+1) {
			return nil, fmt.Errorf("sessions: migrations are not numbered 1 to %d without gaps", len(out))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("sessions: no migrations")
	}
	return out, nil
}

// migrate applies the migrations db lacks, in one transaction. A
// migration already applied must match the embedded one by name and
// checksum, because editing an applied migration changes nothing in a
// database that already ran it.
func migrate(ctx context.Context, db *sql.DB, now time.Time) error {
	want, err := loadMigrations(migrations)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var applied []dbq.ListMigrationsRow
	var exists int
	// sqlc cannot see sqlite_schema, so this probe is written by hand.
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&exists); err != nil {
		return err
	}
	q := dbq.New(tx)
	if exists == 1 {
		if applied, err = q.ListMigrations(ctx); err != nil {
			return err
		}
	}
	if len(applied) > len(want) {
		return ErrSchemaNewer
	}
	for i, a := range applied {
		w := want[i]
		if a.Version != w.version || a.Name != w.name || a.Checksum != w.checksum {
			return fmt.Errorf("sessions: applied migration %d (%s) differs from the one in this build", a.Version, a.Name)
		}
	}
	for _, m := range want[len(applied):] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("sessions: migration %04d_%s: %w", m.version, m.name, err)
		}
		if err := q.InsertMigration(ctx, dbq.InsertMigrationParams{
			Version: m.version, Name: m.name, Checksum: m.checksum, AppliedAt: now.UnixMilli(),
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

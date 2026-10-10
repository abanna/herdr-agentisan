package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
)

// embedded holds the schema migrations, numbered 0001 upward. A migration,
// once released, is never edited: a change to the schema is a new file.
//
//go:embed migrations/*.sql
var embedded embed.FS

// migrationName is the name every migration file has: NNNN_name.sql.
var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// migration is one numbered schema step.
type migration struct {
	version int
	name    string
	sql     string
}

// SchemaVersion is the newest schema this build knows: the number of its
// embedded migrations.
func SchemaVersion() int {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return 0
	}
	set, err := loadMigrations(sub)
	if err != nil {
		return 0
	}
	return len(set)
}

// loadMigrations reads the .sql files at the root of fsys. They must be
// numbered 0001 upward with no gap or repeat, so that the version a database
// records names exactly the migrations it has had. Other files are ignored.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("%w: list migrations: %w", ErrMigration, err)
	}
	var set []migration
	for _, e := range entries { // sorted by name
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("%w: %s is not named NNNN_name.sql", ErrMigration, e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrMigration, e.Name(), err)
		}
		if next := len(set) + 1; v != next {
			return nil, fmt.Errorf("%w: %s: want version %d next; migrations are numbered from 0001 with no gap or repeat",
				ErrMigration, e.Name(), next)
		}
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrMigration, e.Name(), err)
		}
		if blank(string(body)) {
			return nil, fmt.Errorf("%w: %s has no statements", ErrMigration, e.Name())
		}
		set = append(set, migration{version: v, name: e.Name(), sql: string(body)})
	}
	return set, nil
}

// blank reports whether sql holds nothing but whitespace and line comments.
func blank(sql string) bool {
	for line := range strings.Lines(sql) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return false
		}
	}
	return true
}

// migrate applies the migrations db has not had, in order, and returns the
// version it is then at. Each runs in its own transaction with the
// user_version bump, so a failure leaves the database at the last migration
// that succeeded.
//
// The version is read inside that transaction, never before it: the store's
// transactions are BEGIN IMMEDIATE, so the read happens under the write lock,
// and an opener racing this one on the same file sees every migration the
// other committed and does not run it again.
func migrate(ctx context.Context, db *sql.DB, set []migration) (int, error) {
	for {
		applied, err := migrateNext(ctx, db, set)
		if err != nil {
			return 0, err
		}
		if !applied {
			return len(set), nil
		}
	}
}

// migrateNext applies the one migration db needs next, if any, and reports
// whether it applied one. Everything inside goes through tx: the store has a
// single connection, and tx holds it.
func migrateNext(ctx context.Context, db *sql.DB, set []migration) (applied bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("%w: begin: %w", ErrMigration, err)
	}
	defer func() {
		if !applied {
			_ = tx.Rollback()
		}
	}()
	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return false, fmt.Errorf("%w: read user_version: %w", ErrMigration, err)
	}
	switch {
	case current < 0:
		return false, fmt.Errorf("%w: the database records schema version %d; versions count up from 0",
			ErrMigration, current)
	case current > len(set):
		return false, fmt.Errorf("%w: the database is at schema version %d; this build knows up to %d",
			ErrSchemaTooNew, current, len(set))
	case current == len(set):
		return false, nil
	}
	m := set[current]
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return false, fmt.Errorf("%w: %s: %w", ErrMigration, m.name, err)
	}
	// PRAGMA takes no bound parameters.
	// #nosec G201 -- the version is an integer parsed from the migration's file name.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return false, fmt.Errorf("%w: %s: set user_version: %w", ErrMigration, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%w: %s: commit: %w", ErrMigration, m.name, err)
	}
	return true, nil
}

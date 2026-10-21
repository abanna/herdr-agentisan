package store

import (
	"context"
	"database/sql"
	"io/fs"
	"time"
)

// DB returns the store's connection, so a test can check the schema's own
// constraints on rows the store's methods do not write yet.
func DB(s *Store) *sql.DB { return s.db }

// SetCloseWait sets how long Close waits for the store's connection.
func SetCloseWait(s *Store, d time.Duration) { s.closeWait = d }

// OpenWith opens path migrated by the numbered SQL files at the root of
// migrations instead of the embedded ones.
func OpenWith(ctx context.Context, path string, migrations fs.FS) (*Store, error) {
	return open(ctx, path, migrations)
}

// VerifyPragmas checks the pragmas Open relies on against db's connection.
func VerifyPragmas(ctx context.Context, db *sql.DB) error { return verifyPragmas(ctx, db) }

// InUTC is r with its times in UTC, for comparing with a test's literals.
func (r Row) InUTC() Row {
	r.CreatedAt = r.CreatedAt.UTC()
	if !r.RetiredAt.IsZero() {
		r.RetiredAt = r.RetiredAt.UTC()
	}
	return r
}

package store

import (
	"context"
	"database/sql"
	"io/fs"
)

// DB returns the store's connection, so a test can check the schema's own
// constraints on rows the store's methods do not write yet.
func DB(s *Store) *sql.DB { return s.db }

// OpenWith opens path migrated by the numbered SQL files at the root of
// migrations instead of the embedded ones.
func OpenWith(ctx context.Context, path string, migrations fs.FS) (*Store, error) {
	return open(ctx, path, migrations)
}

// VerifyPragmas checks the pragmas Open relies on against db's connection.
func VerifyPragmas(ctx context.Context, db *sql.DB) error { return verifyPragmas(ctx, db) }

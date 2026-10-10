// Package store is the daemon's runtime state: one SQLite database, state.db,
// in the daemon's per-server directory (ADR-001 D13, amendment A1).
//
// The daemon holds the only write connection: Open returns it, and nothing
// else in the plugin opens the file for writing. The CLI reaches the state
// through the daemon's socket instead. The one planned exception is a
// read-only `doctor` command; people and the debugger agent may also open the
// file read-only with sqlite3.
//
// Every time is stored as INTEGER nanoseconds since the Unix epoch, UTC:
// integers order and compare exactly, which RFC 3339 text with trimmed
// fractional seconds does not.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"modernc.org/sqlite" // the pure-Go "sqlite" driver: no cgo (A1)
	sqlite3 "modernc.org/sqlite/lib"
)

// FocusCap is how many focus rows the history keeps. The schema's focus_cap
// trigger enforces it, for every writer.
const FocusCap = 32

// HandoffRetention is how long a finished handoff is kept after its last
// change (A1). Elastic keeps the long-term history.
const HandoffRetention = 14 * 24 * time.Hour

// BusyTimeout is how long a statement waits for a lock another connection
// holds, such as a person reading the file with sqlite3, before it fails.
const BusyTimeout = 5 * time.Second

// walRetry is how long Open waits before retrying a switch to WAL that
// another connection's lock refused.
const walRetry = 10 * time.Millisecond

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrOpen means the database file could not be created or opened.
	ErrOpen = errors.New("cannot open the state database")
	// ErrPragma means a pragma Open sets did not take effect on the
	// connection, so the database would be less durable than A1 requires.
	ErrPragma = errors.New("a state database pragma did not take effect")
	// ErrMigration means the migrations are malformed or one failed. A
	// failed migration is rolled back; the ones before it stay applied.
	ErrMigration = errors.New("state database migration failed")
	// ErrSchemaTooNew means a newer build has migrated the database past
	// every schema this build knows. The daemon refuses to start (A1).
	ErrSchemaTooNew = errors.New("state database schema is newer than this build knows")
	// ErrInvalid means a caller passed a value the store cannot record.
	ErrInvalid = errors.New("invalid state database argument")
	// ErrStore means a query or statement failed, or the store is closed.
	ErrStore = errors.New("state database operation failed")
)

// Store is the daemon's write connection to state.db.
type Store struct {
	db      *sql.DB
	version int
}

// Pragmas are the connection settings Open sets and verifies.
type Pragmas struct {
	// JournalMode is "wal": readers never block the writer.
	JournalMode string
	// Synchronous is 2 (FULL): a committed transaction survives power loss.
	Synchronous int
	// ForeignKeys enforces the schema's REFERENCES clauses.
	ForeignKeys bool
	// BusyTimeout is how long a statement waits for another connection's lock.
	BusyTimeout time.Duration
}

// want is what verifyPragmas requires.
var want = Pragmas{JournalMode: "wal", Synchronous: 2, ForeignKeys: true, BusyTimeout: BusyTimeout}

// Focus is one entry in the recent-focus history.
type Focus struct {
	// Seq orders the history: a larger Seq was focused later.
	Seq    int64
	PaneID string
	At     time.Time
}

// Open opens the state database at path, creating it owner-only if it does
// not exist, and migrates it to the newest schema this build knows. It
// returns the daemon's single write connection (A1). A database a newer
// build has migrated is refused with ErrSchemaTooNew before anything writes
// to it, so it is left exactly as it was. Openers racing on one
// file, even a fresh one, all succeed: each migration runs once, under the
// write lock. A lock another connection holds is waited for up to
// BusyTimeout, then is ErrOpen.
func Open(ctx context.Context, path string) (*Store, error) {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMigration, err)
	}
	return open(ctx, path, sub)
}

func open(ctx context.Context, path string, migrations fs.FS) (*Store, error) {
	set, err := loadMigrations(migrations)
	if err != nil {
		return nil, err
	}
	// Create the file owner-only before SQLite does: SQLite would create it
	// with the umask's default mode, and it gives the -wal and -shm files the
	// mode of the database file.
	// #nosec G304 -- path is the daemon's own state.db under the plugin state dir herdr names.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOpen, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOpen, err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrOpen, path, err)
	}
	// One connection, kept for the store's life: it is the single writer, and
	// every read shares it.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	s := &Store{db: db}
	if err := s.init(ctx, path, set); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context, path string, set []migration) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrOpen, path, err)
	}
	// A database a newer build migrated is refused before anything writes
	// to it: the switch to WAL below rewrites the file's header.
	if err := checkVersion(ctx, s.db, len(set)); err != nil {
		return err
	}
	if err := enableWAL(ctx, s.db); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrOpen, path, err)
	}
	if err := verifyPragmas(ctx, s.db); err != nil {
		return err
	}
	v, err := migrate(ctx, s.db, set)
	if err != nil {
		return err
	}
	s.version = v
	return nil
}

// dsn is the driver's name for path with the per-connection settings A1
// needs. They ride on the DSN rather than one-off statements so that they hold
// on any connection the pool opens. journal_mode is not among them: WAL is
// recorded in the file itself, and enableWAL switches to it with a retry the
// DSN cannot give. The path is a file: URI, escaped, so a '?' or '#' in the
// state directory is part of the path and not the start of the driver's
// parameters.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(FULL)")
	// BEGIN IMMEDIATE: a transaction takes the write lock when it starts,
	// never by upgrading a read lock part way through.
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q.Encode()}
	return u.String()
}

// enableWAL switches the database to WAL, which persists in the file. The
// switch needs an exclusive lock, and SQLite refuses it with SQLITE_BUSY at
// once, without waiting out the busy timeout, when waiting could deadlock: a
// connection holding a shared lock may not wait for a writer that needs it
// released, which is what two openers switching one fresh file at the same
// moment do, and what a writer holding BEGIN IMMEDIATE causes. Such a refusal
// is retried until BusyTimeout has passed; a lock that blocks even reading is
// waited out by the busy timeout itself. A cancelled ctx ends the retries:
// database/sql fails the next attempt at once with the context's error.
// verifyPragmas checks the mode that results.
func enableWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(BusyTimeout)
	for {
		var mode string
		err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil {
			return nil
		}
		if !busy(err) || !time.Now().Before(deadline) {
			return fmt.Errorf("switch to WAL: %w", err)
		}
		time.Sleep(walRetry)
	}
}

// busy reports whether err is SQLite's SQLITE_BUSY, in any of its extended
// forms.
func busy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_BUSY
}

// readPragmas reads the live settings of db's connection.
func readPragmas(ctx context.Context, db *sql.DB) (Pragmas, error) {
	var p Pragmas
	var fk, busy int64
	for _, r := range []struct {
		query string
		dst   any
	}{
		{"PRAGMA journal_mode", &p.JournalMode},
		{"PRAGMA synchronous", &p.Synchronous},
		{"PRAGMA foreign_keys", &fk},
		{"PRAGMA busy_timeout", &busy},
	} {
		if err := db.QueryRowContext(ctx, r.query).Scan(r.dst); err != nil {
			return Pragmas{}, fmt.Errorf("%w: %s: %w", ErrStore, r.query, err)
		}
	}
	p.JournalMode = strings.ToLower(p.JournalMode)
	p.ForeignKeys = fk == 1
	p.BusyTimeout = time.Duration(busy) * time.Millisecond
	return p, nil
}

// verifyPragmas checks that every setting A1 relies on took effect: a DSN
// parameter the driver ignored, or a journal mode the filesystem refused,
// must not leave a quietly less durable database.
func verifyPragmas(ctx context.Context, db *sql.DB) error {
	got, err := readPragmas(ctx, db)
	if err != nil {
		return err
	}
	var bad []string
	if got.JournalMode != want.JournalMode {
		bad = append(bad, fmt.Sprintf("journal_mode is %q, want %q", got.JournalMode, want.JournalMode))
	}
	if got.Synchronous != want.Synchronous {
		bad = append(bad, fmt.Sprintf("synchronous is %d, want %d (FULL)", got.Synchronous, want.Synchronous))
	}
	if got.ForeignKeys != want.ForeignKeys {
		bad = append(bad, "foreign_keys is off, want on")
	}
	if got.BusyTimeout != want.BusyTimeout {
		bad = append(bad, fmt.Sprintf("busy_timeout is %v, want %v", got.BusyTimeout, want.BusyTimeout))
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrPragma, strings.Join(bad, "; "))
	}
	return nil
}

// Pragmas reads the store connection's live settings.
func (s *Store) Pragmas(ctx context.Context) (Pragmas, error) { return readPragmas(ctx, s.db) }

// UserVersion is the schema version the database was migrated to when the
// store opened it (PRAGMA user_version).
func (s *Store) UserVersion() int { return s.version }

// Close closes the connection. The store is unusable afterwards.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("%w: close: %w", ErrStore, err)
	}
	return nil
}

// recordFocus appends a focus unless the pane is already the most recently
// focused one: a repeat is no new place for Back to return to.
const recordFocus = `INSERT INTO focus (pane_id, focused_at)
SELECT ?1, ?2
WHERE ?1 IS NOT (SELECT pane_id FROM focus ORDER BY seq DESC LIMIT 1)`

// RecordFocus records that paneID was focused at at. A focus on the pane
// already most recently focused is not recorded again. The schema keeps the
// newest FocusCap rows.
func (s *Store) RecordFocus(ctx context.Context, paneID string, at time.Time) error {
	if paneID == "" {
		return fmt.Errorf("%w: empty pane id", ErrInvalid)
	}
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, recordFocus, paneID, ns); err != nil {
		return fmt.Errorf("%w: record focus: %w", ErrStore, err)
	}
	return nil
}

// RecentFocus returns up to n of the most recently focused panes, newest
// first. An n of zero or less returns none.
func (s *Store) RecentFocus(ctx context.Context, n int) ([]Focus, error) {
	out := []Focus{}
	if n <= 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT seq, pane_id, focused_at FROM focus ORDER BY seq DESC LIMIT ?", n)
	if err != nil {
		return nil, fmt.Errorf("%w: read focus: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	for rows.Next() {
		var f Focus
		var ns int64
		if err := rows.Scan(&f.Seq, &f.PaneID, &ns); err != nil {
			return nil, fmt.Errorf("%w: read focus: %w", ErrStore, err)
		}
		f.At = time.Unix(0, ns).UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read focus: %w", ErrStore, err)
	}
	return out, nil
}

// FocusRows is how many rows the focus history holds, at most FocusCap.
func (s *Store) FocusRows(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM focus").Scan(&n); err != nil {
		return 0, fmt.Errorf("%w: count focus: %w", ErrStore, err)
	}
	return n, nil
}

// PruneHandoffs deletes the finished handoffs, acknowledged or failed, whose
// last change is more than HandoffRetention before now (A1), and reports how
// many it deleted. A pending handoff is never pruned.
func (s *Store) PruneHandoffs(ctx context.Context, now time.Time) (int64, error) {
	cutoff, err := nanos(now.Add(-HandoffRetention))
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM handoffs WHERE state IN ('acknowledged', 'failed') AND updated_at < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("%w: prune handoffs: %w", ErrStore, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%w: prune handoffs: %w", ErrStore, err)
	}
	return n, nil
}

// The times int64 nanoseconds since the Unix epoch can hold: 1677 to 2262.
var (
	minTime = time.Unix(0, math.MinInt64)
	maxTime = time.Unix(0, math.MaxInt64)
)

// nanos is t as stored: nanoseconds since the Unix epoch. A time outside
// what that holds, such as the zero time, is ErrInvalid rather than a wrong
// number.
func nanos(t time.Time) (int64, error) {
	if t.Before(minTime) || t.After(maxTime) {
		return 0, fmt.Errorf("%w: time %v is outside what nanoseconds since 1970 can hold", ErrInvalid, t)
	}
	return t.UnixNano(), nil
}

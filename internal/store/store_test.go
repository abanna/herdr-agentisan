package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // the driver the store registers, for raw connections

	"github.com/abanna/herdr-agentisan/internal/store"
)

// dbPath is a fresh state.db path in its own scratch directory. Every test
// gets a real database; the store is never mocked (ADR-001 A1).
func dbPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.db")
}

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// raw opens a second, plain connection to path, the way a person with
// sqlite3 would, to look at a database the store refused or left behind.
func raw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&v))
	return v
}

// objects lists the tables and triggers in db by name.
func objects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		"SELECT type || ':' || name FROM sqlite_schema WHERE type IN ('table', 'trigger') AND name NOT LIKE 'sqlite_%' ORDER BY 1")
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // read-only test query
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestOpenSetsAndVerifiesPragmas is A1's durability: WAL with
// synchronous=FULL, foreign keys enforced, and a busy timeout, read back from
// the live connection rather than assumed from the DSN.
func TestOpenSetsAndVerifiesPragmas(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	s := open(t, path)

	got, err := s.Pragmas(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.Pragmas{JournalMode: "wal", Synchronous: 2, ForeignKeys: true, BusyTimeout: 5 * time.Second}, got)
	assert.Equal(t, 5*time.Second, store.BusyTimeout)

	require.NoError(t, s.RecordFocus(t.Context(), "w1:p1", time.Now()))
	for _, f := range []string{path, path + "-wal"} {
		st, err := os.Stat(f)
		require.NoError(t, err, "WAL mode keeps a -wal file beside the database")
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "%s is the owner's only", filepath.Base(f))
	}
}

// TestVerifyPragmasRefusesAConnectionMissingOne: a pragma that silently did
// not take effect (a typo, an in-memory database, a driver that ignores the
// DSN) is an error, never a quietly weaker database.
func TestVerifyPragmasRefusesAConnectionMissingOne(t *testing.T) {
	t.Parallel()

	all := map[string]string{
		"busy_timeout": "busy_timeout(5000)",
		"foreign_keys": "foreign_keys(1)",
		"journal_mode": "journal_mode(WAL)",
		"synchronous":  "synchronous(FULL)",
	}
	dsn := func(path string, override map[string]string) string {
		var q []string
		for k, v := range all {
			if o, ok := override[k]; ok {
				v = o
			}
			if v != "" {
				q = append(q, "_pragma="+v)
			}
		}
		return "file:" + path + "?" + strings.Join(q, "&")
	}

	tests := map[string]struct {
		dsn     func(path string) string
		wantErr string // "" means the pragmas verify
	}{
		"every pragma set":     {dsn: func(p string) string { return dsn(p, nil) }},
		"rollback journal":     {dsn: func(p string) string { return dsn(p, map[string]string{"journal_mode": "journal_mode(DELETE)"}) }, wantErr: "journal_mode"},
		"synchronous NORMAL":   {dsn: func(p string) string { return dsn(p, map[string]string{"synchronous": "synchronous(NORMAL)"}) }, wantErr: "synchronous"},
		"foreign keys off":     {dsn: func(p string) string { return dsn(p, map[string]string{"foreign_keys": ""}) }, wantErr: "foreign_keys"},
		"no busy timeout":      {dsn: func(p string) string { return dsn(p, map[string]string{"busy_timeout": ""}) }, wantErr: "busy_timeout"},
		"a shorter timeout":    {dsn: func(p string) string { return dsn(p, map[string]string{"busy_timeout": "busy_timeout(10)"}) }, wantErr: "busy_timeout"},
		"an in-memory journal": {dsn: func(string) string { return ":memory:" }, wantErr: "journal_mode"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, err := sql.Open("sqlite", tc.dsn(dbPath(t)))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })

			err = store.VerifyPragmas(t.Context(), db)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, store.ErrPragma)
			assert.Contains(t, err.Error(), tc.wantErr, "the error names the pragma")
		})
	}
}

// TestOpenMigratesAndIsIdempotent: a fresh file gets the 0001 schema, and
// opening it again, any number of times, changes neither the schema nor the
// rows.
func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	path := dbPath(t)

	s, err := store.Open(t.Context(), path)
	require.NoError(t, err)
	assert.Equal(t, 1, store.SchemaVersion(), "0001 is the only migration so far")
	assert.Equal(t, store.SchemaVersion(), s.UserVersion())
	assert.Equal(t, []string{"table:focus", "table:handoffs", "table:workers", "trigger:focus_cap"}, objects(t, store.DB(s)),
		"messages belongs to NERD-5256 and arrives in its own migration")
	at := time.Date(2026, 10, 9, 12, 0, 0, 123456789, time.UTC)
	require.NoError(t, s.RecordFocus(t.Context(), "w1:p1", at))
	require.NoError(t, s.Close())

	for range 3 {
		s, err := store.Open(t.Context(), path)
		require.NoError(t, err)
		assert.Equal(t, store.SchemaVersion(), s.UserVersion())
		got, err := s.RecentFocus(t.Context(), store.FocusCap)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "w1:p1", got[0].PaneID)
		assert.True(t, at.Equal(got[0].At), "times round-trip to the nanosecond: %v", got[0].At)
		require.NoError(t, s.Close())
	}
	assert.Equal(t, store.SchemaVersion(), userVersion(t, raw(t, path)))
}

// TestOpenRefusesANewerSchema is A1: a database a newer build has migrated
// is refused, and its schema and version are left unchanged.
func TestOpenRefusesANewerSchema(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	const newer = 99
	setup, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = setup.ExecContext(t.Context(), fmt.Sprintf(
		"CREATE TABLE future (x INTEGER); INSERT INTO future VALUES (42); PRAGMA user_version = %d", newer))
	require.NoError(t, err)
	require.NoError(t, setup.Close())
	require.Equal(t, []byte{1, 1}, header(t, path), "the file starts in rollback-journal mode, as an older sqlite3 leaves it")

	s, err := store.Open(t.Context(), path)
	require.ErrorIs(t, err, store.ErrSchemaTooNew)
	assert.Nil(t, s)
	assert.Contains(t, err.Error(), fmt.Sprint(newer))

	// Refused before anything wrote: not even the switch to WAL.
	assert.Equal(t, []byte{1, 1}, header(t, path), "the refused file is not switched to WAL")
	assert.NoFileExists(t, path+"-wal")
	db := raw(t, path)
	var mode string
	require.NoError(t, db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "delete", mode)
	assert.Equal(t, newer, userVersion(t, db), "a refused database keeps its version")
	assert.Equal(t, []string{"table:future"}, objects(t, db))
	var x int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT x FROM future").Scan(&x))
	assert.Equal(t, 42, x, "its rows are intact")
}

// header is the database file's write and read format versions, bytes 18
// and 19 of its header: 1 for a rollback journal, 2 for WAL. Read from the
// file itself, so no connection's cached journal mode can stand in for it.
func header(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- a test's own scratch database
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(raw), 20)
	return raw[18:20]
}

// TestOpenIsSafeConcurrently: openers racing on one fresh file all succeed.
// Each reads the schema version under the write lock its migration
// transaction takes, so a migration another opener applied is never run
// again, and the switch to WAL outlasts the lock a concurrent opener holds.
// 4 openers on each of 20 fresh files: all 80 opens must succeed.
func TestOpenIsSafeConcurrently(t *testing.T) {
	t.Parallel()

	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }
	tests := map[string]struct {
		open    func(ctx context.Context, path string) (*store.Store, error)
		version int
	}{
		"the embedded migrations": {open: store.Open, version: store.SchemaVersion()},
		"two migrations": {
			open: func(ctx context.Context, path string) (*store.Store, error) {
				return store.OpenWith(ctx, path, fstest.MapFS{
					"0001_a.sql": file("CREATE TABLE a (x INTEGER);"),
					"0002_b.sql": file("CREATE TABLE b (y INTEGER REFERENCES a (x));"),
				})
			},
			version: 2,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var failures []error
			for range 20 {
				path := dbPath(t)
				errs := make([]error, 4)
				var wg sync.WaitGroup
				for i := range errs {
					wg.Go(func() {
						s, err := tc.open(t.Context(), path)
						if err != nil {
							errs[i] = err
							return
						}
						if v := s.UserVersion(); v != tc.version {
							errs[i] = fmt.Errorf("opened at version %d, want %d", v, tc.version)
						}
						errs[i] = errors.Join(errs[i], s.Close())
					})
				}
				wg.Wait()
				for _, err := range errs {
					if err != nil {
						failures = append(failures, err)
					}
				}
				assert.Equal(t, tc.version, userVersion(t, raw(t, path)))
			}
			assert.Empty(t, failures, "of 80 concurrent opens")
		})
	}
}

// TestOpenWaitsForALockUpToTheBusyTimeout: a lock another connection holds
// on a fresh file is waited for, up to BusyTimeout, then is ErrOpen rather
// than a hang. Two waits are bounded so:
//
//   - an exclusive lock keeps Open from even reading the file; SQLite's busy
//     handler waits it out, and a cancelled context does not cut that wait
//     short (SQLite does not interrupt a busy wait), so it still ends at the
//     timeout;
//   - a write lock (BEGIN IMMEDIATE) lets Open read but makes the switch to
//     WAL fail with SQLITE_BUSY at once, every time (SQLite will not wait
//     while holding the shared lock the writer needs released); enableWAL
//     retries it until BusyTimeout has passed, and a cancelled context ends
//     the retries at once.
func TestOpenWaitsForALockUpToTheBusyTimeout(t *testing.T) {
	t.Parallel()
	const long = store.BusyTimeout + 3*time.Second

	tests := map[string]struct {
		migrated bool   // the file is already a migrated WAL database
		lock     string // the statement the other connection holds its lock with
		hold     time.Duration
		cancel   time.Duration // cancels Open's context after this; 0 never
		want     error
		within   time.Duration // a failing Open ends no later than this
		after    time.Duration // and Open ends no sooner than this
	}{
		// No upper bound on a success: after the lock goes, a fresh file
		// costs ten fsyncs, which take seconds on a busy disk.
		"an exclusive lock released within the busy timeout": {lock: "BEGIN EXCLUSIVE", hold: 300 * time.Millisecond, after: 150 * time.Millisecond},
		"an exclusive lock held past the busy timeout": {
			lock: "BEGIN EXCLUSIVE", hold: long, want: store.ErrOpen,
			after: store.BusyTimeout - 200*time.Millisecond, within: store.BusyTimeout + 2*time.Second,
		},
		"a context cancelled during an exclusive lock's wait": {
			lock: "BEGIN EXCLUSIVE", hold: long, cancel: 200 * time.Millisecond, want: store.ErrOpen,
			within: store.BusyTimeout + 2*time.Second,
		},
		"a write lock released within the busy timeout": {lock: "BEGIN IMMEDIATE", hold: 300 * time.Millisecond, after: 150 * time.Millisecond},
		"a write lock held past the busy timeout": {
			lock: "BEGIN IMMEDIATE", hold: long, want: store.ErrOpen,
			after: store.BusyTimeout - 200*time.Millisecond, within: store.BusyTimeout + 2*time.Second,
		},
		"a context cancelled while the switch to WAL is retried": {
			lock: "BEGIN IMMEDIATE", hold: long, cancel: 200 * time.Millisecond, want: context.Canceled,
			within: 2 * time.Second,
		},
		// Already WAL and migrated: the switch passes, and the wait is the
		// migration transaction's BEGIN IMMEDIATE. A held lock is ErrOpen
		// there too, never a failed migration.
		"a write lock on a migrated database held past the busy timeout": {
			migrated: true, lock: "BEGIN IMMEDIATE", hold: long, want: store.ErrOpen,
			after: store.BusyTimeout - 200*time.Millisecond, within: store.BusyTimeout + 2*time.Second,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := dbPath(t)
			if tc.migrated {
				s, err := store.Open(t.Context(), path)
				require.NoError(t, err)
				require.NoError(t, s.Close())
			}
			conn, err := raw(t, path).Conn(t.Context())
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), tc.lock)
			require.NoError(t, err)
			var once sync.Once
			release := func() {
				once.Do(func() {
					_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
					_ = conn.Close()
				})
			}
			timer := time.AfterFunc(tc.hold, release)
			t.Cleanup(func() { timer.Stop(); release() })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel > 0 {
				time.AfterFunc(tc.cancel, cancel)
			}

			start := time.Now()
			s, err := store.Open(ctx, path)
			elapsed := time.Since(start)
			if tc.within > 0 {
				assert.Less(t, elapsed, tc.within)
			}
			assert.GreaterOrEqual(t, elapsed, tc.after)
			if tc.want == nil {
				require.NoError(t, err)
				require.NoError(t, s.Close())
				return
			}
			require.ErrorIs(t, err, tc.want)
			require.ErrorIs(t, err, store.ErrOpen)
			assert.Nil(t, s)
		})
	}
}

// TestMigrationClasses walks what a migration set can hold. Migrations run in
// order, each in its own transaction, from the version the file records; a
// set that is not exactly 0001..N is refused before anything runs.
func TestMigrationClasses(t *testing.T) {
	t.Parallel()

	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }
	tests := map[string]struct {
		seed    string // run on the raw file first
		fs      fstest.MapFS
		want    error
		version int
		objects []string
	}{
		"applied in order": {
			fs: fstest.MapFS{
				"0001_a.sql": file("CREATE TABLE a (x INTEGER);"),
				"0002_b.sql": file("CREATE TABLE b (y INTEGER REFERENCES a (x)); INSERT INTO a VALUES (1);"),
			},
			version: 2, objects: []string{"table:a", "table:b"},
		},
		"resumes from the recorded version": {
			seed: "CREATE TABLE a (x INTEGER); PRAGMA user_version = 1",
			fs: fstest.MapFS{
				"0001_a.sql": file("CREATE TABLE a (x INTEGER);"), // would fail if it ran again
				"0002_b.sql": file("CREATE TABLE b (y INTEGER);"),
			},
			version: 2, objects: []string{"table:a", "table:b"},
		},
		"a failing migration rolls back alone": {
			fs: fstest.MapFS{
				"0001_a.sql": file("CREATE TABLE a (x INTEGER);"),
				"0002_b.sql": file("CREATE TABLE b (y INTEGER); INSERT INTO nowhere VALUES (1);"),
			},
			want: store.ErrMigration, version: 1, objects: []string{"table:a"},
		},
		"other files are ignored": {
			fs: fstest.MapFS{
				"0001_a.sql": file("CREATE TABLE a (x INTEGER);"),
				"README.md":  file("not a migration"),
				"sub/x.sql":  file("CREATE TABLE nope (x INTEGER);"),
			},
			version: 1, objects: []string{"table:a"},
		},
		"no migrations on a fresh file": {fs: fstest.MapFS{}, version: 0},
		"a gap":                         {fs: fstest.MapFS{"0001_a.sql": file("SELECT 1;"), "0003_c.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"a duplicate version":           {fs: fstest.MapFS{"0001_a.sql": file("SELECT 1;"), "0001_b.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"not starting at 0001":          {fs: fstest.MapFS{"0002_b.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"version zero":                  {fs: fstest.MapFS{"0000_a.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"no number":                     {fs: fstest.MapFS{"init.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"no name":                       {fs: fstest.MapFS{"0001.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"a dash, not an underscore":     {fs: fstest.MapFS{"0001-a.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"an upper-case name":            {fs: fstest.MapFS{"0001_Init.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"a non-ASCII name":              {fs: fstest.MapFS{"0001_é.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"a space in the name":           {fs: fstest.MapFS{"0001_a b.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"a leading dash":                {fs: fstest.MapFS{"-0001_a.sql": file("SELECT 1;")}, want: store.ErrMigration},
		"an empty migration":            {fs: fstest.MapFS{"0001_a.sql": file("  \n-- nothing\n")}, want: store.ErrMigration},
		"a newer file than the set":     {seed: "PRAGMA user_version = 3", fs: fstest.MapFS{"0001_a.sql": file("SELECT 1;")}, want: store.ErrSchemaTooNew, version: 3},
		"a negative version":            {seed: "PRAGMA user_version = -1", fs: fstest.MapFS{"0001_a.sql": file("SELECT 1;")}, want: store.ErrMigration, version: -1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := dbPath(t)
			if tc.seed != "" {
				_, err := raw(t, path).ExecContext(t.Context(), tc.seed)
				require.NoError(t, err)
			}

			s, err := store.OpenWith(t.Context(), path, tc.fs)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Nil(t, s)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.version, s.UserVersion())
				require.NoError(t, s.Close())
			}
			db := raw(t, path)
			assert.Equal(t, tc.version, userVersion(t, db))
			assert.Equal(t, tc.objects, objects(t, db))
		})
	}
}

// TestOpenPathClasses: a path the store cannot use is ErrOpen, and leaves
// nothing behind at a path it was not given. A path with bytes a URI gives
// meaning to, or bytes that are not UTF-8, is still exactly that path.
func TestOpenPathClasses(t *testing.T) {
	t.Parallel()
	asRoot := os.Geteuid() == 0

	// locked is a directory under a fresh temp dir with mode perm, restored
	// before the temp dir is removed.
	locked := func(t *testing.T, perm os.FileMode, seed bool) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "locked")
		require.NoError(t, os.Mkdir(dir, 0o700))
		if seed {
			s, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
			require.NoError(t, err)
			require.NoError(t, s.Close())
		}
		require.NoError(t, os.Chmod(dir, perm))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		return dir
	}
	tests := map[string]struct {
		path  func(t *testing.T) string
		want  error
		perms bool // root reads and writes anything
	}{
		"a directory":     {path: func(t *testing.T) string { return t.TempDir() }, want: store.ErrOpen},
		"no parent":       {path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent", "state.db") }, want: store.ErrOpen},
		"an empty path":   {path: func(*testing.T) string { return "" }, want: store.ErrOpen},
		"an embedded NUL": {path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "state\x00.db") }, want: store.ErrOpen},
		"not a database": {path: func(t *testing.T) string {
			p := dbPath(t)
			require.NoError(t, os.WriteFile(p, []byte(strings.Repeat("not sqlite ", 100)), 0o600))
			return p
		}, want: store.ErrOpen},
		"a file the owner cannot write": {path: func(t *testing.T) string {
			p := dbPath(t)
			require.NoError(t, os.WriteFile(p, nil, 0o400))
			return p
		}, want: store.ErrOpen, perms: true},
		"a parent that cannot be searched": {path: func(t *testing.T) string {
			return filepath.Join(locked(t, 0o600, true), "state.db")
		}, want: store.ErrOpen, perms: true},
		"a parent that is not writable": {path: func(t *testing.T) string {
			return filepath.Join(locked(t, 0o500, false), "state.db")
		}, want: store.ErrOpen, perms: true},
		// WAL mode creates state.db-wal and -shm beside the database, so
		// even an existing database needs a writable parent.
		"an existing database in a parent that is not writable": {path: func(t *testing.T) string {
			return filepath.Join(locked(t, 0o500, true), "state.db")
		}, want: store.ErrOpen, perms: true},
		"URI metacharacters in the path": {path: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "a?b#c%d e&f=g\r\nh", "state.db")
		}},
		"non-ASCII and a byte-order mark in the path": {path: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "\ufeffé日本", "state.db")
		}},
		"bytes that are not UTF-8 in the path": {path: func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "mixé-\xe9-\xff-\xe2\x82", "state.db")
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tc.perms && asRoot {
				t.Skip("root reads and writes anything")
			}
			path := tc.path(t)
			if tc.want == nil {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			}

			s, err := store.Open(t.Context(), path)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Nil(t, s)
				return
			}
			require.NoError(t, err)
			require.NoError(t, s.RecordFocus(t.Context(), "w1:p1", time.Now()))
			require.NoError(t, s.Close())

			// The file lands at the path given: nothing is created at a
			// prefix of it cut at a '?', '#' or newline.
			assert.Equal(t, []string{filepath.Base(filepath.Dir(path))}, names(t, filepath.Dir(filepath.Dir(path))))
			assert.Contains(t, names(t, filepath.Dir(path)), "state.db")
			s = open(t, path)
			got, err := s.RecentFocus(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, got, 1, "reopening the same path finds the row")
		})
	}

	t.Run("a NUL never reaches SQLite as a shorter path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := store.Open(t.Context(), filepath.Join(dir, "state\x00.db"))
		require.ErrorIs(t, err, store.ErrOpen)
		assert.Empty(t, names(t, dir), "nothing is created at the path cut at the NUL")
	})
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestFocusHistory is the Back history (D5): newest first, capped at
// FocusCap, and a focus on the pane already most recently focused is not a
// new entry.
func TestFocusHistory(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		panes []string
		n     int
		want  []string
		rows  int
	}{
		"newest first":                     {panes: []string{"a", "b", "c"}, n: 10, want: []string{"c", "b", "a"}, rows: 3},
		"n bounds the answer":              {panes: []string{"a", "b", "c"}, n: 2, want: []string{"c", "b"}, rows: 3},
		"n of zero":                        {panes: []string{"a"}, n: 0, want: []string{}, rows: 1},
		"a negative n":                     {panes: []string{"a"}, n: -1, want: []string{}, rows: 1},
		"empty history":                    {n: 5, want: []string{}, rows: 0},
		"a repeat of the newest is one":    {panes: []string{"a", "b", "b", "b"}, n: 10, want: []string{"b", "a"}, rows: 2},
		"an older repeat is a new entry":   {panes: []string{"a", "b", "a"}, n: 10, want: []string{"a", "b", "a"}, rows: 3},
		"exactly the cap":                  {panes: seq(32), n: 100, want: rev(seq(32)), rows: 32},
		"the 33rd drops the oldest":        {panes: seq(33), n: 100, want: rev(seq(33)[1:]), rows: 32},
		"forty keep the newest thirty-two": {panes: seq(40), n: 100, want: rev(seq(40)[8:]), rows: 32},
		"n equal to the rows":              {panes: []string{"a", "b", "c"}, n: 3, want: []string{"c", "b", "a"}, rows: 3},
		"the largest n":                    {panes: []string{"a", "b"}, n: math.MaxInt, want: []string{"b", "a"}, rows: 2},
		// The repeat check compares bytes: herdr's ids are ASCII, and two
		// spellings are two panes.
		"an NFD spelling of the newest is another pane": {panes: []string{"w\u00e9", "we\u0301"}, n: 10, want: []string{"we\u0301", "w\u00e9"}, rows: 2},
		"another case of the newest is another pane":    {panes: []string{"w1:p1", "W1:P1"}, n: 10, want: []string{"W1:P1", "w1:p1"}, rows: 2},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			for i, p := range tc.panes {
				require.NoError(t, s.RecordFocus(t.Context(), p, t0.Add(time.Duration(i)*time.Second)))
			}

			got, err := s.RecentFocus(t.Context(), tc.n)
			require.NoError(t, err)
			ids := make([]string, 0, len(got))
			for i, f := range got {
				ids = append(ids, f.PaneID)
				assert.Equal(t, time.UTC, f.At.Location())
				if i > 0 {
					assert.Less(t, f.Seq, got[i-1].Seq, "newest first")
				}
			}
			assert.Equal(t, tc.want, ids)
			rows, err := s.FocusRows(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.rows, rows)
		})
	}
	assert.Equal(t, 32, store.FocusCap)
}

func seq(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("w1:p%d", i+1)
	}
	return out
}

func rev(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

// TestFocusCapHoldsForAnyWriter: the cap is the schema's, not the store's, so
// it holds for any writer, and counts rows rather than trusting seq to have
// no gaps.
func TestFocusCapHoldsForAnyWriter(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	for i := range 40 {
		_, err := db.ExecContext(t.Context(), "INSERT INTO focus (seq, pane_id, focused_at) VALUES (?, ?, ?)", (i+1)*10, fmt.Sprintf("p%d", i+1), i)
		require.NoError(t, err)
	}
	rows, err := s.FocusRows(t.Context())
	require.NoError(t, err)
	assert.Equal(t, store.FocusCap, rows)
	got, err := s.RecentFocus(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "p40", got[0].PaneID)
	var oldest string
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT pane_id FROM focus ORDER BY seq LIMIT 1").Scan(&oldest))
	assert.Equal(t, "p9", oldest)
}

// TestRecordFocusRejectsBadInput: an empty pane id names nothing, and a time
// outside what int64 nanoseconds hold would be stored as a wrong one.
func TestRecordFocusRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pane string
		at   time.Time
	}{
		"empty pane id":    {pane: "", at: time.Now()},
		"zero time":        {pane: "w1:p1", at: time.Time{}},
		"before year 1678": {pane: "w1:p1", at: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)},
		"after year 2262":  {pane: "w1:p1", at: time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			require.ErrorIs(t, s.RecordFocus(t.Context(), tc.pane, tc.at), store.ErrInvalid)
			rows, err := s.FocusRows(t.Context())
			require.NoError(t, err)
			assert.Zero(t, rows)
		})
	}
	t.Run("a pane id with any bytes round-trips", func(t *testing.T) {
		t.Parallel()
		s := open(t, dbPath(t))
		for _, p := range []string{
			"wé:p1;$(x) *", "w1\np1", "w1\rp1", "w1\r\n", "w1\x00p1", "'); DROP TABLE focus; --",
			"w1\xff", "w1\xe2\x82", "\ufeffw1:p1", "w\xe9", "wé\xe9", "-w1", "w1:" + strings.Repeat("p", 1<<20),
		} {
			require.NoError(t, s.RecordFocus(t.Context(), p, time.Now()))
			got, err := s.RecentFocus(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, p, got[0].PaneID)
		}
	})
}

// TestFocusTimes: a focus time is stored as int64 nanoseconds since 1970,
// UTC. Every instant that holds round-trips exactly, whatever its zone; one
// nanosecond outside it is refused rather than stored wrong.
func TestFocusTimes(t *testing.T) {
	t.Parallel()
	earliest := time.Unix(0, math.MinInt64).UTC()
	latest := time.Unix(0, math.MaxInt64).UTC()

	tests := map[string]struct {
		at   time.Time
		want error
	}{
		"the earliest instant that holds": {at: earliest},
		"one nanosecond before it":        {at: earliest.Add(-time.Nanosecond), want: store.ErrInvalid},
		"the latest instant that holds":   {at: latest},
		"one nanosecond after it":         {at: latest.Add(time.Nanosecond), want: store.ErrInvalid},
		"before 1970":                     {at: time.Date(1969, 7, 20, 20, 17, 40, 1, time.UTC)},
		"a time in another zone":          {at: time.Date(2026, 10, 9, 8, 0, 0, 1, time.FixedZone("EDT", -4*3600))},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			err := s.RecordFocus(t.Context(), "w1:p1", tc.at)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			got, err := s.RecentFocus(t.Context(), 1)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.True(t, tc.at.Equal(got[0].At), "%v round-trips as %v", tc.at, got[0].At)
			assert.Equal(t, time.UTC, got[0].At.Location())
		})
	}
}

// TestRecordFocusIsOneStatement: callers recording at once cannot both pass
// the repeat check, because the check and the insert are one statement on
// the store's one connection.
func TestRecordFocusIsOneStatement(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				assert.NoError(t, s.RecordFocus(t.Context(), "w1:p1", time.Now()))
			}
		})
	}
	wg.Wait()
	rows, err := s.FocusRows(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, rows, "200 concurrent records of one pane are one row")
}

// TestPruneHandoffs is A1's retention: a finished (acknowledged or failed)
// handoff is deleted once its last change is more than 14 days old. A pending
// one is never deleted, however old.
func TestPruneHandoffs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	tests := map[string]struct {
		state            string
		created, updated time.Duration // before now
		pruned           bool
	}{
		"acknowledged 15 days ago":               {state: "acknowledged", created: 16 * day, updated: 15 * day, pruned: true},
		"failed 15 days ago":                     {state: "failed", created: 15 * day, updated: 15 * day, pruned: true},
		"acknowledged exactly 14 days ago":       {state: "acknowledged", created: 14 * day, updated: 14 * day},
		"acknowledged 14 days and 1 ns ago":      {state: "acknowledged", created: 15 * day, updated: 14*day + 1, pruned: true},
		"acknowledged a day ago, created 20 ago": {state: "acknowledged", created: 20 * day, updated: day},
		"failed an hour ago":                     {state: "failed", created: time.Hour, updated: time.Hour},
		"pending for 30 days":                    {state: "pending", created: 30 * day, updated: 30 * day},
	}
	s := open(t, dbPath(t))
	db := store.DB(s)
	_, err := db.ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at) VALUES ('w', 'p', 'g', 'w1:p1', 1, 'active', 0)")
	require.NoError(t, err)
	ids := map[string]int64{}
	want := 0
	for name, tc := range tests {
		res, err := db.ExecContext(t.Context(),
			"INSERT INTO handoffs (logical_id, old_pane, new_pane, state, created_at, updated_at) VALUES ('w', 'w1:p1', 'w1:p2', ?, ?, ?)",
			tc.state, now.Add(-tc.created).UnixNano(), now.Add(-tc.updated).UnixNano())
		require.NoError(t, err, name)
		ids[name], err = res.LastInsertId()
		require.NoError(t, err)
		if tc.pruned {
			want++
		}
	}

	n, err := s.PruneHandoffs(t.Context(), now)
	require.NoError(t, err)
	assert.EqualValues(t, want, n)
	for name, tc := range tests {
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM handoffs WHERE id = ?", ids[name]).Scan(&count))
		assert.Equal(t, !tc.pruned, count == 1, name)
	}

	n, err = s.PruneHandoffs(t.Context(), now)
	require.NoError(t, err)
	assert.Zero(t, n, "pruning again finds nothing")
	_, err = s.PruneHandoffs(t.Context(), time.Time{})
	require.ErrorIs(t, err, store.ErrInvalid)
	assert.Equal(t, 14*day, store.HandoffRetention)
}

// TestSchemaConstraints: the schema refuses rows no writer should produce, so
// a bug in a later writer is an error rather than a row that is quietly
// wrong. Foreign keys failing here is also the proof foreign_keys=ON held on
// the store's connection.
func TestSchemaConstraints(t *testing.T) {
	t.Parallel()

	const worker = "INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, retired_at) VALUES "
	const handoff = "INSERT INTO handoffs (logical_id, old_pane, new_pane, state, created_at, updated_at) VALUES "
	tests := map[string]struct {
		sql string
		ok  bool
	}{
		"a worker":                          {sql: worker + "('w2', 'p', 'g', 'w1:p2', 0, 'active', 1, NULL)", ok: true},
		"a retired worker":                  {sql: worker + "('w2', 'p', 'g', 'w1:p2', 3, 'retired', 1, 2)", ok: true},
		"a worker without an id":            {sql: worker + "('', 'p', 'g', 'w1:p2', 0, 'active', 1, NULL)"},
		"a worker without a pane":           {sql: worker + "('w2', 'p', 'g', NULL, 0, 'active', 1, NULL)"},
		"a duplicate worker":                {sql: worker + "('w', 'p', 'g', 'w1:p2', 0, 'active', 1, NULL)"},
		"a negative generation":             {sql: worker + "('w2', 'p', 'g', 'w1:p2', -1, 'active', 1, NULL)"},
		"retired before it was created":     {sql: worker + "('w2', 'p', 'g', 'w1:p2', 0, 'retired', 5, 4)"},
		"a time that is text":               {sql: worker + "('w2', 'p', 'g', 'w1:p2', 0, 'active', 'yesterday', NULL)"},
		"a handoff":                         {sql: handoff + "('w', 'w1:p1', 'w1:p2', 'pending', 1, 1)", ok: true},
		"a handoff for no worker":           {sql: handoff + "('nobody', 'w1:p1', 'w1:p2', 'pending', 1, 1)"},
		"a handoff in an unknown state":     {sql: handoff + "('w', 'w1:p1', 'w1:p2', 'done', 1, 1)"},
		"a handoff state in another case":   {sql: handoff + "('w', 'w1:p1', 'w1:p2', 'PENDING', 1, 1)"},
		"a handoff without a new pane":      {sql: handoff + "('w', 'w1:p1', NULL, 'pending', 1, 1)"},
		"a handoff updated before creation": {sql: handoff + "('w', 'w1:p1', 'w1:p2', 'pending', 5, 4)"},
		"a focus without a pane":            {sql: "INSERT INTO focus (pane_id, focused_at) VALUES ('', 1)"},
		"a focus without a time":            {sql: "INSERT INTO focus (pane_id, focused_at) VALUES ('w1:p1', NULL)"},
		"a messages table":                  {sql: "INSERT INTO messages (body) VALUES ('hi')"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			db := store.DB(s)
			_, err := db.ExecContext(t.Context(), worker+"('w', 'p', 'g', 'w1:p1', 1, 'active', 0, NULL)")
			require.NoError(t, err)

			_, err = db.ExecContext(t.Context(), tc.sql)
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

// TestClosedStoreFails: once closed, every operation is an error a caller can
// branch on, never a panic or a silent success.
func TestClosedStoreFails(t *testing.T) {
	t.Parallel()
	s, err := store.Open(t.Context(), dbPath(t))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	ctx := context.Background()
	for name, op := range map[string]func() error{
		"RecordFocus":   func() error { return s.RecordFocus(ctx, "w1:p1", time.Now()) },
		"RecentFocus":   func() error { _, err := s.RecentFocus(ctx, 1); return err },
		"FocusRows":     func() error { _, err := s.FocusRows(ctx); return err },
		"PruneHandoffs": func() error { _, err := s.PruneHandoffs(ctx, time.Now()); return err },
		"Pragmas":       func() error { _, err := s.Pragmas(ctx); return err },
	} {
		require.ErrorIs(t, op(), store.ErrStore, name)
	}
	assert.Equal(t, store.SchemaVersion(), s.UserVersion(), "the version the store opened at stays readable")
}

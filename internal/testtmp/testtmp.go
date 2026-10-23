// Package testtmp puts a test binary's temporary directories on tmpfs
// (NERD-5503). The tests that start in-process daemons create a fresh
// state.db each, and its WAL switch, migrations and lock record cost an
// fsync apiece. On a disk whose journal other processes keep busy each
// fsync waits for a whole journal commit, and a daemon's start went past
// the tests' 10 s health wait; on tmpfs the same start takes under a
// second. Production keeps its state.db, and synchronous=FULL, on disk
// (ADR-001 A1): only the tests' state moves.
//
// Each package whose tests open a state.db or start a daemon calls Use from
// its TestMain. Only _test files import it.
package testtmp

import (
	"errors"
	"os"
)

const (
	// Base is where the tests' temporary directories go: Linux's shared
	// memory tmpfs.
	Base = "/dev/shm"
	// Env opts out: "keep" leaves TMPDIR as it is, to test on a disk on
	// purpose.
	Env = "HERDR_AGENTISAN_TEST_TMPDIR"
	// MinFree is the room Base must have. A container's /dev/shm defaults
	// to 64 MB, too small for a package's parallel tests. On tmpfs it is
	// the mount's size cap, not free memory: tmpfs pages are memory, and a
	// machine out of memory swaps them.
	MinFree = 256 << 20
	// maxBase is the longest base Use accepts. The deepest unix socket the
	// tests make, internal/cli's herdr plugin layout, is 88 bytes past the
	// base, and the daemon binds at most 103 (daemon.MaxSocketPath): Base
	// leaves 7 to spare, and a longer base would fail those tests with a
	// path error that hides why.
	maxBase = len(Base)
)

// ErrBase means the base is too long for the tests' deepest unix socket.
var ErrBase = errors.New("too long a base for the tests' unix sockets")

// disk is the temporary directory Use moved the tests from, or "" when it
// moved nothing. Use sets it in TestMain, before m.Run starts any test.
var disk string

// Disk is the temporary directory the tests had before Use moved them onto
// Base, or os.TempDir when it moved nothing. A test that needs a disk's own
// behaviour makes its files there: tmpfs never gives a new file an old
// inode number, which TestHerdrIdentityClasses needs a filesystem to do.
func Disk() string {
	if disk != "" {
		return disk
	}
	return os.TempDir()
}

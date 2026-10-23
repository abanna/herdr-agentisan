package daemon_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/testtmp"
)

// TestTheTestsRunOnTmpfs: this package's TestMain calls testtmp.Use, so its
// tests' state is on tmpfs wherever tmpfs is available (NERD-5503).
func TestTheTestsRunOnTmpfs(t *testing.T) {
	t.Parallel()
	testtmp.CheckUsed(t)
}

// TestHerdrSocketsAreOnTheDisk: a fake herdr's socket is made where the tests'
// directories were before testtmp moved them. Only a disk gives a new socket
// an old one's inode number, and without it no test reaches A3's birth-time
// checks: five mutants that ignore the birth time all pass on tmpfs. onDisk
// holds every fake herdr a test starts a daemon on, or reads the identity
// of, to the same place.
func TestHerdrSocketsAreOnTheDisk(t *testing.T) {
	t.Parallel()
	assert.Equal(t, filepath.Clean(testtmp.Disk()), filepath.Dir(herdrDir(t)),
		"a fake herdr's socket directory is in the tests' temporary directory from before tmpfs")
}

// onDisk fails t unless sock is in a directory herdrDir made: on the disk the
// tests had before tmpfs. opts and mustIdentity call it, so a fake herdr made
// anywhere else, such as shortDir's tmpfs, fails each test that starts a
// daemon on it or reads its identity, instead of leaving A3's birth-time
// checks untested with every test green (NERD-5503).
func onDisk(t *testing.T, sock string) {
	t.Helper()
	require.Equal(t, filepath.Clean(testtmp.Disk()), filepath.Dir(filepath.Dir(sock)),
		"%s: a fake herdr's socket is made with herdrDir, on the disk, so A3's birth-time checks are reached", sock)
}

package daemon_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

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
// checks: five mutants that ignore the birth time all pass on tmpfs.
func TestHerdrSocketsAreOnTheDisk(t *testing.T) {
	t.Parallel()
	assert.Equal(t, filepath.Clean(testtmp.Disk()), filepath.Dir(herdrDir(t)),
		"a fake herdr's socket directory is in the tests' temporary directory from before tmpfs")
}

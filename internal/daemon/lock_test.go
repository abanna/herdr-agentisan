package daemon_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
)

// TestTheProbeLetsGoWhereverItsFileIsCopied: Start's and Stop's probe takes
// a free lock for an instant. A flock belongs to the open file, and a child
// forked meanwhile holds a copy of it until it execs, so the probe lets go
// of the lock itself: with the copy still open, the next probe reads the
// lock free and a daemon takes it at once. A lock someone else holds is
// left to them.
func TestTheProbeLetsGoWhereverItsFileIsCopied(t *testing.T) {
	t.Parallel()

	t.Run("a free lock", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.lock")
		isHeld, letGo, err := daemon.HeldKeepingACopy(path)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, letGo()) })
		assert.False(t, isHeld)

		again, err := daemon.Held(path)
		require.NoError(t, err)
		assert.False(t, again, "the copy holds no lock")
		f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- the test's own temp file
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), "a daemon takes it at once")
	})

	t.Run("a held lock", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.lock")
		holder, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- the test's own temp file
		require.NoError(t, err)
		require.NoError(t, syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		isHeld, letGo, err := daemon.HeldKeepingACopy(path)
		require.NoError(t, err)
		assert.True(t, isHeld)
		require.NoError(t, letGo(), "no copy to let go of")

		again, err := daemon.Held(path)
		require.NoError(t, err)
		assert.True(t, again, "the probe left the holder its lock")
		require.NoError(t, holder.Close())
		again, err = daemon.Held(path)
		require.NoError(t, err)
		assert.False(t, again, "its holder gone, it is free")
	})
}

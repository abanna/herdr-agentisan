package daemon_test

import (
	"net"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
)

// TestHerdrIdentityWithoutStatx: where statx is unavailable (kernels before
// 4.11 answer ENOSYS, older container seccomp profiles EPERM), the identity
// falls back to stat: device, inode and change time. herdr itself runs on
// such hosts, so the daemon must too. Not parallel: it swaps the statx call
// for the whole package.
func TestHerdrIdentityWithoutStatx(t *testing.T) {
	path := filepath.Join(shortDir(t), "h.sock")
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	withStatx := mustIdentity(t, path)

	for name, errno := range map[string]error{"ENOSYS": syscall.ENOSYS, "EPERM": syscall.EPERM} {
		t.Run(name, func(t *testing.T) {
			restore := daemon.SetStatxForTest(func(string) error { return errno })
			defer restore()

			got, err := daemon.HerdrIdentity(path)
			require.NoError(t, err, "stat stands in for statx")
			assert.Equal(t, withStatx.Dev, got.Dev)
			assert.Equal(t, withStatx.Ino, got.Ino)
			assert.NotZero(t, got.Born, "the change time stands in for the birth time")
			assert.Equal(t, got, mustIdentity(t, path), "stable for the same socket")

			_, err = daemon.HerdrIdentity(filepath.Join(filepath.Dir(path), "absent.sock"))
			require.ErrorIs(t, err, daemon.ErrHerdrGone, "a missing socket is still gone")
		})
	}
}

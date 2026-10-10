//go:build unix

package herdrtest_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// TestSettleOnlySettlesItsOwnServer: Settle dials Path, and a test can remove
// or replace what Path names while the server is still open. Path naming the
// server's own socket, however it is spelled or reached, settles; anything
// else fails the test, and no other server is settled or sent a request.
func TestSettleOnlySettlesItsOwnServer(t *testing.T) {
	t.Parallel()

	// Each change returns the server Path now leads to, if it is not srv.
	tests := map[string]struct {
		change  func(t *testing.T, srv, other *herdrtest.Server) *herdrtest.Server
		settles bool
		nonRoot bool
		// linux: the change needs Linux (APFS refuses a name that is not
		// UTF-8).
		linux bool
	}{
		"its own socket": {settles: true, change: func(*testing.T, *herdrtest.Server, *herdrtest.Server) *herdrtest.Server { return nil }},
		"a relative path to its own socket": {settles: true, change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			wd, err := os.Getwd()
			require.NoError(t, err)
			rel, err := filepath.Rel(wd, srv.Path)
			require.NoError(t, err)
			srv.Path = rel // resolved against the working directory, through ..
			return nil
		}},
		"a symlink to its own socket": {settles: true, change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			alias := srv.Path + ".inode"
			require.NoError(t, os.Link(srv.Path, alias))
			replace(t, srv.Path, func(p string) error { return os.Symlink(alias, p) })
			return nil
		}},
		"renamed, Path following it": {settles: true, change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			// Non-ASCII, white space, a newline and a leading '-': bytes the
			// kernel takes as they are.
			renamed := filepath.Join(filepath.Dir(srv.Path), "-\u25c6 h\t\n.sock")
			require.NoError(t, os.Rename(srv.Path, renamed))
			srv.Path = renamed
			return nil
		}},
		"renamed to a name that is not UTF-8": {settles: true, linux: true, change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			renamed := filepath.Join(filepath.Dir(srv.Path), "h\xff\xe2\x97.sock") // invalid, then truncated
			require.NoError(t, os.Rename(srv.Path, renamed))
			srv.Path = renamed
			return nil
		}},
		"a path at the socket limit": {settles: true, change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			srv.Path = padded(t, srv.Path, sunPathMax())
			return nil
		}},
		"a path one past the socket limit": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			srv.Path = padded(t, srv.Path, sunPathMax()+1)
			return nil
		}},
		"an empty path": {change: func(_ *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			srv.Path = ""
			return nil
		}},
		"a NUL in the path": {change: func(_ *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			srv.Path += "\x00x" // the kernel would read the path only up to the NUL
			return nil
		}},
		"a symlink loop": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			replace(t, srv.Path, func(p string) error { return os.Symlink(p, p) })
			return nil
		}},
		"the socket removed": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			require.NoError(t, os.Remove(srv.Path))
			return nil
		}},
		"a regular file": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			replace(t, srv.Path, func(p string) error { return os.WriteFile(p, nil, 0o600) })
			return nil
		}},
		"a directory": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			replace(t, srv.Path, func(p string) error { return os.Mkdir(p, 0o700) })
			return nil
		}},
		"a FIFO": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			replace(t, srv.Path, func(p string) error { return syscall.Mkfifo(p, 0o600) })
			return nil
		}},
		"a dangling symlink": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			replace(t, srv.Path, func(p string) error { return os.Symlink(p+".gone", p) })
			return nil
		}},
		"a symlink to another server": {change: func(t *testing.T, srv, other *herdrtest.Server) *herdrtest.Server {
			replace(t, srv.Path, func(p string) error { return os.Symlink(other.Path, p) })
			return other
		}},
		"a hard link to another server's socket": {change: func(t *testing.T, srv, other *herdrtest.Server) *herdrtest.Server {
			replace(t, srv.Path, func(p string) error { return os.Link(other.Path, p) })
			return other
		}},
		"another server bound at the path": {change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			require.NoError(t, os.Remove(srv.Path))
			return herdrtest.StartAt(t, srv.Path, ok)
		}},
		"the socket not writable": {nonRoot: true, change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			chmod(t, srv.Path, 0o000)
			return nil
		}},
		"its directory not searchable": {nonRoot: true, change: func(t *testing.T, srv, _ *herdrtest.Server) *herdrtest.Server {
			chmod(t, filepath.Dir(srv.Path), 0o000)
			return nil
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tc.nonRoot && os.Geteuid() == 0 {
				t.Skip("root ignores permission bits")
			}
			if tc.linux && runtime.GOOS != "linux" {
				t.Skip("needs Linux")
			}
			srv := herdrtest.Start(t, ok)
			other := herdrtest.Start(t, ok)
			stranger := tc.change(t, srv, other)

			failure := settleFailure(t, srv)
			if tc.settles {
				assert.Empty(t, failure)
			} else {
				assert.Contains(t, failure, "herdrtest: settle: ")
			}
			assert.Empty(t, srv.Requests(), "Settle's own connection is never a request")
			if stranger != nil {
				assert.Empty(t, stranger.Requests(), "nor is it one to the server Path now leads to")
			}
		})
	}
}

// sunPathMax is the longest socket path the platform's sockaddr_un holds,
// its terminating NUL aside.
func sunPathMax() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// padded is path, n bytes long, spelled with extra slashes before its base.
func padded(t *testing.T, path string, n int) string {
	t.Helper()
	dir, base := filepath.Split(path)
	pad := n - len(dir) - len(base)
	require.GreaterOrEqual(t, pad, 0, "the temp dir is already longer than %d bytes", n)
	return dir + strings.Repeat("/", pad) + base
}

// replace swaps what path names for what create makes there.
func replace(t *testing.T, path string, create func(string) error) {
	t.Helper()
	require.NoError(t, os.Remove(path))
	require.NoError(t, create(path))
}

// chmod sets mode on path and restores it before the server and its
// directory are removed.
func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
}

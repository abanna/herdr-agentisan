package store_test

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestOpenKeepsSQLitesLock: in WAL mode SQLite holds a shared POSIX lock on
// the database file for as long as its connection is open. POSIX locks belong
// to the process, so closing any other descriptor this process has on the
// file would drop it, and a sqlite3 elsewhere could then delete the live WAL
// under the daemon. Open never opens the file itself once SQLite has (NERD-5364).
func TestOpenKeepsSQLitesLock(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	first, err := store.Open(t.Context(), path) // a WAL database to reopen
	require.NoError(t, err)
	require.NoError(t, first.Close())

	s := open(t, path)
	require.NoError(t, s.RecordFocus(t.Context(), "w1:p1", time.Now()))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	ino := fi.Sys().(*syscall.Stat_t).Ino
	require.True(t, holdsLock(t, ino), "this process holds a POSIX lock on state.db while the store is open")
}

// holdsLock reports whether /proc/locks lists a POSIX lock this process holds
// on the file with inode ino.
func holdsLock(t *testing.T, ino uint64) bool {
	t.Helper()
	data, err := os.ReadFile("/proc/locks")
	require.NoError(t, err)
	pid, suffix := fmt.Sprint(os.Getpid()), fmt.Sprintf(":%d", ino)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		// 1: POSIX  ADVISORY  READ 12345 fd:01:6815774 1073741826 1073742335
		if len(f) >= 6 && f[1] == "POSIX" && f[4] == pid && strings.HasSuffix(f[5], suffix) {
			return true
		}
	}
	return false
}

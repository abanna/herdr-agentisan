package store_test

import (
	"errors"
	"fmt"
	"io"
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
	require.True(t, holdsLock(t, ino, func() io.ReadCloser {
		f, err := os.Open("/proc/locks")
		require.NoError(t, err)
		return f
	}), "this process holds a POSIX lock on state.db while the store is open")
}

// holdsLock reports whether /proc/locks lists a POSIX lock this process holds
// on the file with inode ino.
func holdsLock(t *testing.T, ino uint64, open func() io.ReadCloser) bool {
	t.Helper()
	pid, suffix := fmt.Sprint(os.Getpid()), fmt.Sprintf(":%d", ino)
	page := os.Getpagesize()
	// Stagger page boundaries to survive a skipped lock line. See internal/testslots/testslots.go.
	for _, first := range []int{page, page / 3, 2 * page / 3} {
		data := readLockList(t, open(), first)
		for line := range strings.SplitSeq(data, "\n") {
			f := strings.Fields(line)
			// 1: POSIX  ADVISORY  READ 12345 fd:01:6815774 1073741826 1073742335
			if len(f) >= 6 && f[1] == "POSIX" && f[4] == pid && strings.HasSuffix(f[5], suffix) {
				return true
			}
		}
	}
	return false
}

func readLockList(t *testing.T, r io.ReadCloser, first int) string {
	t.Helper()
	defer func() { require.NoError(t, r.Close()) }()
	buf := make([]byte, os.Getpagesize())
	var list strings.Builder
	for size := first; ; size = len(buf) {
		n, err := r.Read(buf[:size])
		list.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			return list.String()
		}
		require.NoError(t, err)
	}
}

// lockListing models a read that skips the target line at particular page boundaries.
type lockListing func(first int) string

func (l lockListing) Read(buf []byte) (int, error) {
	return copy(buf, l(len(buf))), io.EOF
}

func TestHoldsLockAfterSkippedRead(t *testing.T) {
	t.Parallel()
	const ino = 123
	page := os.Getpagesize()
	misses := fmt.Sprintf("1: FLOCK ADVISORY READ %d 00:01:123 0 EOF\n"+
		"2: POSIX ADVISORY READ 0 00:01:123 0 EOF\n"+
		"3: POSIX ADVISORY READ %d 00:01:124 0 EOF\n", os.Getpid(), os.Getpid())
	hit := fmt.Sprintf("4: POSIX ADVISORY READ %d 00:01:123 0 EOF\n", os.Getpid())
	for _, visible := range []int{page, page / 3, 2 * page / 3, 0} {
		listing := lockListing(func(first int) string {
			if first == visible {
				return hit
			}
			return misses
		})
		found := holdsLock(t, ino, func() io.ReadCloser { return io.NopCloser(listing) })
		require.Equal(t, visible != 0, found, "lock visible only with first read size %d", visible)
	}
}

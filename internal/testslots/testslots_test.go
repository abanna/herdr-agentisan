package testslots_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/testslots"
)

// TestHeld: /proc/locks's lines name each held file by its device, major and
// minor in hex, and its inode in decimal. Every kind of lock counts; a
// waiter, a lease and a delegation do not. A line in any other form is
// ErrLocks, never a free slot.
func TestHeld(t *testing.T) {
	t.Parallel()
	file := func(major, minor uint32, inode uint64) testslots.File {
		return testslots.File{Major: major, Minor: minor, Inode: inode}
	}
	tests := map[string]struct {
		content string
		want    []testslots.File
		wantErr bool
	}{
		"nothing held":           {content: ""},
		"a blank line":           {content: "\n\n"},
		"a flock":                {content: "1: FLOCK  ADVISORY  WRITE 1778646 fc:01:69396573 0 EOF\n", want: []testslots.File{file(0xfc, 1, 69396573)}},
		"a shared flock":         {content: "1: FLOCK  ADVISORY  READ  1778646 00:1a:42 0 EOF\n", want: []testslots.File{file(0, 0x1a, 42)}},
		"a POSIX lock":           {content: "2: POSIX  ADVISORY  WRITE 1234 08:01:5678 0 EOF\n", want: []testslots.File{file(8, 1, 5678)}},
		"an OFD lock":            {content: "3: OFDLCK ADVISORY  WRITE -1 08:01:5679 0 EOF\n", want: []testslots.File{file(8, 1, 5679)}},
		"a range lock":           {content: "4: POSIX  ADVISORY  READ  1234 08:01:5680 100 199\n", want: []testslots.File{file(8, 1, 5680)}},
		"hex past nine":          {content: "1: FLOCK  ADVISORY  WRITE 7 1a:2b:77 0 EOF\n", want: []testslots.File{file(0x1a, 0x2b, 77)}},
		"a wide device":          {content: "1: FLOCK  ADVISORY  WRITE 7 103:fffff:9 0 EOF\n", want: []testslots.File{file(0x103, 0xfffff, 9)}},
		"two holders, one file":  {content: "1: FLOCK  ADVISORY  READ  1 08:01:9 0 EOF\n2: FLOCK  ADVISORY  READ  2 08:01:9 0 EOF\n", want: []testslots.File{file(8, 1, 9)}},
		"a waiter holds nothing": {content: "1: -> FLOCK  ADVISORY  WRITE 2 08:01:9 0 EOF\n"},
		"a holder and its waiter": {
			content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n1: -> FLOCK  ADVISORY  WRITE 2 08:01:9 0 EOF\n",
			want:    []testslots.File{file(8, 1, 9)},
		},
		"a lease is not a lock":       {content: "1: LEASE  ACTIVE    READ  1 08:01:9 0 EOF\n"},
		"a lease breaker, no file":    {content: "456: -> LEASE  BREAKER   WRITE 1131107 <none>:0 0 EOF\n"},
		"a breaker beside a lock":     {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n2: LEASE  BREAKING  READ  2 08:01:7 0 EOF\n2: -> LEASE  BREAKER   WRITE 3 <none>:0 0 EOF\n", want: []testslots.File{file(8, 1, 9)}},
		"a lock with no file":         {content: "1: FLOCK  ADVISORY  WRITE 1 <none>:0 0 EOF\n", wantErr: true},
		"a delegation is not a lock":  {content: "1: DELEG  ACTIVE    READ  1 08:01:9 0 EOF\n"},
		"no newline at the end":       {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF", want: []testslots.File{file(8, 1, 9)}},
		"a CRLF line":                 {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\r\n", want: []testslots.File{file(8, 1, 9)}},
		"invalid UTF-8 in a field":    {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9\xff 0 EOF\n", wantErr: true},
		"a byte-order mark":           {content: "\ufeff1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n", wantErr: true},
		"two files":                   {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n2: POSIX  ADVISORY  WRITE 1 fd:00:10 0 EOF\n", want: []testslots.File{file(8, 1, 9), file(0xfd, 0, 10)}},
		"too few fields":              {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0\n", wantErr: true},
		"too many fields":             {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF x\n", wantErr: true},
		"a waiter with too few":       {content: "1: -> FLOCK  ADVISORY  WRITE 1 08:01:9\n", wantErr: true},
		"an unknown type":             {content: "1: XLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n", wantErr: true},
		"no number":                   {content: "FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n", wantErr: true},
		"a number with no colon":      {content: "1 FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n", wantErr: true},
		"a number that is not one":    {content: "x: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n", wantErr: true},
		"a pid not a number":          {content: "1: FLOCK  ADVISORY  WRITE x 08:01:9 0 EOF\n", wantErr: true},
		"a pid past 64 bits":          {content: "1: FLOCK  ADVISORY  WRITE 99999999999999999999 08:01:9 0 EOF\n", wantErr: true},
		"a waiter pid not a number":   {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n1: -> FLOCK  ADVISORY  WRITE 1.5 08:01:9 0 EOF\n", wantErr: true},
		"a device and no inode":       {content: "1: FLOCK  ADVISORY  WRITE 1 08:01 0 EOF\n", wantErr: true},
		"a device that is not hex":    {content: "1: FLOCK  ADVISORY  WRITE 1 zz:01:9 0 EOF\n", wantErr: true},
		"an inode in hex":             {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:ff 0 EOF\n", wantErr: true},
		"a negative inode":            {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:-9 0 EOF\n", wantErr: true},
		"a major past 32 bits":        {content: "1: FLOCK  ADVISORY  WRITE 1 100000000:01:9 0 EOF\n", wantErr: true},
		"a NUL in a line":             {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9\x00 0 EOF\n", wantErr: true},
		"a good line, then a bad one": {content: "1: FLOCK  ADVISORY  WRITE 1 08:01:9 0 EOF\n2: FLOCK\n", wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := testslots.Held([]byte(tc.content))
			if tc.wantErr {
				require.ErrorIs(t, err, testslots.ErrLocks)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			want := map[testslots.File]bool{}
			for _, f := range tc.want {
				want[f] = true
			}
			assert.Equal(t, want, got)
		})
	}
}

// devOf is how /proc/locks would name path.
func devOf(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	dev := uint64(st.Dev) //nolint:unconvert // Dev is int32 on darwin
	return fmt.Sprintf("%02x:%02x:%d", unix.Major(dev), unix.Minor(dev), st.Ino)
}

// count reads procLocks once, judging its holders against the machine's own
// /proc, and counts paths against it.
func count(procLocks string, paths []string) (snapshot.Slots, error) {
	r, err := testslots.Read(procLocks, testslots.Proc)
	if err != nil {
		return snapshot.Slots{}, err //nolint:wrapcheck // the error under test
	}
	return r.Count(paths) //nolint:wrapcheck // the error under test
}

// TestCountWithAStandIn: Read reads whatever lock list it is given, so the
// matching itself is checked against lines written for the test's files.
func TestCountWithAStandIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.lock"), filepath.Join(dir, "b.lock")
	for _, p := range []string{a, b} {
		require.NoError(t, os.WriteFile(p, nil, 0o600))
	}
	fifo, dangling := filepath.Join(dir, "fifo.lock"), filepath.Join(dir, "dangling.lock")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone.lock"), dangling))
	locks := filepath.Join(dir, "locks")
	require.NoError(t, os.WriteFile(locks, []byte(
		"1: FLOCK  ADVISORY  WRITE 1 "+devOf(t, b)+" 0 EOF\n"+
			"2: -> FLOCK  ADVISORY  WRITE 2 "+devOf(t, a)+" 0 EOF\n"+
			"3: FLOCK  ADVISORY  WRITE 3 "+devOf(t, fifo)+" 0 EOF\n"), 0o600))

	got, err := count(locks, []string{a, b})
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 1, Total: 2}, got, "b is held; a only waited on")

	got, err = count(locks, []string{fifo, dangling})
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 1, Total: 2}, got, "a FIFO is matched like any file, never opened; a dangling symlink is free")

	got, err = count(locks, nil)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{}, got, "no lock files, no slots")
}

// TestCountFailures: when the held locks cannot be read, or a lock file
// cannot be looked at, Read or Count says so rather than report a slot free.
func TestCountFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "locks")
	require.NoError(t, os.WriteFile(good, nil, 0o600))
	big := filepath.Join(dir, "big")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("\n", testslots.MaxProcLocks+1)), 0o600))
	atTheCap := filepath.Join(dir, "at-the-cap")
	require.NoError(t, os.WriteFile(atTheCap, []byte(strings.Repeat("\n", testslots.MaxProcLocks)), 0o600))
	bad := filepath.Join(dir, "bad")
	require.NoError(t, os.WriteFile(bad, []byte("1: FLOCK\n"), 0o600))
	shut := filepath.Join(dir, "shut")
	require.NoError(t, os.MkdirAll(filepath.Join(shut, "in"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(shut, "in", "gate.lock"), nil, 0o600))
	require.NoError(t, os.Chmod(shut, 0o000))
	t.Cleanup(func() { _ = os.Chmod(shut, 0o700) })
	notDir := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))
	unreadable := filepath.Join(dir, "unreadable")
	require.NoError(t, os.WriteFile(unreadable, nil, 0o000))

	tests := map[string]struct {
		procLocks string
		paths     []string
		want      snapshot.Slots
		wantErr   error
	}{
		"no lock list":              {procLocks: filepath.Join(dir, "none"), paths: []string{good}, wantErr: testslots.ErrLocks},
		"a directory as the list":   {procLocks: dir, paths: []string{good}, wantErr: testslots.ErrLocks},
		"a list past the cap":       {procLocks: big, paths: []string{good}, wantErr: testslots.ErrLocks},
		"a list at the cap":         {procLocks: atTheCap, paths: []string{good}, want: snapshot.Slots{Total: 1}},
		"a list it may not read":    {procLocks: unreadable, paths: []string{good}, wantErr: testslots.ErrLocks},
		"a line it cannot read":     {procLocks: bad, paths: []string{good}, wantErr: testslots.ErrLocks},
		"a lock file under a file":  {procLocks: good, paths: []string{filepath.Join(notDir, "gate.lock")}, wantErr: testslots.ErrLockFile},
		"a lock file it cannot see": {procLocks: good, paths: []string{filepath.Join(shut, "in", "gate.lock")}, wantErr: testslots.ErrLockFile},
	}
	if os.Geteuid() == 0 {
		delete(tests, "a lock file it cannot see") // root searches it anyway
		delete(tests, "a list it may not read")    // and reads it
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := count(tc.procLocks, tc.paths)
			require.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, tc.want, got)
		})
	}
}

package testslots_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/testslots"
)

// The tests here hold real locks and read them back through the kernel's
// own /proc/locks, which only Linux has.

// lockFile creates path and holds a lock on it with lock, released at the
// end of the test; it returns the open file so a test can release it sooner.
func lockFile(t *testing.T, path string, lock func(fd int) error) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, lock(int(f.Fd())))
	return f
}

func flockEx(fd int) error { return syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) }
func flockSh(fd int) error { return syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB) }

func posixLock(fd int) error {
	return unix.FcntlFlock(uintptr(fd), unix.F_SETLK, &unix.Flock_t{Type: unix.F_WRLCK, Whence: 0})
}

func ofdLock(fd int) error {
	return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, &unix.Flock_t{Type: unix.F_WRLCK, Whence: 0})
}

// needProcLocks skips a test that reads the kernel's own lock list where
// there is none.
func needProcLocks(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(testslots.ProcLocks); err != nil {
		t.Skipf("no %s: %v", testslots.ProcLocks, err)
	}
}

// TestCountReadsTheKernel: with this process holding locks, the real
// /proc/locks says which of the listed files are taken. A held file that is
// not listed counts for nothing, a listed one that does not exist is free,
// and releasing a lock frees its slot at the next count.
func TestCountReadsTheKernel(t *testing.T) {
	t.Parallel()
	needProcLocks(t)
	dir := t.TempDir()
	gate0, gate1, gone := filepath.Join(dir, "gate.0.lock"), filepath.Join(dir, "gate.1.lock"), filepath.Join(dir, "gate.2.lock")
	held := lockFile(t, gate0, flockEx)
	require.NoError(t, os.WriteFile(gate1, nil, 0o600))
	lockFile(t, filepath.Join(dir, "decoy.lock"), flockEx)

	got, err := count(testslots.ProcLocks, []string{gate0, gate1, gone})
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 1, Total: 3}, got, "only gate.0 is held; the decoy is not listed")

	require.NoError(t, syscall.Flock(int(held.Fd()), syscall.LOCK_UN))
	got, err = count(testslots.ProcLocks, []string{gate0, gate1, gone})
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 0, Total: 3}, got, "released")
}

// TestCountEveryKindOfLock: a slot is taken by any lock on its file, flock,
// shared or exclusive, POSIX or open-file-description, held through any
// name the file has.
func TestCountEveryKindOfLock(t *testing.T) {
	t.Parallel()
	needProcLocks(t)
	tests := map[string]func(t *testing.T, dir string) (lock, listed string){
		"an exclusive flock": func(t *testing.T, dir string) (string, string) {
			p := filepath.Join(dir, "a.lock")
			lockFile(t, p, flockEx)
			return p, p
		},
		"two shared flocks": func(t *testing.T, dir string) (string, string) {
			p := filepath.Join(dir, "a.lock")
			lockFile(t, p, flockSh)
			lockFile(t, p, flockSh)
			return p, p
		},
		"a POSIX lock": func(t *testing.T, dir string) (string, string) {
			p := filepath.Join(dir, "a.lock")
			lockFile(t, p, posixLock)
			return p, p
		},
		"an OFD lock": func(t *testing.T, dir string) (string, string) {
			p := filepath.Join(dir, "a.lock")
			lockFile(t, p, ofdLock)
			return p, p
		},
		"listed through a symlink": func(t *testing.T, dir string) (string, string) {
			p := filepath.Join(dir, "a.lock")
			lockFile(t, p, flockEx)
			link := filepath.Join(dir, "link.lock")
			require.NoError(t, os.Symlink(p, link))
			return p, link
		},
		"held through a hard link": func(t *testing.T, dir string) (string, string) {
			p, other := filepath.Join(dir, "a.lock"), filepath.Join(dir, "b.lock")
			require.NoError(t, os.WriteFile(p, nil, 0o600))
			require.NoError(t, os.Link(p, other))
			lockFile(t, other, flockEx)
			return other, p
		},
		"a directory as the lock": func(t *testing.T, dir string) (string, string) {
			p := filepath.Join(dir, "slot.d")
			require.NoError(t, os.Mkdir(p, 0o700))
			f, err := os.Open(p)
			require.NoError(t, err)
			t.Cleanup(func() { _ = f.Close() })
			require.NoError(t, flockEx(int(f.Fd())))
			return p, p
		},
		"a name with spaces and non-ASCII": func(t *testing.T, dir string) (string, string) {
			p := filepath.Join(dir, "my slots", "gäte 0.lock")
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
			lockFile(t, p, flockEx)
			return p, p
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, listed := setup(t, t.TempDir())
			got, err := count(testslots.ProcLocks, []string{listed})
			require.NoError(t, err)
			assert.Equal(t, snapshot.Slots{Used: 1, Total: 1}, got)
		})
	}
}

// TestCountARealHold: the boss's hold, `flock <lock> sleep …` from
// util-linux, read back through the kernel's own lists (A40). Queued behind
// a run it holds nothing, and the run's slot is used; once the run lets go
// the hold takes the lock, and the slot is held, not used.
func TestCountARealHold(t *testing.T) {
	t.Parallel()
	needProcLocks(t)
	flock, err := exec.LookPath("flock")
	require.NoError(t, err, "util-linux flock(1) runs the hold under test")
	dir := t.TempDir()
	slot, other := filepath.Join(dir, "slot.lock"), filepath.Join(dir, "other.lock")
	run := lockFile(t, slot, flockEx)
	lockFile(t, other, flockEx)

	hold := exec.Command(flock, slot, "sleep", "60") // #nosec G204 -- flock from PATH, on a temp lock.
	hold.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, hold.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-hold.Process.Pid, syscall.SIGKILL)
		_ = hold.Wait()
	})
	waiting := regexp.MustCompile(`(?m)^\d+: -> FLOCK +ADVISORY +WRITE +` + strconv.Itoa(hold.Process.Pid) + ` +` + regexp.QuoteMeta(devOf(t, slot)) + ` `)
	require.Eventually(t, func() bool {
		locks, err := os.ReadFile(testslots.ProcLocks)
		return err == nil && waiting.Match(locks)
	}, 30*time.Second, 10*time.Millisecond, "the hold queues behind the run")
	got, err := count(testslots.ProcLocks, []string{slot, other})
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 2, Total: 2}, got, "a hold still waiting holds nothing")

	require.NoError(t, run.Close())
	require.Eventually(t, func() bool {
		got, err := count(testslots.ProcLocks, []string{slot, other})
		return err == nil && got == snapshot.Slots{Used: 1, Held: 1, Total: 2}
	}, 30*time.Second, 10*time.Millisecond, "the hold takes the slot once the run lets go")
}

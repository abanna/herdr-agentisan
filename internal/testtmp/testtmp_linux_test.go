package testtmp_test

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/testtmp"
)

// host is a stand-in for what Use reads and changes: one filesystem's
// statfs, access's answer, an environment, and stderr.
type host struct {
	st                         unix.Statfs_t
	statErr, accessErr, setErr error
	env                        map[string]string
	statted, accessed          []string
	modes                      []uint32
	set                        [][2]string
	stderr                     strings.Builder
}

func (h *host) use(base string) error {
	return testtmp.UseOn(base, h.statfs, h.access, h.getenv, h.setenv, &h.stderr)
}

func (h *host) check(tempDir string) (skip, fail string) {
	return testtmp.CheckUsedOn(tempDir, h.statfs, h.access, h.getenv, h.setenv, &h.stderr)
}

func (h *host) statfs(p string, st *unix.Statfs_t) error {
	h.statted = append(h.statted, p)
	if h.statErr != nil {
		return h.statErr
	}
	*st = h.st
	return nil
}

func (h *host) access(p string, mode uint32) error {
	h.accessed, h.modes = append(h.accessed, p), append(h.modes, mode)
	return h.accessErr
}

func (h *host) getenv(k string) string { return h.env[k] }

func (h *host) setenv(k, v string) error {
	if h.setErr != nil {
		return h.setErr
	}
	h.set = append(h.set, [2]string{k, v})
	return nil
}

// tmpfs is a tmpfs with free bytes free, in 1-byte blocks.
func tmpfs(free uint64) unix.Statfs_t {
	return unix.Statfs_t{Type: unix.TMPFS_MAGIC, Bsize: 1, Bavail: free}
}

// TestUseMovesTheStateOntoTmpfs: a writable tmpfs with MinFree free, to the
// byte, gets TMPDIR, and nothing is said. Only "keep" opts out, so an empty
// or other value of Env moves it all the same.
func TestUseMovesTheStateOntoTmpfs(t *testing.T) {
	t.Parallel()
	for _, opt := range []string{"", "no", "KEEP", "keep "} {
		h := &host{st: tmpfs(testtmp.MinFree), env: map[string]string{testtmp.Env: opt, "TMPDIR": "/scratch"}}
		require.NoError(t, h.use(testtmp.Base), "%q", opt)
		assert.Equal(t, [][2]string{{"TMPDIR", "/dev/shm"}}, h.set, "%q", opt)
		assert.Equal(t, []string{"/dev/shm"}, h.statted)
		assert.Equal(t, []string{"/dev/shm"}, h.accessed)
		assert.Equal(t, []uint32{unix.W_OK}, h.modes, "it asks whether it can write there")
		assert.Empty(t, h.stderr.String())
	}
}

// TestUseStaysOffABaseItCannotUse: an opt-out, a base it cannot stat, one
// that is no tmpfs, one it cannot write, one a byte short of MinFree, one
// whose block size reads negative and a TMPDIR it cannot set each leave
// TMPDIR alone, and one line says where the state goes: TMPDIR when it is
// set, else /tmp.
func TestUseStaysOffABaseItCannotUse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		host host
		why  string
	}{
		"opted out":           {host: host{st: tmpfs(testtmp.MinFree), env: map[string]string{testtmp.Env: "keep"}}, why: "HERDR_AGENTISAN_TEST_TMPDIR=keep"},
		"no such base":        {host: host{statErr: syscall.ENOENT}, why: "/dev/shm cannot be read: no such file or directory"},
		"a disk":              {host: host{st: unix.Statfs_t{Type: unix.EXT4_SUPER_MAGIC, Bsize: 1, Bavail: 1 << 40}}, why: "/dev/shm is not a tmpfs"},
		"read only":           {host: host{st: tmpfs(testtmp.MinFree), accessErr: syscall.EROFS}, why: "/dev/shm is not writable: read-only file system"},
		"a byte short":        {host: host{st: tmpfs(testtmp.MinFree - 1)}, why: "/dev/shm has 255 MiB free, under 256 MiB"},
		"a 64 MB container's": {host: host{st: unix.Statfs_t{Type: unix.TMPFS_MAGIC, Bsize: 4096, Bavail: 64 << 20 / 4096}}, why: "/dev/shm has 64 MiB free, under 256 MiB"},
		"a negative block":    {host: host{st: unix.Statfs_t{Type: unix.TMPFS_MAGIC, Bsize: -1, Bavail: 1}}, why: "/dev/shm has 0 MiB free, under 256 MiB"},
		"TMPDIR refused":      {host: host{st: tmpfs(testtmp.MinFree), setErr: errors.New("no")}, why: "TMPDIR cannot be set: no"},
	} {
		for dir, env := range map[string]string{"/tmp": "", "/scratch": "/scratch"} {
			h := tc.host
			h.env = map[string]string{"TMPDIR": env}
			maps.Copy(h.env, tc.host.env)
			require.NoError(t, h.use(testtmp.Base), name)
			assert.Empty(t, h.set, "%s: TMPDIR is left alone", name)
			assert.Equal(t, "testtmp: the tests' state stays in "+dir+": "+tc.why+"\n", h.stderr.String(), name)
		}
	}
	h := &host{env: map[string]string{testtmp.Env: "keep"}}
	require.NoError(t, h.use(testtmp.Base))
	assert.Empty(t, h.statted, "an opt-out reads nothing of the base")
}

// TestUseRefusesALongBase: a base longer than /dev/shm would put the tests'
// deepest socket past the daemon's limit, so it is refused before anything
// is read, set or said; one as long as /dev/shm is taken.
func TestUseRefusesALongBase(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/dev/shm", testtmp.Base)
	h := &host{st: tmpfs(testtmp.MinFree)}
	err := h.use("/dev/shm/")
	require.ErrorIs(t, err, testtmp.ErrBase)
	assert.Contains(t, err.Error(), "/dev/shm/ is 9 bytes, over 8")
	assert.Empty(t, h.statted)
	assert.Empty(t, h.set)
	assert.Empty(t, h.stderr.String())

	require.NoError(t, h.use("/run/shm"))
	assert.Equal(t, [][2]string{{"TMPDIR", "/run/shm"}}, h.set)
}

// TestDiskIsWhereTheTestsWere runs Use on this host: whether it moved the
// tests onto /dev/shm or not, Disk is the temporary directory they had
// before, never the tmpfs they were moved to.
func TestDiskIsWhereTheTestsWere(t *testing.T) {
	t.Setenv(testtmp.Env, "")
	t.Setenv("TMPDIR", t.TempDir())
	before := os.TempDir()
	testtmp.Use()
	assert.Equal(t, before, testtmp.Disk())
	t.Logf("TMPDIR after Use: %s", os.Getenv("TMPDIR"))
}

// TestUseOnTheHostKeepsAnOptOut runs Use on this host with the opt-out set:
// whatever /dev/shm is here, TMPDIR stays as it was.
func TestUseOnTheHostKeepsAnOptOut(t *testing.T) {
	t.Setenv(testtmp.Env, "keep")
	t.Setenv("TMPDIR", t.TempDir())
	before := os.Getenv("TMPDIR")
	testtmp.Use()
	assert.Equal(t, before, os.Getenv("TMPDIR"))
}

// TestCheckUsedFailsTestsOffTheBase: where Use would move the tests onto
// /dev/shm, a temporary directory on it or under it passes, and any other
// fails, one that only starts with /dev/shm included. CheckUsed reads the
// base as Use does, but sets and says nothing.
func TestCheckUsedFailsTestsOffTheBase(t *testing.T) {
	t.Parallel()
	for dir, off := range map[string]bool{"/dev/shm": false, "/dev/shm/x": false, "/tmp": true, "/dev/shmx": true, "/": true} {
		h := &host{st: tmpfs(testtmp.MinFree)}
		skip, fail := h.check(dir)
		assert.Empty(t, skip, dir)
		if off {
			assert.Equal(t, "the tests' temporary directory is "+dir+", not on /dev/shm: TestMain does not call testtmp.Use", fail)
		} else {
			assert.Empty(t, fail, dir)
		}
		assert.Equal(t, []string{"/dev/shm"}, h.statted, dir)
		assert.Equal(t, []uint32{unix.W_OK}, h.modes, dir)
		assert.Empty(t, h.set, "%s: TMPDIR is left alone", dir)
		assert.Empty(t, h.stderr.String(), dir)
	}
}

// TestCheckUsedSkipsWhereUseStaysOff: wherever Use would leave the tests
// off /dev/shm, CheckUsed skips with Use's own reason, whatever the
// temporary directory. A TMPDIR the host would refuse is no reason: it
// never sets one.
func TestCheckUsedSkipsWhereUseStaysOff(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		host host
		why  string
	}{
		"opted out":    {host: host{st: tmpfs(testtmp.MinFree), env: map[string]string{testtmp.Env: "keep"}}, why: "HERDR_AGENTISAN_TEST_TMPDIR=keep"},
		"no such base": {host: host{statErr: syscall.ENOENT}, why: "/dev/shm cannot be read: no such file or directory"},
		"a disk":       {host: host{st: unix.Statfs_t{Type: unix.EXT4_SUPER_MAGIC, Bsize: 1, Bavail: 1 << 40}}, why: "/dev/shm is not a tmpfs"},
		"read only":    {host: host{st: tmpfs(testtmp.MinFree), accessErr: syscall.EROFS}, why: "/dev/shm is not writable: read-only file system"},
		"a byte short": {host: host{st: tmpfs(testtmp.MinFree - 1)}, why: "/dev/shm has 255 MiB free, under 256 MiB"},
	} {
		for _, dir := range []string{"/tmp", "/dev/shm"} {
			h := tc.host
			skip, fail := h.check(dir)
			assert.Equal(t, "testtmp stays off /dev/shm: "+tc.why, skip, "%s in %s", name, dir)
			assert.Empty(t, fail, "%s in %s", name, dir)
			assert.Empty(t, h.set, name)
		}
	}
	h := &host{st: tmpfs(testtmp.MinFree), setErr: errors.New("no")}
	skip, fail := h.check("/dev/shm")
	assert.Empty(t, skip, "TMPDIR is never set, so a refusal is no reason")
	assert.Empty(t, fail)
}

// TestCheckUsedOnTheHost runs CheckUsed on this host, as each package's
// test does: an opt-out skips, saying so; where /dev/shm is a tmpfs Use
// would take, tests in /tmp fail and tests on it pass, and TMPDIR is left
// as it was. A host whose /dev/shm Use would not take skips the rest.
func TestCheckUsedOnTheHost(t *testing.T) {
	t.Setenv(testtmp.Env, "keep")
	t.Setenv("TMPDIR", "/tmp")
	skipped, failed := verdict()
	assert.Equal(t, "testtmp stays off /dev/shm: HERDR_AGENTISAN_TEST_TMPDIR=keep", skipped)
	assert.Empty(t, failed)

	t.Setenv(testtmp.Env, "")
	skipped, failed = verdict()
	if skipped != "" {
		t.Skip(skipped)
	}
	assert.Equal(t, "the tests' temporary directory is /tmp, not on /dev/shm: TestMain does not call testtmp.Use", failed)
	assert.Equal(t, "/tmp", os.Getenv("TMPDIR"), "CheckUsed never sets TMPDIR")

	t.Setenv("TMPDIR", testtmp.Base)
	skipped, failed = verdict()
	assert.Empty(t, skipped)
	assert.Empty(t, failed)
}

// verdict runs CheckUsed on a stand-in for a test that, like testing.T,
// ends CheckUsed's goroutine at Skip or Fatal, and returns what it said.
func verdict() (skipped, failed string) {
	b := &tb{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		testtmp.CheckUsed(b)
	}()
	<-done
	return b.skipped, b.failed
}

// tb is the stand-in: CheckUsed calls only Helper, Skip and Fatal.
type tb struct {
	testing.TB
	skipped, failed string
}

func (b *tb) Helper() {}

func (b *tb) Skip(args ...any) {
	b.skipped = fmt.Sprint(args...)
	runtime.Goexit()
}

func (b *tb) Fatal(args ...any) {
	b.failed = fmt.Sprint(args...)
	runtime.Goexit()
}

package testslots_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/testslots"
)

// TestHoldArgv: a hold is flock(1) holding its lock while sleep runs, in
// any of the forms flock reads (ADR-001 A40). Anything else is no hold, and
// its slot counts as used.
func TestHoldArgv(t *testing.T) {
	t.Parallel()
	const lock = "/tmp/agentisan-suite-1000.slot3.lock"
	tests := map[string]struct {
		argv []string
		want bool
	}{
		"the boss's hold":                {argv: []string{"flock", lock, "sleep", "86400"}, want: true},
		"paths for both":                 {argv: []string{"/usr/bin/flock", lock, "/usr/bin/sleep", "infinity"}, want: true},
		"-x":                             {argv: []string{"flock", "-x", lock, "sleep", "1"}, want: true},
		"-w and its value":               {argv: []string{"flock", "-w", "5", lock, "sleep"}, want: true},
		"-w5":                            {argv: []string{"flock", "-w5", lock, "sleep"}, want: true},
		"-xw and its value":              {argv: []string{"flock", "-xw", "5", lock, "sleep"}, want: true},
		"--timeout=5":                    {argv: []string{"flock", "--timeout=5", lock, "sleep"}, want: true},
		"--wait and its value":           {argv: []string{"flock", "--wait", "5", lock, "sleep"}, want: true},
		"every short option":             {argv: []string{"flock", "-s", "-e", "-n", "-o", "-E", "3", lock, "sleep"}, want: true},
		"every long option":              {argv: []string{"flock", "--shared", "--exclusive", "--nonblock", "--nb", "--close", "--verbose", "--conflict-exit-code=1", lock, "sleep"}, want: true},
		"--":                             {argv: []string{"flock", "--", lock, "sleep"}, want: true},
		"-c":                             {argv: []string{"flock", lock, "-c", "sleep 86400"}, want: true},
		"--command, with a path":         {argv: []string{"flock", lock, "--command", "/bin/sleep 1"}, want: true},
		"no argv":                        {},
		"a shell running flock":          {argv: []string{"sh", "-c", "flock " + lock + " sleep 1"}},
		"not flock":                      {argv: []string{"flockx", lock, "sleep"}},
		"another command":                {argv: []string{"flock", lock, "cat"}},
		"sleep only as a later argument": {argv: []string{"flock", lock, "sh", "-c", "sleep 1"}},
		"sleep after another command":    {argv: []string{"flock", lock, "nice", "sleep", "60"}},
		"sleep under timeout":            {argv: []string{"flock", lock, "timeout", "5", "sleep", "1"}},
		"no command":                     {argv: []string{"flock", lock}},
		"the fd form":                    {argv: []string{"flock", "-n", "9"}},
		"-c with two strings":            {argv: []string{"flock", lock, "-c", "sleep 1", "x"}},
		"-c with none":                   {argv: []string{"flock", lock, "-c"}},
		"-c running something first":     {argv: []string{"flock", lock, "-c", "exec sleep 1"}},
		"-F: flock becomes the command":  {argv: []string{"flock", "-F", lock, "sleep"}},
		"--no-fork":                      {argv: []string{"flock", "--no-fork", lock, "sleep"}},
		"-u":                             {argv: []string{"flock", "-u", lock, "sleep"}},
		"an option flock lacks":          {argv: []string{"flock", "-q", lock, "sleep"}},
		"an abbreviated long option":     {argv: []string{"flock", "--time=5", lock, "sleep"}},
		"a value for a flag":             {argv: []string{"flock", "--verbose=1", lock, "sleep"}},
		"-w with no value":               {argv: []string{"flock", "-w"}},
		"--wait with no value":           {argv: []string{"flock", "--wait"}},
		"options and no lock":            {argv: []string{"flock", "-w", "5"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, testslots.HoldArgv(tc.argv))
		})
	}
}

// fakeProc is a stand-in /proc for the holders a test writes into it.
type fakeProc struct {
	t    *testing.T
	root string
}

// process gives pid argv as its command line and an open file for each of
// files; nil argv writes no command line.
func (p fakeProc) process(pid int, argv []string, files ...string) {
	p.t.Helper()
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	require.NoError(p.t, os.MkdirAll(filepath.Join(dir, "fd"), 0o700))
	if argv != nil {
		require.NoError(p.t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o600))
	}
	for i, f := range files {
		require.NoError(p.t, os.Symlink(f, filepath.Join(dir, "fd", strconv.Itoa(i+3))))
	}
}

// TestCountHolds: a slot whose lock a hold keeps is held, not used (A40). A
// hold is shown by its command line and by its open file, never by a name;
// whatever cannot be shown is used, never held or free.
func TestCountHolds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.lock"), filepath.Join(dir, "b.lock")
	for _, p := range []string{a, b} {
		require.NoError(t, os.WriteFile(p, nil, 0o600))
	}
	holdOf := func(lock string) []string { return []string{"flock", lock, "sleep", "86400"} }
	flockLine := func(n int, access string, pid int, path string) string {
		return strconv.Itoa(n) + ": FLOCK  ADVISORY  " + access + " " + strconv.Itoa(pid) + " " + devOf(t, path) + " 0 EOF\n"
	}
	tests := map[string]struct {
		locks string
		setup func(p fakeProc)
		want  snapshot.Slots
	}{
		"a hold": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, holdOf(a), a) },
			want:  snapshot.Slots{Held: 1, Total: 1},
		},
		"a hold named by another path": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, holdOf("../elsewhere/x.lock"), a) },
			want:  snapshot.Slots{Held: 1, Total: 1},
		},
		"a hold with the lock among other files": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, holdOf(a), "/dev/null", b, a) },
			want:  snapshot.Slots{Held: 1, Total: 1},
		},
		"a hold of another lock": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, holdOf(a), b) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"a hold with no open file": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, holdOf(a)) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"a hold gone before its files are read": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) {
				p.process(100, holdOf(a), a)
				require.NoError(t, os.RemoveAll(filepath.Join(p.root, "100", "fd")))
			},
			want: snapshot.Slots{Used: 1, Total: 1},
		},
		"a dead pid": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(fakeProc) {},
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"no command line": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, nil, a) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"an empty command line, as a zombie's": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) {
				p.process(100, nil, a)
				require.NoError(t, os.WriteFile(filepath.Join(p.root, "100", "cmdline"), nil, 0o600))
			},
			want: snapshot.Slots{Used: 1, Total: 1},
		},
		"a command line past the cap": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, append(holdOf(a), strings.Repeat("x", 64<<10)), a) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"a command line at the cap": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) {
				argv := holdOf(a)
				size := len(strings.Join(argv, "\x00")) + 2 // a NUL before the padding, one after it
				p.process(100, append(argv, strings.Repeat("x", 64<<10-size)), a)
			},
			want: snapshot.Slots{Held: 1, Total: 1},
		},
		"not a hold": {
			locks: flockLine(1, "WRITE", 100, a),
			setup: func(p fakeProc) { p.process(100, []string{"go", "test"}, a) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"an OFD lock": {
			locks: "1: OFDLCK ADVISORY  WRITE -1 " + devOf(t, a) + " 0 EOF\n",
			setup: func(fakeProc) {},
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		// A suite's dead locker left pid 100 on the lock, and a hold
		// queued behind the suite was given pid 100.
		"the holder's pid also waits": {
			locks: flockLine(1, "WRITE", 100, a) + "1: -> FLOCK  ADVISORY  WRITE 100 " + devOf(t, a) + " 0 EOF\n",
			setup: func(p fakeProc) { p.process(100, holdOf(a), a) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"the holder's pid waits for another lock": {
			locks: flockLine(1, "WRITE", 100, a) + flockLine(2, "WRITE", 200, b) + "2: -> FLOCK  ADVISORY  WRITE 100 " + devOf(t, b) + " 0 EOF\n",
			setup: func(p fakeProc) { p.process(100, holdOf(a), a) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"a hold still waiting holds nothing": {
			locks: flockLine(1, "WRITE", 200, a) + "1: -> FLOCK  ADVISORY  WRITE 100 " + devOf(t, a) + " 0 EOF\n",
			setup: func(p fakeProc) { p.process(100, holdOf(a), a) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"a shared lock, one holder a hold": {
			locks: flockLine(1, "READ ", 300, a) + flockLine(2, "READ ", 100, a),
			setup: func(p fakeProc) {
				p.process(300, []string{"go", "test"}, a)
				p.process(100, holdOf(a), a)
			},
			want: snapshot.Slots{Held: 1, Total: 1},
		},
		"a shared lock, no holder a hold": {
			locks: flockLine(1, "READ ", 300, a) + flockLine(2, "READ ", 301, a),
			setup: func(p fakeProc) { p.process(300, []string{"go", "test"}, a) },
			want:  snapshot.Slots{Used: 1, Total: 1},
		},
		"a hold, a run and a free slot": {
			locks: flockLine(1, "WRITE", 100, a) + flockLine(2, "WRITE", 200, b),
			setup: func(p fakeProc) { p.process(100, holdOf(a), a) },
			want:  snapshot.Slots{Used: 1, Held: 1, Total: 3},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fakeProc{t: t, root: t.TempDir()}
			tc.setup(p)
			r, err := testslots.ReadingOf(p.root, []byte(tc.locks))
			require.NoError(t, err)
			paths := []string{a}
			if tc.want.Total == 3 {
				paths = []string{a, b, filepath.Join(dir, "free.lock")}
			}
			got, err := r.Count(paths)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestOneReadingManyLists: every list counted from one reading agrees, a
// lock file two lists name included, and a list's failure is its own.
func TestOneReadingManyLists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.lock"), filepath.Join(dir, "b.lock")
	for _, p := range []string{a, b} {
		require.NoError(t, os.WriteFile(p, nil, 0o600))
	}
	p := fakeProc{t: t, root: t.TempDir()}
	p.process(100, []string{"flock", a, "sleep", "1"}, a)
	r, err := testslots.ReadingOf(p.root, []byte("1: FLOCK  ADVISORY  WRITE 100 "+devOf(t, a)+" 0 EOF\n2: FLOCK  ADVISORY  WRITE 200 "+devOf(t, b)+" 0 EOF\n"))
	require.NoError(t, err)

	skills, err := r.Count([]string{a, b})
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 1, Held: 1, Total: 2}, skills)
	gate, err := r.Count([]string{b})
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 1, Total: 1}, gate)
	again, err := r.Count([]string{a, b})
	require.NoError(t, err)
	assert.Equal(t, skills, again, "the same list counts the same from one reading")

	notDir := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))
	_, err = r.Count([]string{a, filepath.Join(notDir, "gate.lock")})
	require.ErrorIs(t, err, testslots.ErrLockFile)
	gate, err = r.Count([]string{b})
	require.NoError(t, err, "one list's lock file failing fails no other list")
	assert.Equal(t, snapshot.Slots{Used: 1, Total: 1}, gate)
}

// TestAReadingIsTheUnionOfItsReads: the kernel lists the locks a page at a
// time, so a lock released mid-read can hide another; a reading holds every
// lock and every waiter any of its reads lists (A40).
func TestAReadingIsTheUnionOfItsReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := filepath.Join(dir, "a.lock")
	require.NoError(t, os.WriteFile(a, nil, 0o600))
	p := fakeProc{t: t, root: t.TempDir()}
	p.process(100, []string{"flock", a, "sleep", "1"}, a)
	holder := []byte("1: FLOCK  ADVISORY  WRITE 100 " + devOf(t, a) + " 0 EOF\n")
	waiter := []byte("1: -> FLOCK  ADVISORY  WRITE 100 " + devOf(t, a) + " 0 EOF\n")
	tests := map[string]struct {
		reads [][]byte
		want  snapshot.Slots
	}{
		"held in both":                  {reads: [][]byte{holder, holder}, want: snapshot.Slots{Held: 1, Total: 1}},
		"held in the first only":        {reads: [][]byte{holder, nil}, want: snapshot.Slots{Held: 1, Total: 1}},
		"held in the second only":       {reads: [][]byte{nil, holder}, want: snapshot.Slots{Held: 1, Total: 1}},
		"in neither":                    {reads: [][]byte{nil, nil}, want: snapshot.Slots{Total: 1}},
		"waiting in the first only":     {reads: [][]byte{append(append([]byte{}, holder...), waiter...), holder}, want: snapshot.Slots{Used: 1, Total: 1}},
		"waiting in the second only":    {reads: [][]byte{holder, append(append([]byte{}, waiter...), holder...)}, want: snapshot.Slots{Used: 1, Total: 1}},
		"the same holder in both, once": {reads: [][]byte{holder, holder, holder}, want: snapshot.Slots{Held: 1, Total: 1}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, err := testslots.ReadingOf(p.root, tc.reads...)
			require.NoError(t, err)
			got, err := r.Count([]string{a})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("a read it cannot parse", func(t *testing.T) {
		t.Parallel()
		_, err := testslots.ReadingOf(p.root, holder, []byte("1: FLOCK\n"))
		require.ErrorIs(t, err, testslots.ErrLocks)
	})
}

// TestReadReadsThreeStaggeredReads: Read reads the lock list three times
// for one reading, the first read() of each asking for a page, a third and
// two thirds of one, and keeps what any read lists; a read that fails fails
// the reading (A31, NERD-5341; A40).
func TestReadReadsThreeStaggeredReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var paths []string
	var reads [][]byte
	for i, name := range []string{"a.lock", "b.lock", "c.lock"} {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, nil, 0o600))
		paths = append(paths, p)
		reads = append(reads, []byte(fmt.Sprintf("1: FLOCK  ADVISORY  WRITE %d %s 0 EOF\n", 200+i, devOf(t, p))))
	}
	const page = 4096
	var firsts []int
	open := func() (io.ReadCloser, error) {
		r := &firstSize{r: bytes.NewReader(reads[len(firsts)%len(reads)]), first: -1}
		firsts = append(firsts, 0)
		r.got = &firsts[len(firsts)-1]
		return r, nil
	}
	r, err := testslots.ReadStaggered(t.TempDir(), "locks", open, page)
	require.NoError(t, err)
	assert.Equal(t, []int{page, page / 3, 2 * page / 3}, firsts, "three reads, their first read() a page, a third and two thirds of one")
	got, err := r.Count(paths)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Slots{Used: 3, Total: 3}, got, "a lock any read lists is taken")

	n := 0
	_, err = testslots.ReadStaggered(t.TempDir(), "locks", func() (io.ReadCloser, error) {
		if n++; n == 3 {
			return nil, errors.New("the third read")
		}
		return io.NopCloser(bytes.NewReader(reads[0])), nil
	}, page)
	require.ErrorIs(t, err, testslots.ErrLocks)
}

// firstSize records how much the first Read call asked for.
type firstSize struct {
	r     io.Reader
	first int
	got   *int
}

func (f *firstSize) Read(p []byte) (int, error) {
	if f.first < 0 {
		f.first = len(p)
		*f.got = len(p)
	}
	return f.r.Read(p) //nolint:wrapcheck // a stand-in reader
}

func (*firstSize) Close() error { return nil }

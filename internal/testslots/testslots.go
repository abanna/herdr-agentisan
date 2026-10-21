// Package testslots counts the dashboard header's test slots (ADR-001 A31,
// A40): how many of the configured lock files some process holds a lock on,
// as the kernel lists them in /proc/locks, and how many of those a boss's
// hold keeps. It only reads. It never opens, creates or locks a lock file, so
// it can neither take a slot nor delay whoever is taking one.
package testslots

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// ProcLocks is where Linux lists every lock held on the machine.
const ProcLocks = "/proc/locks"

// Proc is where Linux lists every process: a lock's holder is told for a
// hold by its command line and its open files.
const Proc = "/proc"

// MaxProcLocks bounds how much of /proc/locks is read.
const MaxProcLocks = 8 << 20

// maxCmdline bounds how much of a holder's command line is read; a longer
// one is no hold.
const maxCmdline = 64 << 10

// maxFDs bounds how many of a holder's open files are looked at.
const maxFDs = 1024

var (
	// ErrLocks means the held locks cannot be read: no /proc/locks, as off
	// Linux, or one too large, or a line in a form this package does not
	// know. A form it does not know must never read as every slot free.
	ErrLocks = errors.New("cannot read the held locks")
	// ErrLockFile means a lock file exists but cannot be looked at, so
	// whether it is held is unknown.
	ErrLockFile = errors.New("cannot look at a lock file")
)

// File is a file as /proc/locks names it: the device its filesystem is on,
// by major and minor number, and its inode.
type File struct {
	Major, Minor uint32
	Inode        uint64
}

// lockList is one pass of /proc/locks: each locked file's holders, and every
// process waiting for a lock.
type lockList struct {
	// holders is each locked file's holders, by pid; an open-file-description
	// lock's is -1.
	holders map[File][]int
	// waiting is every pid on a waiter line, whatever it waits for.
	waiting map[int]bool
}

// parse reads the content of /proc/locks. flock(2), POSIX and
// open-file-description locks all hold their file; a waiter (a "->" line)
// holds nothing, and a lease or a delegation is not a lock, so their files
// are not read: the kernel names none, "<none>:0", for the opener waiting to
// break a lease. A line in any other form is ErrLocks.
func parse(content []byte) (lockList, error) {
	l := lockList{holders: map[File][]int{}, waiting: map[int]bool{}}
	for n, line := range bytes.Split(content, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) == 0 {
			continue
		}
		e, err := parseLine(fields)
		if err != nil {
			return lockList{}, fmt.Errorf("%w: line %d: %w", ErrLocks, n+1, err)
		}
		switch {
		case e.waiter:
			l.waiting[e.pid] = true
		case e.lock:
			l.holders[e.file] = append(l.holders[e.file], e.pid)
		}
	}
	return l, nil
}

// Held reads the content of /proc/locks into the files some process holds a
// lock on, as parse reads it.
func Held(content []byte) (map[File]bool, error) {
	l, err := parse(content)
	if err != nil {
		return nil, err
	}
	held := make(map[File]bool, len(l.holders))
	for f := range l.holders {
		held[f] = true
	}
	return held, nil
}

// entry is one line of /proc/locks.
type entry struct {
	// lock is a flock(2), POSIX or open-file-description lock.
	lock   bool
	waiter bool
	pid    int
	// file is set for a lock that is not a waiter's.
	file File
}

// parseLine reads one line, split into fields:
//
//	<id>: [->] <type> <mode> <access> <pid> <major>:<minor>:<inode> <start> <end>
//
// with the major and minor in hex and the inode in decimal.
func parseLine(fields []string) (entry, error) {
	id, ok := strings.CutSuffix(fields[0], ":")
	if _, err := strconv.ParseUint(id, 10, 64); !ok || err != nil {
		return entry{}, fmt.Errorf("%q is not a lock's number", fields[0])
	}
	rest := fields[1:]
	var e entry
	if e.waiter = len(rest) > 0 && rest[0] == "->"; e.waiter {
		rest = rest[1:]
	}
	if len(rest) != 7 {
		return entry{}, fmt.Errorf("%d fields after the number, want 7", len(rest))
	}
	switch rest[0] {
	case "FLOCK", "POSIX", "OFDLCK":
		e.lock = true
	case "LEASE", "DELEG":
	default:
		return entry{}, fmt.Errorf("%q is not a lock type", rest[0])
	}
	pid, err := strconv.Atoi(rest[3])
	if err != nil {
		return entry{}, fmt.Errorf("%q is not a pid", rest[3])
	}
	e.pid = pid
	if !e.lock || e.waiter {
		return e, nil
	}
	if e.file, err = parseFile(rest[4]); err != nil {
		return entry{}, err
	}
	return e, nil
}

func parseFile(s string) (File, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return File{}, fmt.Errorf("%q is not major:minor:inode", s)
	}
	major, errMajor := strconv.ParseUint(parts[0], 16, 32)
	minor, errMinor := strconv.ParseUint(parts[1], 16, 32)
	inode, errInode := strconv.ParseUint(parts[2], 10, 64)
	if errMajor != nil || errMinor != nil || errInode != nil {
		return File{}, fmt.Errorf("%q is not major:minor:inode", s)
	}
	return File{Major: uint32(major), Minor: uint32(minor), Inode: inode}, nil
}

// Reading counts lock lists against one reading of /proc/locks, so every
// list counted from it agrees (A40). It looks each lock file up, and judges
// each locked file's holders, once however many lists name it. It is not
// safe for concurrent use.
type Reading struct {
	locks lockList
	// proc is where the holders' command lines and open files are read.
	proc  string
	files map[string]stat
	holds map[File]bool
}

// stat is a lock file looked up: its device and inode, and whether it exists.
type stat struct {
	file   File
	exists bool
	err    error
}

// passes is how many times Read reads the lock list for one reading.
const passes = 2

// Read reads the held locks from procLocks, the path of a /proc/locks, and
// judges their holders against proc, the root of a /proc.
//
// The kernel lists the locks a page at a time, and a lock released between
// two pages can shift the next page past a lock that is still held, which
// would read as free. So one reading is two passes, back to back, and holds
// every lock and every waiter either pass lists (A40). The kernel never lists
// a lock that was not there, so the union only keeps a lock released during
// the reading taken until the next poll.
func Read(procLocks, proc string) (*Reading, error) {
	return readPasses(proc, func() ([]byte, error) { return readList(procLocks) })
}

// readPasses is one reading made of passes calls to read, each one pass of
// the lock list.
func readPasses(proc string, read func() ([]byte, error)) (*Reading, error) {
	contents := make([][]byte, 0, passes)
	for range passes {
		content, err := read()
		if err != nil {
			return nil, err
		}
		contents = append(contents, content)
	}
	return reading(proc, contents...)
}

// reading is one reading made of the passes in contents.
func reading(proc string, contents ...[]byte) (*Reading, error) {
	l := lockList{holders: map[File][]int{}, waiting: map[int]bool{}}
	for _, content := range contents {
		p, err := parse(content)
		if err != nil {
			return nil, err
		}
		for f, pids := range p.holders {
			for _, pid := range pids {
				if !slices.Contains(l.holders[f], pid) {
					l.holders[f] = append(l.holders[f], pid)
				}
			}
		}
		maps.Copy(l.waiting, p.waiting)
	}
	return &Reading{locks: l, proc: proc, files: map[string]stat{}, holds: map[File]bool{}}, nil
}

// readList reads one pass of the lock list at procLocks.
func readList(procLocks string) ([]byte, error) {
	f, err := os.Open(procLocks) // #nosec G304 -- the kernel's lock list, or a test's stand-in for it; only read.
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocks, err)
	}
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(io.LimitReader(f, MaxProcLocks+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocks, err)
	}
	if len(content) > MaxProcLocks {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrLocks, procLocks, MaxProcLocks)
	}
	return content, nil
}

// Count is how many of paths some process holds a lock on, of len(paths),
// and how many of those a hold keeps (A40), which are held and not used. A
// lock file that does not exist is free: nothing has taken it yet. Each path
// is followed through symlinks, as flock(2) follows them.
func (r *Reading) Count(paths []string) (snapshot.Slots, error) {
	slots := snapshot.Slots{Total: len(paths)}
	for _, p := range paths {
		st, ok := r.files[p]
		if !ok {
			st.file, st.exists, st.err = fileOf(p)
			r.files[p] = st
		}
		switch {
		case st.err != nil:
			return snapshot.Slots{}, st.err
		case !st.exists || len(r.locks.holders[st.file]) == 0:
		case r.held(st.file):
			slots.Held++
		default:
			slots.Used++
		}
	}
	return slots, nil
}

// held reports whether a hold keeps f: whether any of its holders is a hold.
func (r *Reading) held(f File) bool {
	if h, ok := r.holds[f]; ok {
		return h
	}
	h := slices.ContainsFunc(r.locks.holders[f], func(pid int) bool { return r.isHold(pid, f) })
	r.holds[f] = h
	return h
}

// isHold reports whether pid holds f as a boss's hold does (A40): it runs
// `flock <f> sleep …`, and has f open. Anything this cannot show is no hold,
// and its slot is used: an open-file-description lock (-1), a pid that also
// waits for a lock, as a hold queued behind the run whose dead pid it was
// given would, a process gone or not readable, and an argv in another form.
func (r *Reading) isHold(pid int, f File) bool {
	if pid <= 0 || r.locks.waiting[pid] {
		return false
	}
	dir := filepath.Join(r.proc, strconv.Itoa(pid))
	argv, ok := cmdline(filepath.Join(dir, "cmdline"))
	return ok && HoldArgv(argv) && hasOpen(filepath.Join(dir, "fd"), f)
}

// cmdline reads a process's argv from its /proc cmdline: NUL-terminated
// strings, at most maxCmdline bytes of them. false is none to read: the
// process is gone, a zombie, not readable, or its argv is too long.
func cmdline(path string) ([]string, bool) {
	f, err := os.Open(path) // #nosec G304 -- a holder's /proc cmdline, or a test's stand-in; only read.
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxCmdline+1))
	if err != nil || len(raw) == 0 || len(raw) > maxCmdline {
		return nil, false
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), true
}

// hasOpen reports whether some entry of fds, a /proc/<pid>/fd, is f: by the
// device and inode it leads to, never by a path, which may be relative or
// renamed. At most maxFDs entries are looked at.
func hasOpen(fds string, f File) bool {
	d, err := os.Open(fds) // #nosec G304 -- a holder's /proc fd directory, or a test's stand-in; only read.
	if err != nil {
		return false
	}
	defer func() { _ = d.Close() }()
	entries, _ := d.ReadDir(maxFDs)
	for _, e := range entries {
		if g, ok, err := fileOf(filepath.Join(fds, e.Name())); err == nil && ok && g == f {
			return true
		}
	}
	return false
}

// HoldArgv reports whether argv is a hold's command line (A40): flock(1)
// holding its lock while sleep runs, as
//
//	flock [options] <lock> sleep …
//	flock [options] <lock> -c|--command 'sleep …'
//
// with argv[0], and the command, named by a base name or a path. The options
// are flock's own, spelt in full: -s, -x, -e, -n, -o, -w and -E, and their long
// forms. Any other is no hold, -F (--no-fork) among them: flock then becomes
// the command, so no holder runs flock.
func HoldArgv(argv []string) bool {
	if len(argv) == 0 || filepath.Base(argv[0]) != "flock" {
		return false
	}
	operands, ok := flockOperands(argv[1:])
	if !ok || len(operands) < 2 {
		return false
	}
	cmd := operands[1:] // operands[0] is the lock
	if cmd[0] == "-c" || cmd[0] == "--command" {
		if len(cmd) != 2 {
			return false
		}
		words := strings.Fields(cmd[1])
		return len(words) > 0 && isSleep(words[0])
	}
	return isSleep(cmd[0])
}

func isSleep(name string) bool { return name != "" && filepath.Base(name) == "sleep" }

// flockShort and flockLong are the flock(1) options a hold may give, each
// with whether it takes a value.
var (
	flockShort = map[byte]bool{'s': false, 'x': false, 'e': false, 'n': false, 'o': false, 'w': true, 'E': true}
	flockLong  = map[string]bool{
		"shared": false, "exclusive": false, "nonblock": false, "nb": false, "close": false, "verbose": false,
		"wait": true, "timeout": true, "conflict-exit-code": true,
	}
)

// flockOperands is args after flock's options, read as flock reads them: up
// to the first operand, or "--". false is an option a hold does not give.
func flockOperands(args []string) ([]string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return args[i+1:], true
		case strings.HasPrefix(a, "--"):
			name, _, inline := strings.Cut(a[2:], "=")
			takes, known := flockLong[name]
			if !known || (inline && !takes) {
				return nil, false
			}
			if takes && !inline {
				i++
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				takes, known := flockShort[a[j]]
				if !known {
					return nil, false
				}
				if takes {
					if j == len(a)-1 {
						i++ // the value is the next argument
					}
					break
				}
			}
		default:
			return args[i:], true
		}
	}
	return nil, false
}

// fileOf is path's device and inode, and false when there is no file there.
func fileOf(path string) (File, bool, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("%w: %w", ErrLockFile, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return File{}, false, fmt.Errorf("%w: %s: no device and inode", ErrLockFile, path)
	}
	dev := devNumber(st.Dev)
	return File{Major: unix.Major(dev), Minor: unix.Minor(dev), Inode: st.Ino}, true, nil
}

// devNumber is a stat device number as the uint64 unix.Major and unix.Minor
// take. Dev is a uint64 on linux and an int32 on darwin, where it is negative
// for a major of 128 or more; the conversion sign-extends it, and Major and
// Minor mask the extension off.
func devNumber[T ~int32 | ~uint64](dev T) uint64 { return uint64(dev) }

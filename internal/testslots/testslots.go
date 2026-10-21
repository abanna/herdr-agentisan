// Package testslots counts the dashboard header's test slots (ADR-001 A31):
// how many of the configured lock files some process holds a lock on, as the
// kernel lists them in /proc/locks. It only reads. It never opens, creates or
// locks a lock file, so it can neither take a slot nor delay whoever is
// taking one.
package testslots

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// ProcLocks is where Linux lists every lock held on the machine.
const ProcLocks = "/proc/locks"

// MaxProcLocks bounds how much of /proc/locks is read.
const MaxProcLocks = 8 << 20

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

// Held reads the content of /proc/locks into the files some process holds a
// lock on. flock(2), POSIX and open-file-description locks all count; a
// waiter (a "->" line) holds nothing, and a lease or a delegation is not a
// lock, so their files are not read: the kernel names none, "<none>:0", for
// the opener waiting to break a lease. A line in any other form is ErrLocks.
func Held(content []byte) (map[File]bool, error) {
	held := map[File]bool{}
	n := 0
	for line := range bytes.Lines(content) {
		n++
		fields := strings.Fields(string(line))
		if len(fields) == 0 {
			continue
		}
		f, holds, err := parseLine(fields)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %w", ErrLocks, n, err)
		}
		if holds {
			held[f] = true
		}
	}
	return held, nil
}

// parseLine reads one line, split into fields:
//
//	<id>: [->] <type> <mode> <access> <pid> <major>:<minor>:<inode> <start> <end>
//
// with the major and minor in hex and the inode in decimal.
func parseLine(fields []string) (File, bool, error) {
	id, ok := strings.CutSuffix(fields[0], ":")
	if _, err := strconv.ParseUint(id, 10, 64); !ok || err != nil {
		return File{}, false, fmt.Errorf("%q is not a lock's number", fields[0])
	}
	rest := fields[1:]
	waiter := len(rest) > 0 && rest[0] == "->"
	if waiter {
		rest = rest[1:]
	}
	if len(rest) != 7 {
		return File{}, false, fmt.Errorf("%d fields after the number, want 7", len(rest))
	}
	var lock bool
	switch rest[0] {
	case "FLOCK", "POSIX", "OFDLCK":
		lock = true
	case "LEASE", "DELEG":
	default:
		return File{}, false, fmt.Errorf("%q is not a lock type", rest[0])
	}
	if !lock || waiter {
		return File{}, false, nil
	}
	f, err := parseFile(rest[4])
	if err != nil {
		return File{}, false, err
	}
	return f, true, nil
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

// Count is how many of paths some process holds a lock on, of len(paths),
// with the held locks read from procLocks, the path of a /proc/locks. A lock
// file that does not exist is free: nothing has taken it yet. Each path is
// followed through symlinks, as flock(2) follows them.
func Count(procLocks string, paths []string) (snapshot.Slots, error) {
	held, err := read(procLocks)
	if err != nil {
		return snapshot.Slots{}, err
	}
	used := 0
	for _, p := range paths {
		f, exists, err := fileOf(p)
		if err != nil {
			return snapshot.Slots{}, err
		}
		if exists && held[f] {
			used++
		}
	}
	return snapshot.Slots{Used: used, Total: len(paths)}, nil
}

// read is the locks held in procLocks, the union of three whole reads of
// it. /proc/locks is a seq_file: each read() call returns at most a page,
// and the kernel starts each page again from the index where the last one
// stopped (fs/seq_file.c seq_read_iter, fs/locks.c locks_start). So when
// another process drops a lock listed before that index between two calls,
// every later line moves back one, and the next page skips one: a lock held
// all along reads as free. The skip can only fall where a page begins, so
// each read asks for a different first size, a page, a third and two
// thirds of one, which sets its pages a third of a page apart from the
// others'. A held lock is then missed only if all three reads begin a page
// on it.
//
// Measured with 64 locks held while goroutines took and dropped others
// (NERD-5341): one read missed a held lock in 1,974 of 3,000 counts with 8
// of them, two plain reads, whose pages begin in the same places, in 1,244,
// and three staggered reads in none of 30,000 with 16. The union can count a
// slot that was released during the reads as used: it was, during the count.
func read(procLocks string) (map[File]bool, error) {
	open := func() (io.ReadCloser, error) { return os.Open(procLocks) } // #nosec G304 -- the kernel's lock list, or a test's stand-in for it; only read.
	return readHeld(procLocks, open, os.Getpagesize())
}

// readHeld is the union of the locks held in three whole reads of what open
// opens, their first read() calls asking for page, page/3 and 2*page/3
// bytes. A read that fails or holds a line Held cannot read fails it all: a
// part of the list must never read as a slot free.
func readHeld(name string, open func() (io.ReadCloser, error), page int) (map[File]bool, error) {
	held := map[File]bool{}
	for _, first := range []int{page, page / 3, 2 * page / 3} {
		content, err := readWhole(name, open, max(first, 1))
		if err != nil {
			return nil, err
		}
		h, err := Held(content)
		if err != nil {
			return nil, err
		}
		maps.Copy(held, h)
	}
	return held, nil
}

// readChunk is how much each read() after the first asks for: more than a
// page, which is all /proc/locks gives a call.
const readChunk = 64 << 10

// readWhole reads what open opens to its end, its first read() call asking
// for first bytes, and fails past MaxProcLocks.
func readWhole(name string, open func() (io.ReadCloser, error), first int) ([]byte, error) {
	f, err := open()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLocks, err)
	}
	defer func() { _ = f.Close() }()
	var content []byte
	buf := make([]byte, max(first, readChunk))
	ask := first
	for {
		n, err := f.Read(buf[:ask])
		content = append(content, buf[:n]...)
		if len(content) > MaxProcLocks {
			return nil, fmt.Errorf("%w: %s is over %d bytes", ErrLocks, name, MaxProcLocks)
		}
		if errors.Is(err, io.EOF) {
			return content, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrLocks, err)
		}
		ask = len(buf)
	}
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

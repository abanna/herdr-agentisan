package report

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// MaxLineage bounds how many processes ReadLineage reads. A statusline report
// is about eight processes from init; the cap only stops a pathological chain.
const MaxLineage = 64

// ErrProcStat means a process's /proc stat could not be read or parsed, as
// when the process has already exited.
var ErrProcStat = errors.New("unreadable /proc stat")

// Stat is the part of a process's /proc stat a lineage needs.
type Stat struct {
	PPID int
	// Start is when the process started, in clock ticks since boot. A pid is
	// reused only by a process that starts later, so a pid with the same Start
	// at two reads is the same process, alive in between.
	Start uint64
}

// StatFunc reads a process's Stat.
type StatFunc func(pid int) (Stat, error)

// Proc is one process of a lineage, as it was when the lineage was read.
type Proc struct {
	PID   int
	Start uint64
}

// Lineage is the reporting process followed by its ancestors, nearest first,
// together with the reader that confirms each is still the process it was.
// Build one with ReadLineage; a zero Lineage confirms nothing, so a report
// given one lands nowhere.
type Lineage struct {
	Procs []Proc
	stat  StatFunc
}

// ReadLineage reads pid's lineage through stat. It stops before pid 1 (init
// runs in no pane) and at the first process that cannot be read, repeats,
// would exceed MaxLineage entries, or started after its child: a real parent,
// and any reaper that adopts an orphan, always started first, so a later one
// holds the pid of a parent that has exited.
//
// It never fails. A lineage cut short matches fewer panes, so whatever cannot
// be read can only make a report go nowhere, never to the wrong pane.
func ReadLineage(pid int, stat StatFunc) *Lineage {
	l := &Lineage{Procs: []Proc{}, stat: stat}
	seen := map[int]bool{}
	for pid > 1 && !seen[pid] && len(l.Procs) < MaxLineage {
		s, err := stat(pid)
		if err != nil {
			break
		}
		if n := len(l.Procs); n > 0 && s.Start > l.Procs[n-1].Start {
			break
		}
		l.Procs = append(l.Procs, Proc{PID: pid, Start: s.Start})
		seen[pid] = true
		pid = s.PPID
	}
	return l
}

// PIDs lists the lineage's pids, nearest first; nil for a nil Lineage.
func (l *Lineage) PIDs() []int {
	if l == nil {
		return nil
	}
	out := make([]int, 0, len(l.Procs))
	for _, p := range l.Procs {
		out = append(out, p.PID)
	}
	return out
}

// runsIn reports whether the pane herdr describes runs a process of the
// lineage: its shell, its foreground job's group leader, or a process of that
// job. A pid counts only if it is still the process the lineage read, so a
// pid that exited and was reused since then never matches. herdr reports a
// pid it does not know as 0, which no lineage holds.
func (l *Lineage) runsIn(info herdr.ProcessInfo) bool {
	candidates := make([]uint32, 0, 2+len(info.ForegroundProcesses))
	candidates = append(candidates, info.ShellPID, info.ForegroundProcessGroupID)
	for _, p := range info.ForegroundProcesses {
		candidates = append(candidates, p.PID)
	}
	return slices.ContainsFunc(candidates, l.confirm)
}

// confirm reports whether pid is in the lineage and still has the start time
// the lineage read for it.
func (l *Lineage) confirm(pid uint32) bool {
	if l.stat == nil {
		return false
	}
	for _, p := range l.Procs {
		if int64(p.PID) != int64(pid) {
			continue
		}
		s, err := l.stat(p.PID)
		return err == nil && s.Start == p.Start
	}
	return false
}

// ProcStat reads Stats from a Linux procfs mounted at proc.
func ProcStat(proc fs.FS) StatFunc {
	return func(pid int) (Stat, error) {
		raw, err := fs.ReadFile(proc, strconv.Itoa(pid)+"/stat")
		if err != nil {
			return Stat{}, fmt.Errorf("%w: %w", ErrProcStat, err)
		}
		return parseStat(pid, raw)
	}
}

// parseStat reads a stat line, "pid (comm) state ppid ... starttime ...".
// comm is the executable name and may itself hold spaces and parentheses, so
// fields are counted from the LAST ')': after it, the parent (field 4 in
// proc(5)) is index 1 and the start time (field 22) index 19.
func parseStat(pid int, raw []byte) (Stat, error) {
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return Stat{}, fmt.Errorf("%w: %d: no (comm) field", ErrProcStat, pid)
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return Stat{}, fmt.Errorf("%w: %d: %d fields after comm, want at least 20", ErrProcStat, pid, len(fields))
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return Stat{}, fmt.Errorf("%w: %d: parent %q is not a pid", ErrProcStat, pid, fields[1])
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return Stat{}, fmt.Errorf("%w: %d: start time %q: %w", ErrProcStat, pid, fields[19], err)
	}
	return Stat{PPID: ppid, Start: start}, nil
}

// SelfLineage is the running process's lineage, read from /proc. It is nil
// where the OS has no procfs: a report given no lineage trusts HERDR_PANE_ID
// unchecked, as it did before lineage existed.
func SelfLineage() *Lineage {
	if !procfs {
		return nil
	}
	return ReadLineage(os.Getpid(), ProcStat(os.DirFS("/proc")))
}

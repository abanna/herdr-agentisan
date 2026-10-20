package report

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// The darwin process table (ADR-001 A26). The sysctls themselves are in
// lineage_darwin.go; what they return is parsed here, with no build tag, so
// the parsing is tested on every OS.

// maxProcArgs2Padding is the most NULs XNU writes after the exec path's own
// terminator: it pads the string area to the target's pointer size
// (exec_extract_strings, kern_exec.c), so fewer than 8.
const maxProcArgs2Padding = 7

// Kinfo is the part of a darwin kinfo_proc a lineage needs: p_pid,
// e_ppid and p_starttime, the wall-clock time the process forked, which
// exec keeps.
type Kinfo struct {
	PID       int
	PPID      int
	StartSec  int64
	StartUsec int64
}

// KinfoStat reads Stats through read, which returns a process's kinfo_proc
// (sysctl kern.proc.pid.<pid> on darwin). Start is in microseconds since the
// epoch.
func KinfoStat(read func(pid int) (Kinfo, error)) StatFunc {
	return func(pid int) (Stat, error) {
		k, err := read(pid)
		if err != nil {
			return Stat{}, fmt.Errorf("%w: %w", ErrProcStat, err)
		}
		switch {
		case k.PID != pid:
			return Stat{}, fmt.Errorf("%w: %d: kinfo_proc is for pid %d", ErrProcStat, pid, k.PID)
		case k.PPID < 0:
			return Stat{}, fmt.Errorf("%w: %d: parent %d is not a pid", ErrProcStat, pid, k.PPID)
		case k.StartSec < 0 || k.StartSec > (math.MaxInt64-999_999)/1_000_000:
			return Stat{}, fmt.Errorf("%w: %d: start second %d is out of range", ErrProcStat, pid, k.StartSec)
		case k.StartUsec < 0 || k.StartUsec >= 1_000_000:
			return Stat{}, fmt.Errorf("%w: %d: start microsecond %d is out of range", ErrProcStat, pid, k.StartUsec)
		}
		return Stat{PPID: k.PPID, Start: uint64(k.StartSec*1_000_000 + k.StartUsec)}, nil // #nosec G115 -- both checked non-negative and in range above
	}
}

// ProcArgs2Cmdline reads argument vectors through read, which returns a
// process's kern.procargs2 (darwin). The sysctl sizes its answer in one call
// and fills it in a second, and an exec in between returns a slice of the new
// arguments that does not start where the parse does; so read runs twice and
// the arguments count only when both reads agree byte for byte.
func ProcArgs2Cmdline(read func(pid int) ([]byte, error)) CmdlineFunc {
	return func(pid int) ([]string, error) {
		first, err := read(pid)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrProcCmdline, err)
		}
		second, err := read(pid)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrProcCmdline, err)
		}
		if !bytes.Equal(first, second) {
			return nil, fmt.Errorf("%w: %d: the arguments changed between two reads", ErrProcCmdline, pid)
		}
		return parseProcArgs2(pid, first)
	}
}

// parseProcArgs2 reads argv from a kern.procargs2 buffer: argc as a native
// int32 (little-endian on every darwin port), the exec path and its NUL, up to
// maxProcArgs2Padding NULs of alignment, argv's argc strings, each with its
// NUL, then the environment, which the kernel leaves out for a restricted
// target. argv[0] is what the Linux path's /proc/<pid>/cmdline gives too, so
// both compare the same name; the exec path is not it.
//
// The alignment NULs are skipped as the kernel itself skips them. So an empty
// argv[0] that the padding hides reads as the next argument; more NULs than
// any padding can only come from an empty argument, and are refused.
func parseProcArgs2(pid int, raw []byte) ([]string, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("%w: %d: %d bytes, too short for argc", ErrProcCmdline, pid, len(raw))
	}
	argc := int32(binary.LittleEndian.Uint32(raw)) // #nosec G115 -- the kernel writes argc as an int32
	rest := raw[4:]
	path := bytes.IndexByte(rest, 0)
	if path <= 0 {
		return nil, fmt.Errorf("%w: %d: no exec path", ErrProcCmdline, pid)
	}
	rest = rest[path+1:]
	argv := bytes.TrimLeft(rest, "\x00")
	if pad := len(rest) - len(argv); pad > maxProcArgs2Padding {
		return nil, fmt.Errorf("%w: %d: %d NULs after the exec path, more than alignment pads, so an argument is empty", ErrProcCmdline, pid, pad)
	}
	// Every argument takes at least its NUL, which also bounds the allocation.
	if argc < 0 || int(argc) > len(argv) {
		return nil, fmt.Errorf("%w: %d: argc %d does not fit %d bytes of arguments", ErrProcCmdline, pid, argc, len(argv))
	}
	out := make([]string, 0, argc)
	for i := range int(argc) {
		end := bytes.IndexByte(argv, 0)
		if end < 0 {
			return nil, fmt.Errorf("%w: %d: argument %d of %d has no terminator", ErrProcCmdline, pid, i, argc)
		}
		out = append(out, string(argv[:end]))
		argv = argv[end+1:]
	}
	return out, nil
}

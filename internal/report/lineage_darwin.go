package report

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// systemReaders reads the process table through sysctl (A26): each
// process's kinfo_proc for its parent and start time, and its
// kern.procargs2 for its arguments. Both are parsed in sysctl.go.
func systemReaders() (StatFunc, CmdlineFunc) {
	return KinfoStat(sysctlKinfo), ProcArgs2Cmdline(sysctlProcArgs2)
}

// sysctlKinfo reads pid's kinfo_proc. A pid with no process fails, since
// the kernel returns no record and x/sys then reports EIO.
func sysctlKinfo(pid int) (Kinfo, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Kinfo{}, fmt.Errorf("sysctl kern.proc.pid.%d: %w", pid, err)
	}
	return Kinfo{
		PID:       int(k.Proc.P_pid),
		PPID:      int(k.Eproc.Ppid),
		StartSec:  k.Proc.P_starttime.Sec,
		StartUsec: int64(k.Proc.P_starttime.Usec),
	}, nil
}

// sysctlProcArgs2 reads pid's kern.procargs2. The kernel refuses another
// user's process with EINVAL, unless the caller is root.
func sysctlProcArgs2(pid int) ([]byte, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.procargs2.%d: %w", pid, err)
	}
	return raw, nil
}

// systemStartedAt puts kinfo_proc's starts, already microseconds since the
// epoch, on the wall clock.
func systemStartedAt() StartedAtFunc {
	return KinfoStartedAt
}

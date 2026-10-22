package report

import "os"

// systemReaders reads the process table from /proc.
func systemReaders() (StatFunc, CmdlineFunc) {
	proc := os.DirFS("/proc")
	return ProcStat(proc), ProcCmdline(proc)
}

// systemStartedAt puts /proc's starts on the wall clock through /proc/stat's
// boot time.
func systemStartedAt() StartedAtFunc {
	return ProcStartedAt(os.DirFS("/proc"))
}

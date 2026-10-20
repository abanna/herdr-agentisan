package report

import "os"

// systemReaders reads the process table from /proc.
func systemReaders() (StatFunc, CmdlineFunc) {
	proc := os.DirFS("/proc")
	return ProcStat(proc), ProcCmdline(proc)
}

//go:build unix

package settings

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// killGroup starts op as the leader of a process group of its own and makes
// cancellation kill the whole group, so nothing op started outlives a
// timeout or a cancel.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(syscall.Kill, cmd.Process.Pid) }
}

// signalGroup sends SIGKILL, through kill (syscall.Kill), to the process
// group that pid leads, and is what op's Cancel does. A group already gone
// is os.ErrProcessDone, which exec takes as done. Any other failure goes
// back to exec, which kills the leader alone once WaitDelay passes, and the
// read still reports the cancel or the timeout. kill passes over a member it
// may not signal, so an EPERM means it could signal none of them, the leader
// included: exec's kill fails too, and the read waits for the leader to exit.
func signalGroup(kill func(pid int, sig syscall.Signal) error, pid int) error {
	err := kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if err != nil {
		return fmt.Errorf("kill op's process group: %w", err)
	}
	return nil
}

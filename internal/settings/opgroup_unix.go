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
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		if err != nil {
			return fmt.Errorf("kill op's process group: %w", err)
		}
		return nil
	}
}

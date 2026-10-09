package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

// ExecSpawn returns a Spawn hook that starts exe with args as a detached
// process: a new session (setsid), stdio on /dev/null and the caller's
// environment, so the daemon is tied to no shell, pane or pipe of whoever
// started it, and herdr's startup hook returns at once. The process is
// released rather than waited for: it is meant to outlive its starter.
func ExecSpawn(exe string, args ...string) func(context.Context) (int, error) {
	return func(context.Context) (int, error) {
		// Not CommandContext: the daemon must outlive the starting command.
		// #nosec G204 -- exe is this binary's own path and the caller fixes args.
		cmd := exec.Command(exe, args...) //nolint:noctx // the daemon must outlive the starting command's context
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return 0, fmt.Errorf("start %s: %w", exe, err)
		}
		pid := cmd.Process.Pid
		if err := cmd.Process.Release(); err != nil {
			return pid, fmt.Errorf("release %s: %w", exe, err)
		}
		return pid, nil
	}
}

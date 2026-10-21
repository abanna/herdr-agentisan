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
// started it, and herdr's startup hook returns at once. The process is meant
// to outlive its starter: it is waited for only to learn when it exits
// (Spawned.Exited, A48), and a starter that exits first leaves it running,
// reparented.
func ExecSpawn(exe string, args ...string) func(context.Context) (Spawned, error) {
	return func(context.Context) (Spawned, error) {
		// Not CommandContext: the daemon must outlive the starting command.
		// #nosec G204 -- exe is this binary's own path and the caller fixes args.
		cmd := exec.Command(exe, args...) //nolint:noctx // the daemon must outlive the starting command's context
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return Spawned{}, fmt.Errorf("start %s: %w", exe, err)
		}
		exited := make(chan struct{})
		go func() {
			defer close(exited)
			_ = cmd.Wait() // that it exited is what Start needs; daemon.log says why
		}()
		return Spawned{PID: cmd.Process.Pid, Exited: exited}, nil
	}
}

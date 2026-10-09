package daemon_test

import (
	"context"
	"os/exec"
)

type execCmd = exec.Cmd

func newExecCmd(ctx context.Context, name string) *execCmd {
	return exec.CommandContext(ctx, name) //nolint:gosec // re-execs this test binary as a helper
}

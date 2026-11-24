package bundle

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// The watchdog creates the group and signals readiness before the launcher
// joins. Its only input is the CLI-owned lifeline. EOF kills the group.
const watchLifeline = "printf r; IFS= read -r ignored; kill -s KILL 0"

func startSupervisor(ctx context.Context) (*exec.Cmd, *os.File, error) {
	in, life, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create the supervisor lifeline: %w", err)
	}
	defer func() { _ = in.Close() }()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", watchLifeline)
	cmd.Cancel = nil
	cmd.Stdin = in
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ready, err := cmd.StdoutPipe()
	if err != nil {
		_ = life.Close()
		return nil, nil, fmt.Errorf("create the supervisor ready pipe: %w", err)
	}
	defer func() { _ = ready.Close() }()
	stop := context.AfterFunc(ctx, func() { _, _ = life.Close(), ready.Close() })
	defer stop()
	if err := cmd.Start(); err != nil {
		_ = life.Close()
		return nil, nil, fmt.Errorf("start the supervisor: %w", err)
	}
	var signal [1]byte
	if _, err := io.ReadFull(ready, signal[:]); err != nil {
		_ = life.Close()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("wait for the supervisor: %w", err)
	}
	return cmd, life, nil
}

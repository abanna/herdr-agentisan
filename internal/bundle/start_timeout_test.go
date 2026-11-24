package bundle

import (
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadInstalledStateTimesOutDuringStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in, out, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = in.Close(), out.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	started := make(chan error, 1)
	release := make(chan struct{})
	defer close(release)
	begin := time.Now()
	_, err = ReadInstalledState(ctx, func() (Launch, error) {
		return Launch{Argv: []string{"/bin/sleep", "10"}, Dir: dir, start: func(cmd *exec.Cmd) error {
			cmd.Stderr = out
			err := cmd.Start()
			_ = out.Close()
			started <- err
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
			return err
		}}, nil
	}, dir)
	require.ErrorIs(t, err, ErrTimeout)
	require.Less(t, time.Since(begin), time.Second)
	select {
	case startErr := <-started:
		require.NoError(t, startErr)
	default:
		t.Fatal("the launcher did not reach Start")
	}
	require.NoError(t, in.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = in.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "the group must die while Start is still blocked")
}

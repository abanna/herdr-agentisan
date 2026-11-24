package bundle

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	_, err = ReadInstalledState(ctx, func(string) (Launch, error) {
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
	}, dir, dir)
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

func TestReadInstalledStateRefusesLateLauncher(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	exited := make(chan error, 1)
	_, err := ReadInstalledState(ctx, func(string) (Launch, error) {
		l := Launch{Dir: dir, Argv: []string{"/bin/sh", "-c", "printf ran > \"$1\"", "probe", marker}}
		l.start = func(cmd *exec.Cmd) error {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			late := exec.Command(cmd.Path)
			late.Args, late.Dir, late.Env = cmd.Args, cmd.Dir, cmd.Env
			late.Stdin, late.Stdout, late.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr
			late.ExtraFiles, late.SysProcAttr = cmd.ExtraFiles, cmd.SysProcAttr
			exited <- late.Run()
			return errors.New("stub ran the late bootstrap")
		}
		return l, nil
	}, dir, dir)
	require.ErrorIs(t, err, ErrTimeout)
	select {
	case err := <-exited:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the delayed Start did not complete")
	}
	require.NoFileExists(t, marker, "the launcher ran after the timeout")
}

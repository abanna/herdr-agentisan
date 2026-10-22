//go:build unix

package cli_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/routing"
)

const readPacket = `{"task_lookup_status":"found","worker_assignment":"x"}`

// TestReadTaskPaths: --task-file reads a packet by any path the boss's shell
// would open, a FIFO once its writer writes, and refuses a path that is not
// a readable file: missing, a dangling link, a directory, unreadable or
// behind a directory it cannot search. A stdin that fails is refused too.
func TestReadTaskPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "packet.json")
	require.NoError(t, os.WriteFile(file, []byte(readPacket), 0o600))
	link, dangling, fifo := filepath.Join(dir, "link.json"), filepath.Join(dir, "dangling.json"), filepath.Join(dir, "fifo")
	require.NoError(t, os.Symlink(file, link))
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), dangling))
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	go func() { _ = os.WriteFile(fifo, []byte(readPacket), 0o600) }() //nolint:gosec // the test's own FIFO
	for _, path := range []string{file, link, fifo} {
		task, err := cli.ReadTask(strings.NewReader(""), path)
		require.NoError(t, err, path)
		assert.Equal(t, "x", task.WorkerAssignment, path)
	}
	refused := []string{filepath.Join(dir, "missing.json"), dangling, dir}
	if os.Geteuid() != 0 {
		locked, shut := filepath.Join(dir, "locked.json"), filepath.Join(dir, "shut")
		require.NoError(t, os.WriteFile(locked, []byte(readPacket), 0o000))
		require.NoError(t, os.Mkdir(shut, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(shut, "packet.json"), []byte(readPacket), 0o600))
		require.NoError(t, os.Chmod(shut, 0o000))       //nolint:gosec // unsearchable on purpose
		t.Cleanup(func() { _ = os.Chmod(shut, 0o700) }) //nolint:gosec // restored so TempDir can remove it
		refused = append(refused, locked, filepath.Join(shut, "packet.json"))
	}
	for _, path := range refused {
		_, err := cli.ReadTask(strings.NewReader(""), path)
		require.Error(t, err, path)
	}
	_, err := cli.ReadTask(iotest.ErrReader(errors.New("stdin closed")), "-")
	require.ErrorIs(t, err, routing.ErrInvalid)
}

// TestReadTaskRelativePath: a relative --task-file is resolved against the
// working directory, as the boss's shell resolves it. Not parallel: t.Chdir
// changes the whole process's directory.
func TestReadTaskRelativePath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "packet.json"), []byte(readPacket), 0o600))
	t.Chdir(dir)
	task, err := cli.ReadTask(strings.NewReader(""), "packet.json")
	require.NoError(t, err)
	assert.Equal(t, "x", task.WorkerAssignment)
}

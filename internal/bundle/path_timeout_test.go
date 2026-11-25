package bundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkDirResolutionHonorsDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := resolveWithin(ctx, 0, func() (string, error) {
		time.Sleep(2 * time.Second)
		return "/late", nil
	})
	require.ErrorIs(t, err, ErrTimeout)
	require.Less(t, time.Since(start), time.Second)
}

func TestReadInstalledStateSendsOnDiskSpelling(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	repo := filepath.Join(base, "Repo")
	dir := filepath.Join(repo, "Generations", "G001")
	alias := filepath.Join(base, "repo", "generations", "g001")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	names := strings.NewReplacer("/repo", "/Repo", "/generations", "/Generations", "/g001", "/G001")
	stat := func(path string) (os.FileInfo, error) {
		return os.Stat(names.Replace(path))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = readInstalledState(ctx, func(string) (Launch, error) {
		return Launch{Argv: []string{os.Args[0]}, Env: append(os.Environ(), "BUNDLETEST_MODE=ok", "BUNDLETEST_WORKDIR="+dir)}, nil
	}, func() (worktree, error) { return volumeWorktree(repo, alias, stat) })
	require.NoError(t, err)
	paths, err := volumeWorktree(filepath.Join(base, "repo"), alias, stat)
	require.NoError(t, err)
	require.Equal(t, worktree{repo: repo, dir: dir}, paths)
}

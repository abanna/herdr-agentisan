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
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	searchOnly := filepath.Join(base, "search-only")
	repo := filepath.Join(searchOnly, "Repo")
	aliasRepo := filepath.Join(searchOnly, "repo")
	dir := filepath.Join(repo, "Generations", "G001")
	alias := filepath.Join(aliasRepo, "generations", "g001")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(searchOnly, 0o111))
	t.Cleanup(func() { require.NoError(t, os.Chmod(searchOnly, 0o700)) })
	t.Setenv("PATH", base)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.directory")
	t.Setenv("GIT_CONFIG_VALUE_0", repo)
	git := "#!/bin/sh\n[ \"$GIT_CONFIG_COUNT:$GIT_CONFIG_KEY_0\" = 1:safe.directory ] && printf '%s\\n' \"$GIT_CONFIG_VALUE_0\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(base, "git"), []byte(git), 0o700))
	names := strings.NewReplacer("/repo", "/Repo", "/generations", "/Generations", "/g001", "/G001")
	lstat := func(path string) (os.FileInfo, error) {
		return os.Lstat(names.Replace(path))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	gitTop := func(ctx context.Context, dir string) string { return gitTopLevel(ctx, names.Replace(dir)) }
	_, err = readInstalledState(ctx, func(string) (Launch, error) {
		return Launch{Argv: []string{os.Args[0]}, Env: append(os.Environ(), "BUNDLETEST_MODE=ok", "BUNDLETEST_WORKDIR="+dir)}, nil
	}, func() (worktree, error) { return volumeWorktree(ctx, aliasRepo, dir, lstat, gitTop) })
	require.NoError(t, err)
	paths, err := volumeWorktree(ctx, aliasRepo, alias, lstat, gitTop)
	require.NoError(t, err)
	require.Equal(t, worktree{repo: repo, dir: dir}, paths)
	if os.Geteuid() != 0 {
		_, err := volumeWorktree(ctx, aliasRepo, dir, lstat, func(context.Context, string) string { return "" })
		require.ErrorContains(t, err, "cannot confirm the on-disk spelling")
	}
}

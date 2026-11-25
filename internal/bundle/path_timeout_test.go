package bundle_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bundle"
	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
)

func TestWorkDirResolutionHonorsDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := bundle.ResolveWithinString(ctx, 0, func() (string, error) {
		time.Sleep(2 * time.Second)
		return "/late", nil
	})
	require.ErrorIs(t, err, bundle.ErrTimeout)
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
	gitTop := func(ctx context.Context, dir string) string { return bundle.GitTopLevel(ctx, names.Replace(dir)) }
	_, err = bundle.ReadInstalledStateWithResolver(ctx, func(string) (bundle.Launch, error) {
		return bundletest.Launch(t, repo, "ok", bundletest.WorkDirEnv+"="+dir), nil
	}, func() (bundle.Worktree, error) { return bundle.VolumeWorktree(ctx, aliasRepo, dir, lstat, gitTop) })
	require.NoError(t, err)
	paths, err := bundle.VolumeWorktree(ctx, aliasRepo, alias, lstat, gitTop)
	require.NoError(t, err)
	resolvedRepo, resolvedDir := paths.Paths()
	require.Equal(t, repo, resolvedRepo)
	require.Equal(t, dir, resolvedDir)
	if os.Geteuid() != 0 {
		_, err := bundle.VolumeWorktree(ctx, aliasRepo, dir, lstat, func(context.Context, string) string { return "" })
		require.ErrorContains(t, err, "cannot confirm the on-disk spelling")
	}
}

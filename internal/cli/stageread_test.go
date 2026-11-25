package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bundle"
	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
	"github.com/abanna/herdr-agentisan/internal/cli"
)

func runStageRead(t *testing.T, mode string, args ...string) (string, error) {
	t.Helper()
	ctx := cli.WithBundleLocator(t.Context(), func(repo, _ string) (bundle.Launch, error) {
		return bundletest.Launch(t, repo, mode, bundletest.StageEnv+"=verify"), nil
	})
	return runBundle(ctx, append([]string{"stage", "read"}, args...)...)
}

func runBundle(ctx context.Context, args ...string) (string, error) {
	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func repoAndWorkDir(t *testing.T) (repo, dir string) {
	t.Helper()
	repo = t.TempDir()
	dir = filepath.Join(repo, "generations", "g001")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return repo, dir
}

// TestStageReadPrintsTheWorktreesStage exercises the real reply fixture.
func TestStageReadPrintsTheWorktreesStage(t *testing.T) {
	t.Parallel()
	repo, dir := repoAndWorkDir(t)

	out, err := runStageRead(t, "ok", "--repo", repo, "--work-dir", dir)
	require.NoError(t, err)
	assert.Equal(t, "stage verify · NERD-5790 · updated 2026-11-24T14:00:05Z\n", out)
}

func TestStageReadRefusesAWorkDirOutsideTheRepo(t *testing.T) {
	t.Parallel()
	repo, _ := repoAndWorkDir(t)
	for _, dir := range []string{t.TempDir(), "."} {
		_, err := runStageRead(t, "ok", "--repo", repo, "--work-dir", dir)
		assert.ErrorIs(t, err, bundle.ErrRefused, dir)
		assert.Equal(t, 1, cli.ExitCode(err))
	}
}

// TestStageReadFailsWithTheDomainsErrors exercises wrapped errors and exit status.
func TestStageReadFailsWithTheDomainsErrors(t *testing.T) {
	t.Parallel()
	repo, dir := repoAndWorkDir(t)
	_, err := runStageRead(t, "old_version", "--repo", repo, "--work-dir", dir)
	require.ErrorIs(t, err, bundle.ErrVersion)
	assert.Equal(t, 1, cli.ExitCode(err))

	_, err = runStageRead(t, "hang_call", "--repo", repo, "--work-dir", dir, "--timeout", "1s")
	assert.ErrorIs(t, err, bundle.ErrTimeout)

	out, err := runStageRead(t, "tool_error_ok", "--repo", repo, "--work-dir", dir)
	assert.ErrorIs(t, err, bundle.ErrContract)
	assert.Equal(t, 1, cli.ExitCode(err))
	assert.NotContains(t, out, "stage verify ·")

	ctx := cli.WithBundleLocator(t.Context(), func(string, string) (bundle.Launch, error) {
		return bundle.Launch{}, bundle.ErrUnavailable
	})
	_, err = runBundle(ctx, "stage", "read", "--repo", repo, "--work-dir", dir)
	assert.ErrorIs(t, err, bundle.ErrUnavailable)

	_, err = runStageRead(t, "ok", "--repo", repo)
	require.Error(t, err, "--work-dir is required")
	_, err = runStageRead(t, "ok", "--work-dir", dir, "--timeout", "0s", "--repo", repo)
	require.Error(t, err, "--timeout must be positive")
}

func TestStageReadTimesOutDuringDiscovery(t *testing.T) {
	t.Parallel()
	repo, dir := repoAndWorkDir(t)
	ctx := cli.WithBundleLocator(t.Context(), func(string, string) (bundle.Launch, error) {
		time.Sleep(2 * time.Second)
		return bundle.Launch{}, bundle.ErrUnavailable
	})
	start := time.Now()
	_, err := runBundle(ctx, "stage", "read", "--repo", repo, "--work-dir", dir, "--timeout", "50ms")
	require.ErrorIs(t, err, bundle.ErrTimeout)
	assert.Less(t, time.Since(start), time.Second)
}

func TestStageReadResolvesPathsBeforeDiscovery(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	dir := filepath.Join(repo, "g001")
	require.NoError(t, os.Mkdir(dir, 0o700))
	alias := filepath.Join(t.TempDir(), "linked")
	require.NoError(t, os.Symlink(dir, alias))
	root, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	dir, err = filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	seen := make(chan string, 1)
	ctx := cli.WithBundleLocator(t.Context(), func(path, _ string) (bundle.Launch, error) {
		seen <- path
		return bundletest.Launch(t, path, "ok", bundletest.WorkDirEnv+"="+dir), nil
	})
	_, err = runBundle(ctx, "stage", "read", "--repo", alias+"/..", "--work-dir", alias)
	require.NoError(t, err)
	require.Equal(t, root, <-seen)
}

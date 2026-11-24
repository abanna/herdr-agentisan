package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bundle"
	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
	"github.com/abanna/herdr-agentisan/internal/cli"
)

// runStageRead runs `stage read` against a stand-in server in mode.
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

// TestStageReadPrintsTheWorktreesStage: the command reads one worktree's
// stage, ticket and update time through the bundle only (NERD-5790).
func TestStageReadPrintsTheWorktreesStage(t *testing.T) {
	t.Parallel()
	repo, dir := repoAndWorkDir(t)

	out, err := runStageRead(t, "ok", "--repo", repo, "--work-dir", dir)
	require.NoError(t, err)
	assert.Equal(t, "stage verify · NERD-5790 · updated 2026-11-24T14:00:05Z\n", out)
}

// TestStageReadFailsWithTheDomainsErrors: every state of the bundle is a
// wrapped sentinel the command returns, so the process exits 1.
func TestStageReadFailsWithTheDomainsErrors(t *testing.T) {
	t.Parallel()
	repo, dir := repoAndWorkDir(t)
	_, err := runStageRead(t, "old_version", "--repo", repo, "--work-dir", dir)
	require.ErrorIs(t, err, bundle.ErrVersion)
	assert.Equal(t, 1, cli.ExitCode(err))

	_, err = runStageRead(t, "hang_call", "--repo", repo, "--work-dir", dir, "--timeout", "1s")
	assert.ErrorIs(t, err, bundle.ErrTimeout)

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

package devctl_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

// gitEnv is base with every GIT_ variable dropped, plus the two that stop git
// reading global and system config. A throwaway repo then sees only its own
// config: a developer's commit.gpgsign and signing program never reach it,
// and neither does config an outer git process passes down in the
// environment (GIT_CONFIG_PARAMETERS, GIT_CONFIG_COUNT).
func gitEnv(base []string) []string {
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
}

// gitIn runs git in dir with base as its environment, isolated by gitEnv, and
// returns what it printed.
func gitIn(t *testing.T, base []string, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv(base)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGit is gitIn with the test's own environment, failing the test if git
// fails. Every git a test runs goes through it.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := gitIn(t, os.Environ(), dir, args...)
	require.NoError(t, err, "git %v: %s", args, out)
}

// newGitRepo builds a throwaway repo so the check is exercised against real
// `git ls-files` output rather than a stubbed file list. Its signing programs
// exit 1. That does not turn signing off, which only gitEnv does; it makes a
// signing attempt fail at once rather than reach a real key or agent.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	for _, kv := range [][2]string{
		{"user.email", "t@t"},
		{"user.name", "t"},
		{"gpg.program", "false"},
		{"gpg.ssh.program", "false"},
	} {
		runGit(t, dir, "config", kv[0], kv[1])
	}
	return dir
}

func writeTracked(t *testing.T, dir, name string, size int) {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, make([]byte, size), 0o600))
	runGit(t, dir, "add", name)
}

// TestGitReposNeverSign: a throwaway repo reads no config but its own, so a
// developer whose git signs every commit (here through 1Password's
// op-ssh-sign) never has a test sign, prompt or wait on an agent. Each case
// turns signing on the way a developer's machine can, and the repo's signing
// programs exit 1, so any attempt fails the commit or the tag. The global
// config also names a hooks directory whose pre-commit refuses every commit.
func TestGitReposNeverSign(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	hooks := filepath.Join(home, "hooks")
	require.NoError(t, os.Mkdir(hooks, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o700)) //nolint:gosec // a hook must be executable
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(
		"[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n"+
			"[user]\n\tsigningkey = key::ssh-ed25519 AAAAtest\n[core]\n\thooksPath = "+hooks+"\n",
	), 0o600))
	tests := map[string][]string{
		// The developer's global config, which GIT_CONFIG_GLOBAL replaces.
		"signing in ~/.gitconfig": {"HOME=" + home},
		// Config an outer git passes down, which outranks the repo's own.
		"signing passed down in the environment": {
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=commit.gpgsign", "GIT_CONFIG_VALUE_0=true",
			"GIT_CONFIG_KEY_1=tag.gpgsign", "GIT_CONFIG_VALUE_1=true",
		},
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := newGitRepo(t)
			writeTracked(t, dir, "a.txt", 1)
			env := append(os.Environ(), extra...)

			out, err := gitIn(t, env, dir, "commit", "-qm", "a")
			require.NoError(t, err, "a test commit must never try to sign: %s", out)
			out, err = gitIn(t, env, dir, "tag", "-a", "v1", "-m", "v1")
			require.NoError(t, err, "a test tag must never try to sign: %s", out)
			out, err = gitIn(t, env, dir, "cat-file", "commit", "HEAD")
			require.NoError(t, err, out)
			assert.NotContains(t, out, "gpgsig")

			// The control: asked outright to sign, the commit fails, so the
			// one above would have failed too had git tried to sign it.
			out, err = gitIn(t, env, dir, "commit", "--allow-empty", "-S", "-qm", "signed")
			require.Error(t, err, "the repo's signing program must refuse: %s", out)
		})
	}
}

func TestFindLargeFilesEmptyRepo(t *testing.T) {
	t.Parallel()

	got, err := devctl.FindLargeFiles(t.Context(), newGitRepo(t), 512)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFindLargeFilesFlagsOversizeFile(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "big.bin", 700*1024)
	writeTracked(t, dir, "small.go", 10)

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "big.bin", got[0].Path)
	assert.Equal(t, int64(700*1024), got[0].Bytes)
}

func TestFindLargeFilesIgnoresUntracked(t *testing.T) {
	t.Parallel()

	// An untracked build artifact is not a commit risk — only tracked files
	// can be pushed, so a directory walk would produce false positives.
	dir := newGitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "untracked.bin"), make([]byte, 900*1024), 0o600))

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFindLargeFilesHonoursAllowlist(t *testing.T) {
	t.Parallel()

	// go.sum grows on its own with the tool directive; it is allowlisted.
	dir := newGitRepo(t)
	writeTracked(t, dir, "go.sum", 900*1024)

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFindLargeFilesThresholdIsInclusive(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "exactly.bin", 512*1024) // 512KB exactly

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	assert.Empty(t, got, "a file exactly at the ceiling is allowed; only over fails")
}

func TestFindLargeFilesSortsBiggestFirst(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "medium.bin", 600*1024)
	writeTracked(t, dir, "huge.bin", 900*1024)

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "huge.bin", got[0].Path, "biggest offender should be reported first")
}

func TestFindLargeFilesOutsideGitRepoErrors(t *testing.T) {
	t.Parallel()

	// A silent empty result outside a repo would make the gate vacuous.
	_, err := devctl.FindLargeFiles(t.Context(), t.TempDir(), 512)
	require.Error(t, err)
}

func TestLargeFilesCommandPassesOnThisRepo(t *testing.T) {
	out, err := runDev(t, "large-files")
	require.NoError(t, err)
	assert.Contains(t, out, "no tracked file exceeds")
}

// TestFindLargeFilesCatchesJustOverTheCeiling is the regression guard for the
// floor-division bug: 524,800 bytes is 512.5KB, which `size/1024 > 512`
// truncated to 512 and waved through. The real ceiling was 513KB-1B.
func TestFindLargeFilesCatchesJustOverTheCeiling(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "justover.bin", 512*1024+1)

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	require.Len(t, got, 1, "one byte over the ceiling must fail")
	assert.Equal(t, int64(512*1024+1), got[0].Bytes)
}

// TestFindLargeFilesAgreesWithPreCommitCeiling pins the two mechanisms
// together: check-added-large-files compares raw bytes against maxkb*1024, so
// this gate must reject exactly what that hook rejects, not a wider set.
func TestFindLargeFilesAgreesWithPreCommitCeiling(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "half-kb-over.bin", 524800) // 512.5KB

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	require.Len(t, got, 1, "pre-commit rejects 524800 bytes at --maxkb=512; so must this")
	assert.Equal(t, int64(513), got[0].KB(), "display size rounds up, never down")
}

// TestFindLargeFilesSeesIndexNotWorkingTree is the sparse-checkout case: an
// entry can be in the index while absent from disk. Stat-ing the checkout
// returned ENOENT, the entry was skipped as "tracked but deleted", and the
// gate reported clean over a blob that would still be pushed.
func TestFindLargeFilesSeesIndexNotWorkingTree(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "hidden.bin", 700*1024)
	runGit(t, dir, "commit", "-qm", "add")

	// skip-worktree plus removal from disk is exactly what a sparse checkout
	// leaves behind: still in the index, not on the filesystem.
	runGit(t, dir, "update-index", "--skip-worktree", "hidden.bin")
	require.NoError(t, os.Remove(filepath.Join(dir, "hidden.bin")))

	got, err := devctl.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	require.Len(t, got, 1, "a blob in the index must be measured even when absent from disk")
	assert.Equal(t, "hidden.bin", got[0].Path)
	assert.Equal(t, int64(700*1024), got[0].Bytes)
}

package devcli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/devcli"
)

// newGitRepo builds a throwaway repo so the check is exercised against real
// `git ls-files` output rather than a stubbed file list.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		require.NoError(t, cmd.Run(), "git %v", args)
	}
	return dir
}

func writeTracked(t *testing.T, dir, name string, size int) {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, make([]byte, size), 0o600))
	require.NoError(t, exec.Command("git", "-C", dir, "add", name).Run())
}

func TestFindLargeFilesEmptyRepo(t *testing.T) {
	t.Parallel()

	got, err := devcli.FindLargeFiles(t.Context(), newGitRepo(t), 512)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFindLargeFilesFlagsOversizeFile(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "big.bin", 700*1024)
	writeTracked(t, dir, "small.go", 10)

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
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

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFindLargeFilesHonoursAllowlist(t *testing.T) {
	t.Parallel()

	// go.sum grows on its own with the tool directive; it is allowlisted.
	dir := newGitRepo(t)
	writeTracked(t, dir, "go.sum", 900*1024)

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFindLargeFilesThresholdIsInclusive(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "exactly.bin", 512*1024) // 512KB exactly

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	assert.Empty(t, got, "a file exactly at the ceiling is allowed; only over fails")
}

func TestFindLargeFilesSortsBiggestFirst(t *testing.T) {
	t.Parallel()

	dir := newGitRepo(t)
	writeTracked(t, dir, "medium.bin", 600*1024)
	writeTracked(t, dir, "huge.bin", 900*1024)

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "huge.bin", got[0].Path, "biggest offender should be reported first")
}

func TestFindLargeFilesOutsideGitRepoErrors(t *testing.T) {
	t.Parallel()

	// A silent empty result outside a repo would make the gate vacuous.
	_, err := devcli.FindLargeFiles(t.Context(), t.TempDir(), 512)
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

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
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

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
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
	require.NoError(t, exec.Command("git", "-C", dir, "commit", "-qm", "add").Run())

	// skip-worktree plus removal from disk is exactly what a sparse checkout
	// leaves behind: still in the index, not on the filesystem.
	require.NoError(t, exec.Command("git", "-C", dir, "update-index",
		"--skip-worktree", "hidden.bin").Run())
	require.NoError(t, os.Remove(filepath.Join(dir, "hidden.bin")))

	got, err := devcli.FindLargeFiles(t.Context(), dir, 512)
	require.NoError(t, err)
	require.Len(t, got, 1, "a blob in the index must be measured even when absent from disk")
	assert.Equal(t, "hidden.bin", got[0].Path)
	assert.Equal(t, int64(700*1024), got[0].Bytes)
}

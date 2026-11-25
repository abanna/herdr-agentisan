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

func TestWorktreeContainmentOnCaseInsensitiveVolume(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repo, alias := filepath.Join(base, "Repo"), filepath.Join(base, "repo")
	dir := filepath.Join(repo, "worktree")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	stat := func(path string) (os.FileInfo, error) {
		return os.Stat(strings.Replace(path, alias, repo, 1))
	}
	require.True(t, withinRepo(alias, dir, stat))
}

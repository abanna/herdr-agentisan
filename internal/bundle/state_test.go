package bundle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bundle"
	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
)

func TestReadStateContainsAndCanonicalizesWorkDir(t *testing.T) {
	t.Parallel()
	repo, dir := workDir(t)
	sibling := repo + "-sibling"
	require.NoError(t, os.Mkdir(sibling, 0o700))
	t.Cleanup(func() { _ = os.Remove(sibling) })
	escape, inside := filepath.Join(repo, "escape"), filepath.Join(repo, "inside")
	require.NoError(t, os.Symlink(sibling, escape))
	require.NoError(t, os.Symlink(dir, inside))
	file := filepath.Join(repo, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	repoLink := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Symlink(repo, repoLink))

	real, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	s, err := open(t, repoLink, "ok", bundletest.WorkDirEnv+"="+real)
	require.NoError(t, err)
	for _, invalid := range []string{"", sibling, escape, file, filepath.Join(repo, "missing")} {
		_, err := bundle.ReadState(s, invalid)
		assert.ErrorIs(t, err, bundle.ErrRefused, invalid)
	}
	_, err = bundle.ReadState(s, inside)
	require.NoError(t, err)
}

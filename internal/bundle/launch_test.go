package bundle_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bundle"
)

// installedRepo is a repo with the bundle's marker and server entrypoint.
func installedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	server := filepath.Join(repo, ".claude", "scripts", "agentisan", "mcp")
	require.NoError(t, os.MkdirAll(server, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(server, "server.py"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".claude", "version.txt"), []byte("0.19.0\n"), 0o600))
	return repo
}

// withUV puts a uv on PATH; it is found, never run.
func withUV(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "uv"), []byte("#!/bin/sh\n"), 0o700)) //nolint:gosec // never run
	t.Setenv("PATH", bin)
	return filepath.Join(bin, "uv")
}

// TestLocateFindsTheInstalledBundle: the bundle's own template argv with
// --no-project, an absolute uv, the repo as the directory, and the caller's
// environment.
func TestLocateFindsTheInstalledBundle(t *testing.T) { //nolint:paralleltest // sets PATH and the environment
	uv := withUV(t)
	t.Setenv("AGENTISAN_STATE_HOME", "/elsewhere")
	repo := installedRepo(t)
	l, err := bundle.Locate(repo, "v1.2.3")
	require.NoError(t, err)
	assert.Equal(t, []string{uv, "run", "--no-project", "--with-editable", ".claude/scripts", "python", "-m", "agentisan.mcp.server"}, l.Argv)
	assert.Equal(t, repo, l.Dir)
	assert.Equal(t, "v1.2.3", l.ClientVersion)
	assert.Contains(t, l.Env, "AGENTISAN_STATE_HOME=/elsewhere")
}

// TestLocateSaysWhyTheBundleIsUnavailable: each missing piece is
// ErrUnavailable; a repo that is not an absolute path is a refused call.
func TestLocateSaysWhyTheBundleIsUnavailable(t *testing.T) { //nolint:paralleltest // sets PATH
	withUV(t)
	repo := installedRepo(t)
	tests := map[string]func(t *testing.T) string{
		"no repo": func(*testing.T) string { return filepath.Join(repo, "missing") },
		"no marker": func(t *testing.T) string {
			r := installedRepo(t)
			require.NoError(t, os.Remove(filepath.Join(r, ".claude", "version.txt")))
			return r
		},
		"a marker dir": func(t *testing.T) string {
			r := installedRepo(t)
			p := filepath.Join(r, ".claude", "version.txt")
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Mkdir(p, 0o700))
			return r
		},
		"no server file": func(t *testing.T) string {
			r := installedRepo(t)
			require.NoError(t, os.Remove(filepath.Join(r, ".claude", "scripts", "agentisan", "mcp", "server.py")))
			return r
		},
	}
	for name, mk := range tests {
		_, err := bundle.Locate(mk(t), "x")
		assert.ErrorIs(t, err, bundle.ErrUnavailable, name)
	}
	_, err := bundle.Locate("relative/repo", "x")
	assert.ErrorIs(t, err, bundle.ErrRefused)

	t.Setenv("PATH", t.TempDir())
	_, err = bundle.Locate(repo, "x")
	assert.ErrorIs(t, err, bundle.ErrUnavailable, "no uv")
}

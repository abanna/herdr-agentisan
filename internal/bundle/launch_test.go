package bundle_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// withUV puts a stub uv on PATH.
func withUV(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "uv"), []byte("#!/bin/sh\n"), 0o700)) //nolint:gosec // never run
	t.Setenv("PATH", bin)
	return filepath.Join(bin, "uv")
}

// TestLocateFindsTheInstalledBundle pins the launch and environment.
func TestLocateFindsTheInstalledBundle(t *testing.T) { //nolint:paralleltest // sets PATH and the environment
	uv := withUV(t)
	for _, key := range []string{"AGENTISAN_STATE_HOME", "AGENTISAN_GOVERNED_READ_ONLY", "UV_OFFLINE", "UV_NO_PROGRESS"} {
		t.Setenv(key, "keep")
	}
	drop := []string{
		"UV", "UV_WORKING_DIR", "UV_PROJECT", "UV_PROJECT_ENVIRONMENT", "UV_CONFIG_FILE", "UV_ENV_FILE",
		"UV_INDEX", "UV_PYTHON", "UV_FUTURE_SELECTOR", "VIRTUAL_ENV", "CONDA_PREFIX", "_CONDA_ROOT",
		"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONUSERBASE", "PYTHONSAFEPATH", "__PYVENV_LAUNCHER__",
		"_PYTHON_SYSCONFIGDATA_NAME", "PYENV_VERSION", "PIP_INDEX_URL", "LD_PRELOAD", "DYLD_LIBRARY_PATH",
		"XDG_BIN_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_DIRS", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
		"AGENTISAN_AGENT_SPAWN", "AGENTISAN_EXTERNAL_WRITES", "AGENTISAN_RUN_ID", "BASH_ENV", "ENV",
	}
	for _, key := range drop {
		t.Setenv(key, "drop")
	}
	repo := installedRepo(t)
	l, err := bundle.Locate(repo, "v1.2.3")
	require.NoError(t, err)
	assert.Equal(t, []string{
		uv, "--directory", repo, "--no-config", "run", "--no-project", "--isolated", "--no-env-file",
		"--with-editable", filepath.Join(repo, ".claude", "scripts"),
		"--", "python", "-I", "-m", "agentisan.mcp.server",
	}, l.Argv)
	assert.Equal(t, repo, l.Dir)
	assert.Equal(t, "v1.2.3", l.ClientVersion)
	for _, key := range []string{"AGENTISAN_STATE_HOME", "AGENTISAN_GOVERNED_READ_ONLY", "UV_OFFLINE", "UV_NO_PROGRESS"} {
		assert.Contains(t, l.Env, key+"=keep")
	}
	for _, entry := range l.Env {
		key, _, _ := strings.Cut(entry, "=")
		assert.NotContains(t, drop, key)
	}
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

func TestLocatePinsUVToTheRequestedRepo(t *testing.T) {
	uv := withUV(t)
	repo, other := installedRepo(t), installedRepo(t)
	t.Setenv("UV_WORKING_DIR", other)
	const standIn = `#!/bin/sh
directory=${UV_WORKING_DIR:-$PWD}
while [ "$#" -gt 0 ]; do
	case "$1" in
		--directory) directory=$2; shift ;;
		--with-editable) editable=$2; shift ;;
	esac
	shift
done
case "$editable" in
	/*) ;;
	*) editable=$directory/$editable ;;
esac
printf '%s\n%s\n' "$directory" "$editable"
`
	require.NoError(t, os.WriteFile(uv, []byte(standIn), 0o700))
	launch, err := bundle.Locate(repo, "test")
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), launch.Argv[0], launch.Argv[1:]...)
	cmd.Dir, cmd.Env = launch.Dir, launch.Env
	out, err := cmd.Output()
	require.NoError(t, err)
	assert.Equal(t, repo+"\n"+filepath.Join(repo, ".claude", "scripts")+"\n", string(out))
}

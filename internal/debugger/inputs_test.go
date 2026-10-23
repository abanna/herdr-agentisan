package debugger_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

// TestRoot: the plugin's checkout is HERDR_PLUGIN_ROOT when herdr set it,
// else the checkout the binary was built into.
func TestRoot(t *testing.T) {
	t.Parallel()
	bin := "/opt/agentisan/bin/herdr-agentisan"
	assert.Equal(t, "/srv/plugin", debugger.Root(env(map[string]string{debugger.RootEnv: "/srv/plugin"}), bin))
	assert.Equal(t, "/opt/agentisan", debugger.Root(env(nil), bin))
	assert.Equal(t, "/opt/agentisan", debugger.Root(env(map[string]string{debugger.RootEnv: ""}), bin), "set but empty is unset")
}

// TestCheckRoot: a root with no role under it is refused, naming the root,
// and so is one whose role is no regular file: a directory, a FIFO claude
// would wait on, or a dangling link. A link to a regular role is one.
func TestCheckRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	err := debugger.CheckRoot(root)
	require.ErrorIs(t, err, debugger.ErrInvalid)
	assert.Contains(t, err.Error(), root)
	role := filepath.Join(root, debugger.RoleFile)
	require.NoError(t, os.MkdirAll(role, 0o700))
	require.ErrorIs(t, debugger.CheckRoot(root), debugger.ErrInvalid, "a directory")
	require.NoError(t, os.Remove(role))
	require.NoError(t, syscall.Mkfifo(role, 0o600))
	require.ErrorIs(t, debugger.CheckRoot(root), debugger.ErrInvalid, "a FIFO")
	require.NoError(t, os.Remove(role))
	require.NoError(t, os.Symlink(filepath.Join(root, "gone.md"), role))
	require.ErrorIs(t, debugger.CheckRoot(root), debugger.ErrInvalid, "a dangling link")
	require.NoError(t, os.WriteFile(filepath.Join(root, "gone.md"), []byte("# role\n"), 0o600))
	require.NoError(t, debugger.CheckRoot(root), "a link to a regular file")
	require.NoError(t, os.Remove(role))
	require.NoError(t, os.WriteFile(role, []byte("# role\n"), 0o600))
	require.NoError(t, debugger.CheckRoot(root))
}

// TestKeyFiles: the env file the shared config names, under ~/ through
// HOME, and its target too when it is a link; no env file, or a config
// that does not resolve, is refused. The file itself is never needed.
func TestKeyFiles(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+body), 0o600))
		return dir
	}
	target := filepath.Join(home, "elastic.env")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	link := filepath.Join(home, "link.env")
	require.NoError(t, os.Symlink(target, link))
	lookup := env(map[string]string{"HOME": home})

	got, err := debugger.KeyFiles(write(t, "[debugger]\nenv_file = \"~/elastic.env\"\n"), lookup)
	require.NoError(t, err)
	assert.Equal(t, []string{target}, got)

	got, err = debugger.KeyFiles(write(t, "[debugger]\nenv_file = \""+link+"\"\n"), lookup)
	require.NoError(t, err)
	assert.Equal(t, []string{link, target}, got, "a link and its target")

	missing := filepath.Join(home, "missing.env")
	got, err = debugger.KeyFiles(write(t, "[debugger]\nenv_file = \""+missing+"\"\n"), lookup)
	require.NoError(t, err)
	assert.Equal(t, []string{missing}, got, "never opened, so it need not exist yet")

	none := write(t, "")
	_, err = debugger.KeyFiles(none, lookup)
	require.ErrorIs(t, err, debugger.ErrInvalid)
	assert.Contains(t, err.Error(), "set [debugger] env_file in "+filepath.Join(none, settings.FileName))

	_, err = debugger.KeyFiles(write(t, "[debugger]\nenv_file = 7\n"), lookup)
	require.ErrorIs(t, err, debugger.ErrInvalid)
	require.ErrorIs(t, err, settings.ErrInvalid, "the config's own error stays in the chain")
}

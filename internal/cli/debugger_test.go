package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// roleFile is the role as it ships in the plugin checkout.
var roleFile = filepath.Join("..", "..", debugger.RoleFile)

// launchDirs makes a plugin root holding the role, a repo, a server dir and
// a config dir whose shared file names keyFile(config) as the env file.
func launchDirs(t *testing.T) (root, repo, server, config string) {
	t.Helper()
	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "roles"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, debugger.RoleFile), []byte("# role\n"), 0o600))
	config = t.TempDir()
	writeEnvFileSetting(t, config, keyFile(config))
	return root, t.TempDir(), t.TempDir(), config
}

// keyFile is the env file launchDirs' config names; it is never created.
func keyFile(config string) string { return filepath.Join(config, "elastic.env") }

// writeEnvFileSetting writes a shared file in config naming envFile.
func writeEnvFileSetting(t *testing.T, config, envFile string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(config, settings.FileName),
		[]byte("schema_version = 1\n[debugger]\nenv_file = \""+envFile+"\"\n"), 0o600))
}

// TestDebuggerArgsPrintsTheLaunch: `debugger args` prints the launch the
// workforce will build, for this binary: as JSON, and as one shell line
// whose quoting a shell reads back into exactly that argv.
func TestDebuggerArgsPrintsTheLaunch(t *testing.T) {
	t.Parallel()
	root, repo, server, config := launchDirs(t)
	bin, err := os.Executable()
	require.NoError(t, err)
	want, err := debugger.Launch{Root: root, Bin: bin, Repo: repo, ServerDir: server, ConfigDir: config, KeyFiles: []string{keyFile(config)}}.Command()
	require.NoError(t, err)
	ctx := envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config})

	out, err := runIn(ctx, "debugger", "args", "--root", root, "--repo", repo, "--server-dir", server, "--json")
	require.NoError(t, err)
	var got struct {
		Env  map[string]string `json:"env"`
		Argv []string          `json:"argv"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, append([]string{"claude"}, want.Args...), got.Argv)
	assert.Equal(t, want.Env, got.Env)

	out, err = runIn(ctx, "debugger", "args", "--root", root, "--repo", repo, "--server-dir", server)
	require.NoError(t, err)
	line := strings.TrimSuffix(out, "\n")
	prefix := "HERDR_PLUGIN_CONFIG_DIR=" + config + " "
	require.True(t, strings.HasPrefix(line, prefix), line)
	words, err := exec.CommandContext(t.Context(), "sh", "-c", `eval "set -- $1"; for a in "$@"; do printf '%s\n' "$a"; done`,
		"sh", strings.TrimPrefix(line, prefix)).Output()
	require.NoError(t, err)
	assert.Equal(t, got.Argv, strings.Split(strings.TrimSuffix(string(words), "\n"), "\n"), "the shell reads back the argv")
}

// TestDebuggerArgsNeedsItsInputs: no role under the root, or no config dir,
// is refused with what to pass; HERDR_PLUGIN_ROOT is the root when herdr
// set it.
func TestDebuggerArgsNeedsItsInputs(t *testing.T) {
	t.Parallel()
	root, repo, _, config := launchDirs(t)

	_, err := runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config}), "debugger", "args", "--root", t.TempDir(), "--repo", repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pass --root")

	_, err = runIn(envCtx(t, map[string]string{}), "debugger", "args", "--root", root, "--repo", repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pass --config-dir")

	_, err = runIn(envCtx(t, map[string]string{}), "debugger", "args", "--root", root, "--repo", repo, "--config-dir", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set [debugger] env_file", "no env file, no launch: the deny list needs it")

	out, err := runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_ROOT": root}), "debugger", "args", "--repo", repo, "--config-dir", config, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, filepath.Join(root, debugger.RoleFile))

	// With neither, the root is the checkout the binary sits in: the test
	// binary's has no role.
	_, err = runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config}), "debugger", "args", "--repo", repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pass --root")
}

// TestDebuggerArgsPathForms: a relative --repo is the repo under the
// working directory, made absolute, and a --root reached through a symlink
// is used as given; a --repo checkPath refuses fails before anything is
// printed. Not parallel: t.Chdir changes the process's directory.
func TestDebuggerArgsPathForms(t *testing.T) {
	root, _, _, config := launchDirs(t)
	cwd := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(cwd, "assay"), 0o700))
	link := filepath.Join(t.TempDir(), "plugin")
	require.NoError(t, os.Symlink(root, link))
	t.Chdir(cwd)
	ctx := envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config})

	out, err := runIn(ctx, "debugger", "args", "--root", link, "--repo", "assay", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "Read(/"+filepath.Join(cwd, "assay")+"/**)")
	assert.Contains(t, out, filepath.Join(link, debugger.RoleFile))

	out, err = runIn(ctx, "debugger", "args", "--root", root, "--repo", "my repo", "--json")
	require.ErrorIs(t, err, debugger.ErrInvalid)
	assert.NotContains(t, out, "claude")

	// A repo holding the env file is refused; an env file that is a link is
	// denied by its name and its target's.
	_, err = runIn(ctx, "debugger", "args", "--root", root, "--repo", config, "--json")
	require.ErrorIs(t, err, debugger.ErrInvalid)
	target := filepath.Join(t.TempDir(), "local.env")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	linked := t.TempDir()
	require.NoError(t, os.Symlink(target, keyFile(linked)))
	writeEnvFileSetting(t, linked, keyFile(linked))
	out, err = runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": linked}), "debugger", "args", "--root", root, "--repo", "assay", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "Read(/"+keyFile(linked)+")")
	assert.Contains(t, out, "Read(/"+target+")")
}

// TestRoleMatchesTheLaunch: the role file, the CLI and the launch say the
// same thing. Every command the role tells the debugger to run is a real
// command, with real flags, inside the launch's Bash allow rule; every
// field it names is one the trail carries (telemetry's attribute
// constants, or the resource's version and commit).
func TestRoleMatchesTheLaunch(t *testing.T) {
	t.Parallel()
	role, err := os.ReadFile(roleFile)
	require.NoError(t, err)
	text := string(role)

	l := debugger.Launch{Root: "/r", Bin: "/r/bin/herdr-agentisan", Repo: "/repo", ConfigDir: "/c", KeyFiles: []string{"/c/local.env"}}
	launch, err := l.Command()
	require.NoError(t, err)
	var rule string
	for _, a := range launch.Args {
		if p, ok := strings.CutPrefix(a, "Bash("); ok {
			rule = strings.TrimSuffix(p, ":*)")
		}
	}
	require.Equal(t, l.Bin+" events", rule, "the one Bash rule")

	// Every `<bin> ...` the role names, whatever it runs, must sit inside
	// the one Bash rule and be a real command with real flags.
	commands := regexp.MustCompile("`<bin> ([^`]*)`").FindAllStringSubmatch(text, -1)
	require.NotEmpty(t, commands)
	for _, m := range commands {
		require.True(t, strings.HasPrefix(l.Bin+" "+m[1], rule+" "), "%s is inside the allow rule", m[0])
		words := strings.Fields(m[1])
		require.GreaterOrEqual(t, len(words), 2, m[0])
		c, rest, err := cli.Root().Find(words[:2])
		require.NoError(t, err, m[0])
		require.Empty(t, rest, m[0])
		assert.Equal(t, words[1], c.Name(), m[0])
		for _, f := range regexp.MustCompile(`--([a-z-]+)`).FindAllStringSubmatch(m[1], -1) {
			assert.NotNil(t, c.Flags().Lookup(f[1]), "%s: --%s", m[0], f[1])
		}
	}

	attrs := []string{
		telemetry.AttrWorkerID, telemetry.AttrWorkerBound, telemetry.AttrProject, telemetry.AttrGroup,
		telemetry.AttrRole, telemetry.AttrAgent, telemetry.AttrPaneID, telemetry.AttrFrom, telemetry.AttrTo,
	}
	resource := []string{"service.version", "service.commit"}
	fields := regexp.MustCompile(`(resource\.)?attributes\.([a-z_.]+[a-z])`).FindAllStringSubmatch(text, -1)
	require.NotEmpty(t, fields)
	for _, f := range fields {
		if f[1] != "" {
			assert.Contains(t, resource, f[2], f[0])
			continue
		}
		assert.Contains(t, attrs, f[2], "%s is a field the trail carries", f[0])
	}
	assert.Contains(t, text, telemetry.EventStatusChanged)
	assert.True(t, slices.ContainsFunc(launch.Args, func(a string) bool { return strings.HasSuffix(a, debugger.RoleFile) }))
}

package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
// that a shell runs as exactly that launch: in the repo, with that
// environment and that argv. A stand-in claude on PATH reports all three.
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
		Dir  string            `json:"dir"`
		Env  map[string]string `json:"env"`
		Argv []string          `json:"argv"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, repo, got.Dir)
	assert.Equal(t, append([]string{"claude"}, want.Args...), got.Argv)
	assert.Equal(t, want.Env, got.Env)

	out, err = runIn(ctx, "debugger", "args", "--root", root, "--repo", repo, "--server-dir", server)
	require.NoError(t, err)
	ran, err := runLine(t, out)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(ran), "\n"), "\n")
	require.Greater(t, len(lines), 2, string(ran))
	assert.Equal(t, repo, lines[0], "the line starts claude in the repo, wherever it is run from")
	assert.Equal(t, config, lines[1], "with the config dir in its environment")
	assert.Equal(t, want.Args, lines[2:], "the shell reads back the argv")
}

// TestDebuggerArgsFailedCdNeverStartsClaude: the line joins the cd and
// claude with &&, so a repo that is gone by the time the line runs fails the
// shell before claude starts. With a ; claude would start in the caller's
// directory, which the fence would then admit whole.
func TestDebuggerArgsFailedCdNeverStartsClaude(t *testing.T) {
	t.Parallel()
	root, repo, server, config := launchDirs(t)
	out, err := runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config}), "debugger", "args", "--root", root, "--repo", repo, "--server-dir", server)
	require.NoError(t, err)
	require.NoError(t, os.Remove(repo))

	ran, err := runLine(t, out)
	require.Error(t, err, "the cd fails, and so does the line")
	assert.Empty(t, string(ran), "claude never ran")
}

// runLine runs a printed launch line through sh, from a directory of its
// own, with a stand-in claude first on PATH that prints its working
// directory, HERDR_PLUGIN_CONFIG_DIR and each argument, one per line.
func runLine(t *testing.T, line string) ([]byte, error) {
	t.Helper()
	stub := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stub, "claude"),
		[]byte("#!/bin/sh\npwd\nprintf '%s\\n' \"$HERDR_PLUGIN_CONFIG_DIR\"\nfor a; do printf '%s\\n' \"$a\"; done\n"), 0o700)) //nolint:gosec // an executable stand-in
	sh := exec.CommandContext(t.Context(), "sh", "-c", line)
	sh.Dir = t.TempDir()
	sh.Env = []string{"PATH=" + stub + ":/usr/bin:/bin"}
	return sh.Output()
}

// TestDebuggerArgsNeedsItsInputs: no role under the root, no config dir, or
// a server dir that is not a directory (claude's --add-dir would refuse it)
// is refused with what to pass; HERDR_PLUGIN_ROOT is the root when herdr
// set it.
func TestDebuggerArgsNeedsItsInputs(t *testing.T) {
	t.Parallel()
	root, repo, _, config := launchDirs(t)

	notDir := filepath.Join(t.TempDir(), "daemon.log")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))
	for name, server := range map[string]string{"missing": filepath.Join(t.TempDir(), "srv-gone"), "a file": notDir} {
		_, err := runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config}), "debugger", "args", "--root", root, "--repo", repo, "--server-dir", server)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "is not a directory; pass the daemon's", name)
	}

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

	// With neither, the root is the checkout the binary sits in,
	// <root>/bin/herdr-agentisan, two levels up from the binary and not its
	// own directory: the test binary's has no role, as an installed copy's
	// has none, and the error says so.
	exe, err := os.Executable()
	require.NoError(t, err)
	_, err = runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config}), "debugger", "args", "--repo", repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("under %q", filepath.Dir(filepath.Dir(exe))))
	assert.Contains(t, err.Error(), "it is the checkout around this binary, "+exe+", and an installed copy (task install, go install) sits in none: pass --root")
	assert.Contains(t, err.Error(), "or set HERDR_PLUGIN_ROOT")

	// A HERDR_PLUGIN_ROOT with no role is named as the source.
	_, err = runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config, "HERDR_PLUGIN_ROOT": t.TempDir()}), "debugger", "args", "--repo", repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "it is HERDR_PLUGIN_ROOT's; pass --root")
}

// TestDebuggerArgsOddPathsRunTheLaunch: the printed line quotes every path
// checkPath accepts, shell metacharacters and expansions among them, so a
// shell runs it as exactly the launch: in the repo, with the config dir in
// its environment and the launch's argv. Each name is in the root, the
// repo, the server dir and the config dir at once, and so in the env file's
// deny rule too. A backtick, in any of them, is refused before anything is
// printed.
func TestDebuggerArgsOddPathsRunTheLaunch(t *testing.T) {
	t.Parallel()
	bin, err := os.Executable()
	require.NoError(t, err)
	plain := regexp.MustCompile(`^[A-Za-z0-9_./:=@%+-]+$`)
	for _, name := range []string{
		"a$b", "a;b", "a&b", "a|b", "a<b>c", "a#b", "~a", "a!b", "$PWD", "a$HOME",
		// A tilde after a colon, which an assignment expands: the env value.
		"a:~root",
		// shellQuote leaves these bare, but for the non-ASCII and the ^.
		"a=b", "a%b@c+d^e:f", "kódér", "-x",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, repo, server, config := oddDirs(t, name)
			want, err := debugger.Launch{Root: root, Bin: bin, Repo: repo, ServerDir: server, ConfigDir: config, KeyFiles: []string{keyFile(config)}}.Command()
			require.NoError(t, err)
			require.Contains(t, want.Args, server, "the server dir is in the argv")
			ctx := envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config})

			out, err := runIn(ctx, "debugger", "args", "--root", root, "--repo", repo, "--server-dir", server, "--json")
			require.NoError(t, err)
			var got struct {
				Dir  string            `json:"dir"`
				Env  map[string]string `json:"env"`
				Argv []string          `json:"argv"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			assert.Equal(t, repo, got.Dir)
			assert.Equal(t, want.Env, got.Env)
			assert.Equal(t, append([]string{"claude"}, want.Args...), got.Argv)

			line, err := runIn(ctx, "debugger", "args", "--root", root, "--repo", repo, "--server-dir", server)
			require.NoError(t, err)
			ran, err := runLine(t, line)
			require.NoError(t, err, line)
			lines := strings.Split(strings.TrimSuffix(string(ran), "\n"), "\n")
			require.Greater(t, len(lines), 2, string(ran))
			assert.Equal(t, repo, lines[0], "claude starts in the repo")
			assert.Equal(t, config, lines[1], "with the config dir in its environment")
			assert.Equal(t, want.Args, lines[2:], "and the launch's argv")
			// Any other character is quoted, though sh -c runs it bare: the line
			// is pasted into an interactive zsh or bash, which expands a bare !,
			// and a # or ^ under EXTENDED_GLOB (QA #76, S1).
			words := " " + strings.TrimSpace(line) + " "
			for _, w := range []struct{ before, path string }{{" cd ", repo}, {" HERDR_PLUGIN_CONFIG_DIR=", config}, {" ", server}} {
				if !plain.MatchString(w.path) {
					assert.Contains(t, words, w.before+"'"+w.path+"' ", "quoted on the line")
				}
			}
		})
	}

	for _, which := range []string{"root", "repo", "server dir", "config dir"} {
		t.Run("a backtick in the "+which, func(t *testing.T) {
			t.Parallel()
			root, repo, server, config := launchDirs(t)
			odd, _, _, _ := oddDirs(t, "a`b")
			switch which {
			case "root":
				root = odd
			case "repo":
				repo = odd
			case "server dir":
				server = odd
			case "config dir":
				config = odd
				writeEnvFileSetting(t, config, keyFile(config))
			}
			out, err := runIn(envCtx(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": config}),
				"debugger", "args", "--root", root, "--repo", repo, "--server-dir", server)
			require.ErrorIs(t, err, debugger.ErrInvalid)
			assert.ErrorContains(t, err, "the "+which+" holds '`'")
			assert.Empty(t, out, "nothing is printed")
		})
	}
}

// oddDirs makes a plugin root holding the role, a repo, a server dir and a
// config dir whose shared file names keyFile(config), each named with name
// in it.
func oddDirs(t *testing.T, name string) (root, repo, server, config string) {
	t.Helper()
	base := t.TempDir()
	root, repo, server, config = filepath.Join(base, "root"+name), filepath.Join(base, "repo"+name),
		filepath.Join(base, "srv"+name), filepath.Join(base, "cfg"+name)
	for _, d := range []string{filepath.Join(root, "roles"), repo, server, config} {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, debugger.RoleFile), []byte("# role\n"), 0o600))
	writeEnvFileSetting(t, config, keyFile(config))
	return root, repo, server, config
}

// TestRoleKeepsItsGuards: the role's sentences that tell the debugger what
// it must never do are in it as written, whatever its line breaks. No rule
// of the launch can carry them.
func TestRoleKeepsItsGuards(t *testing.T) {
	t.Parallel()
	role, err := os.ReadFile(roleFile)
	require.NoError(t, err)
	text := strings.Join(strings.Fields(string(role)), " ")
	for _, guard := range []string{
		"You change nothing",
		"Any other command is denied without asking",
		"Your reads are fenced to the repo and the daemon's directory",
		"Never ask for a credential, never try to read one",
		"never work around a denial",
		"What you pass to a Linear tool leaves the machine",
		"never text you read from the trail, the repo or the daemon's log",
	} {
		assert.Contains(t, text, guard)
	}
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

// TestDaemonRunKnowsThePluginCheckout: `daemon run` gives the workforce the
// plugin's checkout and binary, which a debugger worker's launch names
// (A32): the checkout herdr set, else the one the binary sits in.
func TestDaemonRunKnowsThePluginCheckout(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	root := t.TempDir()
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"herdr's":          {env: map[string]string{"HERDR_PLUGIN_ROOT": root}, want: root},
		"the binary's own": {want: filepath.Dir(filepath.Dir(exe))},
	} {
		cmd := &cobra.Command{}
		cmd.SetContext(cli.WithLookupEnv(t.Context(), e.lookup(tc.env)))
		o, err := cli.DaemonOptions(cmd, "", false)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, o.Team.PluginRoot, name)
		assert.Equal(t, exe, o.Team.Binary, name)
	}
}

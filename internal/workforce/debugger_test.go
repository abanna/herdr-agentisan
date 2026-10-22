package workforce_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/trail"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// debuggerTeam is teamConfig with a debugger group (and group, more of its
// table), the env file the debugger's events read (envLine: "" for none)
// and a top-level command whose flag must never reach the debugger.
func debuggerTeam(envLine, group string) string {
	return "schema_version = 1\n[agent]\ncommand = [\"claude\", \"--permission-mode\", \"acceptEdits\"]\nmodel = \"opus\"\n" + envLine +
		strings.TrimPrefix(teamConfig, "schema_version = 1\n") +
		"\n[projects.herdr-agentisan.groups.debugger]\nworkers = 0\nslots = 1\n" + group
}

// newDebuggerFixture is a fixture whose daemon was given a plugin checkout
// with the role, its binary and its directory; the debugger's env file is
// envFile, outside the repo, and group is more of its group's table.
func newDebuggerFixture(t *testing.T, group string) (f *fixture, envFile string) {
	t.Helper()
	envFile = filepath.Join(t.TempDir(), "elastic.env")
	f = newFixture(t, debuggerTeam("[debugger]\nenv_file = \""+envFile+"\"\n", group))
	f.svc.Debugger = pluginLaunch(t)
	return f, envFile
}

// pluginLaunch is a plugin checkout holding the role, its binary and a
// daemon directory.
func pluginLaunch(t *testing.T) debugger.Launch {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "roles"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, debugger.RoleFile), []byte("# debugger\n"), 0o600))
	return debugger.Launch{Root: root, Bin: filepath.Join(root, "bin", "herdr-agentisan"), ServerDir: t.TempDir()}
}

// TestSpawnLaunchesTheDebugger: a worker of the debugger type runs the
// plugin's read-only launch (A32), the one `debugger args` builds from the
// same inputs, then the workforce's --model and --name: its tab starts in
// the repo with the config dir in its shell's environment beside its id,
// and the top-level command's flags never reach it. It is a worker like any
// other: its row is bound in the debugger group, and team status lists it
// there.
func TestSpawnLaunchesTheDebugger(t *testing.T) {
	t.Parallel()
	f, envFile := newDebuggerFixture(t, "")

	res, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: settings.DebuggerType, Count: 1})
	require.NoError(t, err)

	l := f.svc.Debugger
	l.Repo, l.ConfigDir, l.KeyFiles = f.repo, f.config, []string{envFile}
	want, err := l.Command()
	require.NoError(t, err)
	tabs := f.h.params("tab.create")
	require.Len(t, tabs, 1)
	assert.Equal(t, map[string]any{
		"workspace_id": f.ws, "cwd": want.Dir, "label": "ha-debugger-1", "focus": false,
		"env": map[string]any{workforce.WorkerEnv: "ha-debugger-1", trail.ConfigDirEnv: f.config},
	}, tabs[0])
	starts := f.h.params("agent.start")
	require.Len(t, starts, 1)
	assert.Equal(t, "claude", starts[0]["kind"])
	args := make([]any, 0, len(want.Args)+4)
	for _, a := range append(slices.Clone(want.Args), "--model", "opus", "--name", "ha-debugger-1") {
		args = append(args, a)
	}
	assert.Equal(t, args, starts[0]["args"])
	assert.NotContains(t, starts[0]["args"], "acceptEdits", "the top-level command never applies to the debugger")

	require.Len(t, res.Workers, 1)
	assert.Equal(t, workforce.Spawned{ID: "ha-debugger-1", Group: settings.DebuggerType, PaneID: res.Workers[0].PaneID, Agent: "claude"}, res.Workers[0])
	assert.Equal(t, store.StatusActive, f.row(t, "ha-debugger-1").Status)
	assert.Equal(t, settings.DebuggerType, f.row(t, "ha-debugger-1").Group)

	st, err := f.svc.Status(t.Context(), workforce.StatusRequest{})
	require.NoError(t, err)
	i := slices.IndexFunc(st.Groups, func(g workforce.GroupStatus) bool { return g.Name == settings.DebuggerType })
	require.GreaterOrEqual(t, i, 0, "the debugger group is listed")
	require.Len(t, st.Groups[i].Members, 1)
	assert.Equal(t, "ha-debugger-1", st.Groups[i].Members[0].ID)
}

// TestSpawnTheDebuggerWithAModel: --model, else the group's model, else the
// top-level one, as for any type.
func TestSpawnTheDebuggerWithAModel(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		group, flag, want string
	}{
		"the flag":          {flag: "sonnet", want: "sonnet"},
		"the group's model": {group: "agent = { model = \"haiku\" }\n", want: "haiku"},
		"the flag first":    {group: "agent = { model = \"haiku\" }\n", flag: "sonnet", want: "sonnet"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, _ := newDebuggerFixture(t, tc.group)
			_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: settings.DebuggerType, Count: 1, Model: tc.flag})
			require.NoError(t, err)
			args := f.h.params("agent.start")[0]["args"].([]any)
			assert.Equal(t, []any{"--model", tc.want, "--name", "ha-debugger-1"}, args[len(args)-4:])
		})
	}
}

// debuggerEnvFile names the debugger's env file, outside any repo; a launch
// never opens it.
const debuggerEnvFile = "\n[debugger]\nenv_file = \"/nonexistent/elastic.env\"\n"

// TestSpawnRoutesTheDebugger: with routing on, the debugger starts on what
// its role, alex_debugger, chose (A50), after its own launch's arguments,
// and its row records the decision.
func TestSpawnRoutesTheDebugger(t *testing.T) {
	t.Parallel()
	f, rec := routedFixture(t, strings.Replace(routingOn, opsGroup, "\n[projects.herdr-agentisan.groups.debugger]\nworkers = 0\nslots = 1\n", 1)+debuggerEnvFile)
	f.svc.Debugger = pluginLaunch(t)

	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: settings.DebuggerType, Count: 1})
	require.NoError(t, err)
	l := f.svc.Debugger
	l.Repo, l.ConfigDir, l.KeyFiles = f.repo, f.config, []string{"/nonexistent/elastic.env"}
	want, err := l.Command()
	require.NoError(t, err)
	args := make([]any, 0, len(want.Args)+6)
	for _, a := range append(slices.Clone(want.Args), "--model", "claude-opus-5-5", "--effort", "high", "--name", "ha-debugger-1") {
		args = append(args, a)
	}
	starts := f.h.params("agent.start")
	require.Len(t, starts, 1)
	assert.Equal(t, "claude", starts[0]["kind"])
	assert.Equal(t, args, starts[0]["args"])
	require.Len(t, rec.all(), 1)
	r := routingOf(t, f, "ha-debugger-1")
	require.NotNil(t, r)
	assert.Equal(t, "alex_debugger", r.Role)
	assert.Equal(t, "claude_strong", r.Binding)
}

// TestSpawnRefusesADebuggerItCannotLaunch: a launch that cannot be built is
// refused whole as a configuration error, before the preflight, a tab or a
// row: no env file named, a daemon given no checkout, a checkout with no
// role, an env file in the repo, which its fence would admit, and an MCP
// config it cannot read. A debugger with no group is an unknown group,
// whose launch is never built.
func TestSpawnRefusesADebuggerItCannotLaunch(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		envLine string
		launch  func(*testing.T, *fixture) debugger.Launch
		repo    func(*testing.T, string)
		want    string
	}{
		"no_env_file": {want: "set [debugger] env_file"},
		"no_checkout": {envLine: "[debugger]\nenv_file = \"/nonexistent/elastic.env\"\n", launch: func(*testing.T, *fixture) debugger.Launch { return debugger.Launch{} }, want: "not given the plugin's checkout"},
		"no_role": {envLine: "[debugger]\nenv_file = \"/nonexistent/elastic.env\"\n", launch: func(t *testing.T, _ *fixture) debugger.Launch {
			return debugger.Launch{Root: t.TempDir(), Bin: "/opt/bin/herdr-agentisan"}
		}, want: "no " + debugger.RoleFile + " under"},
		"env_file_in_the_repo": {envLine: "[debugger]\nenv_file = \"REPO/.config/elastic.env\"\n", want: "holds the trail's env file"},
		"an_unreadable_mcp_config": {
			envLine: "[debugger]\nenv_file = \"/nonexistent/elastic.env\"\n",
			repo:    func(t *testing.T, repo string) { require.NoError(t, os.Mkdir(filepath.Join(repo, ".mcp.json"), 0o700)) },
			want:    "cannot be read",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, debuggerTeam(tc.envLine, ""))
			f.svc.Debugger = pluginLaunch(t)
			if tc.launch != nil {
				f.svc.Debugger = tc.launch(t, f)
			}
			if tc.repo != nil {
				tc.repo(t, f.repo)
			}
			checked := false
			f.svc.Preflight = func(context.Context, string) error {
				checked = true
				return nil
			}
			_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: settings.DebuggerType, Count: 1})
			require.ErrorIs(t, err, workforce.ErrConfig)
			require.ErrorIs(t, err, debugger.ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, checked, "refused before the preflight")
			assert.Empty(t, f.h.params("tab.create"))
			assert.Empty(t, f.h.params("agent.start"))
			rows, err := f.st.Workers(t.Context())
			require.NoError(t, err)
			assert.Empty(t, rows)
		})
	}

	f := newFixture(t, teamConfig)
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: settings.DebuggerType, Count: 1})
	require.ErrorIs(t, err, workforce.ErrUnknownGroup, "no debugger group: its launch is never built")
	assert.NotErrorIs(t, err, debugger.ErrInvalid)
}

// TestSpawnADescriptionOnlyDebugger: a debugger group that only describes
// is no worker type, so a spawn is an unknown group and its launch is never
// built: with no env file named, a build would be a configuration error.
func TestSpawnADescriptionOnlyDebugger(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig+"\n[projects.herdr-agentisan.groups.debugger]\ndescription = \"reads the trail\"\n")
	f.svc.Debugger = pluginLaunch(t)
	_, err := f.svc.Spawn(t.Context(), workforce.SpawnRequest{Group: settings.DebuggerType, Count: 1})
	require.ErrorIs(t, err, workforce.ErrUnknownGroup)
	assert.NotErrorIs(t, err, debugger.ErrInvalid)
	assert.Empty(t, f.h.params("tab.create"))
}

// upDebuggerTeam is debuggerTeam whose debugger group fills to one worker.
func upDebuggerTeam(envLine string) string {
	return strings.Replace(debuggerTeam(envLine, ""), "groups.debugger]\nworkers = 0\n", "groups.debugger]\nworkers = 1\n", 1)
}

// debuggerFill is team up's fill of the debugger group.
func debuggerFill(t *testing.T, res workforce.UpResult) workforce.GroupFill {
	t.Helper()
	i := slices.IndexFunc(res.Groups, func(g workforce.GroupFill) bool { return g.Name == settings.DebuggerType })
	require.GreaterOrEqual(t, i, 0, "the debugger group is filled")
	return res.Groups[i]
}

// TestUpOpensTheDebugger: team up fills a debugger group like any other,
// with the launch Spawn gives it (A32): its tab in the repo with the config
// dir in its environment, and the launch's argv, then --model and --name.
// Once it is up, a launch that can no longer be built refuses nothing,
// since no debugger is to open.
func TestUpOpensTheDebugger(t *testing.T) {
	t.Parallel()
	envFile := filepath.Join(t.TempDir(), "elastic.env")
	f := upFixture(t, upDebuggerTeam("[debugger]\nenv_file = \""+envFile+"\"\n"))
	f.svc.Debugger = pluginLaunch(t)

	res, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err)
	assert.Equal(t, workforce.GroupFill{Name: settings.DebuggerType, Workers: 1, Spawned: 1}, debuggerFill(t, res))
	l := f.svc.Debugger
	l.Repo, l.ConfigDir, l.KeyFiles = f.repo, f.config, []string{envFile}
	want, err := l.Command()
	require.NoError(t, err)
	i := slices.IndexFunc(f.h.params("tab.create"), func(m map[string]any) bool { return m["label"] == "ha-debugger-1" })
	require.GreaterOrEqual(t, i, 0, "the debugger's tab opens")
	tab := f.h.params("tab.create")[i]
	assert.Equal(t, want.Dir, tab["cwd"])
	assert.Equal(t, map[string]any{workforce.WorkerEnv: "ha-debugger-1", trail.ConfigDirEnv: f.config}, tab["env"])
	args := make([]any, 0, len(want.Args)+4)
	for _, a := range append(slices.Clone(want.Args), "--model", "opus", "--name", "ha-debugger-1") {
		args = append(args, a)
	}
	j := slices.IndexFunc(f.h.params("agent.start"), func(m map[string]any) bool { return m["name"] == "ha-debugger-1" })
	require.GreaterOrEqual(t, j, 0, "the debugger's agent starts")
	start := f.h.params("agent.start")[j]
	assert.Equal(t, "claude", start["kind"])
	assert.Equal(t, args, start["args"])
	assert.Equal(t, store.StatusActive, f.row(t, "ha-debugger-1").Status)
	assert.Equal(t, settings.DebuggerType, f.row(t, "ha-debugger-1").Group)

	f.svc.Debugger = debugger.Launch{}
	again, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.NoError(t, err, "the debugger is up: its launch is not needed")
	assert.Equal(t, workforce.GroupFill{Name: settings.DebuggerType, Workers: 1, Occupied: 1}, debuggerFill(t, again))
	assert.Empty(t, again.Workers)
}

// TestUpRefusesADebuggerItCannotLaunch: a debugger team up is to open but
// cannot launch refuses the whole team as a configuration error, as Spawn
// does: no tab, agent or row for any group.
func TestUpRefusesADebuggerItCannotLaunch(t *testing.T) {
	t.Parallel()
	f := upFixture(t, upDebuggerTeam(""))
	f.svc.Debugger = pluginLaunch(t)

	_, err := f.svc.Up(t.Context(), workforce.UpRequest{})
	require.ErrorIs(t, err, workforce.ErrConfig)
	require.ErrorIs(t, err, debugger.ErrInvalid)
	assert.Contains(t, err.Error(), "set [debugger] env_file")
	assert.Empty(t, f.h.params("tab.create"))
	assert.Empty(t, f.h.params("agent.start"))
	rows, err := f.st.Workers(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestPlanNeedsTheDebuggersLaunch: planning a debugger without its launch
// is a configuration error, never a bare claude.
func TestPlanNeedsTheDebuggersLaunch(t *testing.T) {
	t.Parallel()
	_, _, err := workforce.Plan(&settings.Profile{Agent: settings.Agent{Command: []string{"claude"}}}, settings.DebuggerType, "", "")
	require.ErrorIs(t, err, workforce.ErrConfig)
}

// TestRetireADebugger: a debugger writes no handoff, so retiring one after
// its handoff is refused at once, its pane left open; retiring it plainly
// closes its pane.
func TestRetireADebugger(t *testing.T) {
	t.Parallel()
	f, _ := newDebuggerFixture(t, "")
	w := f.worker(t, "ha-debugger-1", settings.DebuggerType)

	_, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID, AfterHandoff: true})
	require.ErrorIs(t, err, workforce.ErrBadRequest)
	assert.Contains(t, err.Error(), "retire it without --after-handoff")
	assert.Empty(t, f.h.params("pane.close"))
	assert.Equal(t, store.StatusActive, f.row(t, w.ID).Status)

	res, err := f.svc.Retire(t.Context(), workforce.RetireRequest{Target: w.ID})
	require.NoError(t, err)
	assert.Equal(t, workforce.OutcomeRetired, res.Outcome)
}

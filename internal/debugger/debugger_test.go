package debugger_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

var launch = debugger.Launch{
	Root:      "/opt/herdr/plugins/agentisan",
	Bin:       "/opt/herdr/plugins/agentisan/bin/herdr-agentisan",
	Repo:      "/home/u/src/assay",
	ServerDir: "/home/u/.local/state/herdr/plugins/agentisan/srv-0123456789ab",
	ConfigDir: "/home/u/.config/herdr/plugins/agentisan",
	KeyFiles:  []string{"/home/u/.config/nerdsrun/local.env"},
}

// TestCommand: the launch, exactly. The role comes from the plugin root, a
// second prompt gives the paths, and dontAsk denies every call that would
// ask: the debugger reads the repo and the daemon's directory, runs
// `events` and the built-in read-only commands, reads Linear, and does
// nothing else. The session tools that never ask are denied by name, and
// no MCP server loads but those --mcp-config names, which is none. Its
// reads are fenced to its working directories: the repo it starts in and
// the daemon's directory. The env names the config dir, where `events`
// finds its env file, and never a URL or a key.
func TestCommand(t *testing.T) {
	t.Parallel()
	got, err := launch.Command()
	require.NoError(t, err)
	assert.Equal(t, []string{
		"--append-system-prompt-file", "/opt/herdr/plugins/agentisan/roles/debugger.md",
		"--append-system-prompt", "Debugger session facts: the plugin binary is /opt/herdr/plugins/agentisan/bin/herdr-agentisan, " +
			"so run `/opt/herdr/plugins/agentisan/bin/herdr-agentisan events worker <id>` and " +
			"`/opt/herdr/plugins/agentisan/bin/herdr-agentisan events query '<ES|QL>'`; the project repo is /home/u/src/assay; " +
			"daemon.log is /home/u/.local/state/herdr/plugins/agentisan/srv-0123456789ab/daemon.log (rotated to daemon.log.1).",
		"--setting-sources", "project",
		"--permission-mode", "dontAsk",
		"--settings", `{"permissions":{"blockReadsOutsideWorkingDirectories":true}}`,
		"--strict-mcp-config",
		"--add-dir", "/home/u/.local/state/herdr/plugins/agentisan/srv-0123456789ab",
		"--allowedTools",
		"Read(//home/u/src/assay/**)",
		"Read(//home/u/.local/state/herdr/plugins/agentisan/srv-0123456789ab/**)",
		"Bash(/opt/herdr/plugins/agentisan/bin/herdr-agentisan events:*)",
		"mcp__claude_ai_Linear__get_issue", "mcp__claude_ai_Linear__list_issues", "mcp__claude_ai_Linear__list_comments",
		"--disallowedTools",
		"Read(//home/u/src/assay/**/.env*)", "Read(//home/u/.config/nerdsrun/local.env)",
		"Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch",
		"Agent", "Workflow", "Monitor", "TaskStop",
		"CronCreate", "CronDelete", "CronList", "ScheduleWakeup", "RemoteTrigger",
		"SendMessage", "ListAgents", "PushNotification", "SendUserFile", "SendFeedback",
		"Artifact", "ArtifactData", "ArtifactComments", "DesignSync",
		"EnterWorktree", "ExitWorktree",
		"ListMcpResourcesTool", "ReadMcpResourceTool",
	}, got.Args)
	assert.Equal(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": launch.ConfigDir}, got.Env)
	assert.Equal(t, launch.Repo, got.Dir, "it starts in the repo: the directory it starts in is a working directory")
	assertFenced(t, got.Args)

	require.NoError(t, settings.CheckAgentCommand(append([]string{"claude"}, got.Args...)), "D12: claude, the repo's bundle still loads")
	for _, flag := range []string{"--model", "-m", "--name", "-n", "--bare", "--dangerously-skip-permissions", "--mcp-config"} {
		assert.NotContains(t, got.Args, flag, "the workforce sets --model and --name; nothing widens the session")
	}
	for _, a := range got.Args {
		assert.NotContains(t, a, "ELASTIC", "the launch never carries the trail's URL or key")
	}

	// An env file reached through a link is denied by both of its names.
	l := launch
	l.KeyFiles = []string{"/home/u/.config/nerdsrun/local.env", "/run/user/1000/op/local.env"}
	got, err = l.Command()
	require.NoError(t, err)
	assert.Contains(t, got.Args, "Read(//home/u/.config/nerdsrun/local.env)")
	assert.Contains(t, got.Args, "Read(//run/user/1000/op/local.env)")
}

// assertFenced checks that args turn on the read fence, by the setting's
// exact name: Claude Code ignores a key it does not know, so a misspelled
// one would leave the reads unfenced without a word.
func assertFenced(t *testing.T, args []string) {
	t.Helper()
	i := slices.Index(args, "--settings")
	require.GreaterOrEqual(t, i, 0, "the launch passes --settings")
	require.Less(t, i+1, len(args))
	var s struct {
		Permissions map[string]any `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal([]byte(args[i+1]), &s))
	assert.Equal(t, map[string]any{"blockReadsOutsideWorkingDirectories": true}, s.Permissions)
}

// TestCommandWithoutTheServerDir: a launch that does not know the daemon's
// directory still starts, says so in its facts, and allows and fences
// reading the repo alone: no allow rule and no working directory for it.
func TestCommandWithoutTheServerDir(t *testing.T) {
	t.Parallel()
	l := launch
	l.ServerDir = ""
	got, err := l.Command()
	require.NoError(t, err)
	assertFenced(t, got.Args)
	assert.NotContains(t, got.Args, "--add-dir", "the repo, the directory it starts in, is its only working directory")
	assert.Equal(t, l.Repo, got.Dir)
	assert.Contains(t, got.Args, "Read(//home/u/src/assay/**)")
	assert.False(t, slices.ContainsFunc(got.Args, func(a string) bool { return strings.Contains(a, "srv-") }))
	assert.True(t, slices.ContainsFunc(got.Args, func(a string) bool {
		return strings.HasSuffix(a, "daemon.log was not given to this session.")
	}))
}

// TestCommandRefusesUnusablePaths: every path is absolute and clean, and
// holds nothing Claude's tool rules or argument lists would read as
// structure (a space or comma splits a list, a parenthesis ends a rule, a
// glob widens it); the error names which path.
func TestCommandRefusesUnusablePaths(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*debugger.Launch){
		"a relative repo":          func(l *debugger.Launch) { l.Repo = "src/assay" },
		"an unclean repo":          func(l *debugger.Launch) { l.Repo = "/home/u/src/../src/assay" },
		"a trailing slash":         func(l *debugger.Launch) { l.Repo = "/home/u/src/assay/" },
		"no root":                  func(l *debugger.Launch) { l.Root = "" },
		"no binary":                func(l *debugger.Launch) { l.Bin = "" },
		"no config dir":            func(l *debugger.Launch) { l.ConfigDir = "" },
		"a space in the binary":    func(l *debugger.Launch) { l.Bin = "/opt/my plugins/herdr-agentisan" },
		"a comma in the repo":      func(l *debugger.Launch) { l.Repo = "/home/u/a,b" },
		"a parenthesis":            func(l *debugger.Launch) { l.Repo = "/home/u/a)" },
		"a glob in the server dir": func(l *debugger.Launch) { l.ServerDir = "/state/srv-*" },
		"a quote":                  func(l *debugger.Launch) { l.Bin = "/opt/x'y" },
		"a line break":             func(l *debugger.Launch) { l.Root = "/opt/x\ny" },
		"a non-ASCII space":        func(l *debugger.Launch) { l.Repo = "/home/u/a\u00a0b" },
		"a NUL":                    func(l *debugger.Launch) { l.ConfigDir = "/home/u/a\x00b" },
		"invalid UTF-8":            func(l *debugger.Launch) { l.Repo = "/home/u/a\xffb" },
		"a cut multibyte sequence": func(l *debugger.Launch) { l.Root = "/opt/x\xe5\xaf" },
		// The key file must lie outside every directory the debugger reads.
		"no env file":                          func(l *debugger.Launch) { l.KeyFiles = nil },
		"a relative env file":                  func(l *debugger.Launch) { l.KeyFiles = []string{"local.env"} },
		"a repo that holds the env file":       func(l *debugger.Launch) { l.Repo = "/home/u" },
		"the filesystem root as the repo":      func(l *debugger.Launch) { l.Repo = "/" },
		"the env file as the repo":             func(l *debugger.Launch) { l.Repo = "/home/u/.config/nerdsrun/local.env" },
		"a server dir that holds the env file": func(l *debugger.Launch) { l.ServerDir = "/home/u/.config" },
		"a repo that holds the link's target": func(l *debugger.Launch) {
			l.KeyFiles = append(l.KeyFiles, "/home/u/src/assay/secrets/local.env")
		},
	} {
		l := launch
		mutate(&l)
		_, err := l.Command()
		require.ErrorIs(t, err, debugger.ErrInvalid, name)
	}
	l := launch
	l.Bin = "/opt/my plugins/herdr-agentisan"
	_, err := l.Command()
	assert.ErrorContains(t, err, `the binary holds ' '`)
}

// TestCommandKeepsUsablePathsAsWritten: a path checkPath accepts reaches the
// rules exactly as written, never normalized: claude matches the rule
// against the path as the filesystem spells it.
func TestCommandKeepsUsablePathsAsWritten(t *testing.T) {
	t.Parallel()
	for name, repo := range map[string]string{
		"non-ASCII letters":       "/home/u/kódér",
		"NFD":                     "/home/u/cafe\u0301",
		"NFC":                     "/home/u/caf\u00e9",
		"case kept":               "/Home/U/Assay",
		"a dash-led component":    "/home/u/-x",
		"secret-looking":          "/home/u/password=hunter2",
		"a Windows-reserved name": "/home/u/CON",
		// A directory beside the env file's, sharing a prefix, holds no part
		// of it.
		"a sibling sharing a prefix": "/home/u/.config/nerd",
	} {
		l := launch
		l.Repo = repo
		got, err := l.Command()
		require.NoError(t, err, name)
		assert.Contains(t, got.Args, "Read(/"+repo+"/**)", name)
		assert.Contains(t, got.Args, "Read(/"+repo+"/**/.env*)", name)
	}
}

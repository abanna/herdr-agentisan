package debugger_test

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

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
// nothing else. Its built-in tools are Bash, Read and ToolSearch alone, and
// its one MCP server is Linear's connector; the session tools that never
// ask are denied by name beneath that, and so are Linear's writes. With no
// MCP config above the repo, the settings are the fence and the server
// allowlist. Its reads are fenced to its working directories: the repo it
// starts in and the daemon's directory. The env names the config dir, where
// `events` finds its env file, and never a URL or a key.
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
		"--settings", `{"permissions":{"blockReadsOutsideWorkingDirectories":true},"allowedMcpServers":[{"serverUrl":"https://mcp.linear.app/*"}]}`,
		"--add-dir", "/home/u/.local/state/herdr/plugins/agentisan/srv-0123456789ab",
		"--tools", "Bash,Read,ToolSearch",
		"--allowedTools",
		"Read(//home/u/src/assay/**)",
		"Read(//home/u/.local/state/herdr/plugins/agentisan/srv-0123456789ab/**)",
		"Bash(/opt/herdr/plugins/agentisan/bin/herdr-agentisan events:*)",
		"mcp__claude_ai_Linear__get_issue", "mcp__claude_ai_Linear__list_issues", "mcp__claude_ai_Linear__list_comments",
		"--disallowedTools",
		"Read(//home/u/src/assay/**/.env*)", "Read(//home/u/.config/nerdsrun/local.env)",
		"Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch",
		"Agent", "Workflow", "Monitor", "TaskStop", "EnterPlanMode",
		"CronCreate", "CronDelete", "CronList", "ScheduleWakeup", "RemoteTrigger",
		"SendMessage", "ListAgents", "PushNotification", "SendUserFile", "SendFeedback",
		"Artifact", "ArtifactData", "ArtifactComments", "DesignSync",
		"EnterWorktree", "ExitWorktree",
		"ListMcpResourcesTool", "ReadMcpResourceTool", "ReadMcpResourceDirTool",
		"mcp__claude_ai_Linear__save_*", "mcp__claude_ai_Linear__delete_*", "mcp__claude_ai_Linear__create_*",
		"mcp__claude_ai_Linear__update_*", "mcp__claude_ai_Linear__merge_*", "mcp__claude_ai_Linear__submit_*",
		"mcp__claude_ai_Linear__share_*", "mcp__claude_ai_Linear__unshare_*", "mcp__claude_ai_Linear__mark_*",
		"mcp__claude_ai_Linear__resolve_*", "mcp__claude_ai_Linear__restore_*", "mcp__claude_ai_Linear__retire_*",
		"mcp__claude_ai_Linear__prepare_*",
	}, got.Args)
	assert.Equal(t, map[string]string{"HERDR_PLUGIN_CONFIG_DIR": launch.ConfigDir}, got.Env)
	assert.Equal(t, launch.Repo, got.Dir, "it starts in the repo: the directory it starts in is a working directory")
	assertFenced(t, got.Args)

	require.NoError(t, settings.CheckAgentCommand(append([]string{"claude"}, got.Args...)), "D12: claude, the repo's bundle still loads")
	for _, flag := range []string{"--model", "-m", "--name", "-n", "--bare", "--dangerously-skip-permissions", "--mcp-config"} {
		assert.NotContains(t, got.Args, flag, "the workforce sets --model and --name; nothing widens the session")
	}
	assert.NotContains(t, got.Args, "--strict-mcp-config", "it would keep claude.ai's connectors out, and Linear's with them")
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

// assertFenced checks that args turn on the read fence and allow Linear's
// connector as the only MCP server, by the settings' exact names: Claude
// Code ignores a key it does not know, so a misspelled one would leave the
// reads unfenced, or every server loading, without a word. The allowlist
// is a top-level key, beside permissions, not inside it.
func assertFenced(t *testing.T, args []string) {
	t.Helper()
	i := slices.Index(args, "--settings")
	require.GreaterOrEqual(t, i, 0, "the launch passes --settings")
	require.Less(t, i+1, len(args))
	var s struct {
		Permissions       map[string]any `json:"permissions"`
		AllowedMcpServers []any          `json:"allowedMcpServers"`
	}
	require.NoError(t, json.Unmarshal([]byte(args[i+1]), &s))
	assert.Equal(t, map[string]any{"blockReadsOutsideWorkingDirectories": true}, s.Permissions)
	assert.Equal(t, []any{map[string]any{"serverUrl": "https://mcp.linear.app/*"}}, s.AllowedMcpServers,
		"Linear's connector, by URL: a claude.ai connector's name cannot go in an allowlist")
}

// TestCommandRejectsTheProjectsMCPServers: every server named in the
// .mcp.json of the repo or a directory above it is rejected by name in the
// settings, beside the fence, so none loads or asks at start; a config
// with no servers adds nothing, and one that cannot be read or parsed
// refuses the launch, naming the file.
func TestCommandRejectsTheProjectsMCPServers(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	repo := filepath.Join(parent, "assay")
	sub := filepath.Join(repo, "svc")
	require.NoError(t, os.MkdirAll(sub, 0o700))
	writeMCP := func(dir, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(body), 0o600))
	}
	writeMCP(parent, `{"mcpServers":{"outer":{"command":"o"},"agentisan":{"command":"a"}}}`)
	writeMCP(repo, `{"mcpServers":{"agentisan":{"command":"a"},"b":{"type":"http","url":"https://b.example/mcp"}},"MCPSERVERS":{"not-one":{}}}`)
	writeMCP(sub, `{"mcpServers":{"below":{}}}`)
	l := launch
	l.Repo, l.ServerDir = repo, ""

	got, err := l.Command()
	require.NoError(t, err)
	assertFenced(t, got.Args)
	assert.Equal(t, []string{"agentisan", "b", "outer"}, rejectedServers(t, got.Args), "the repo's and its parents', once each; not a config below it, nor a key that is not mcpServers")

	writeMCP(repo, `{"projects":{}}`)
	writeMCP(parent, `{"mcpServers":null}`)
	got, err = l.Command()
	require.NoError(t, err)
	assert.Contains(t, got.Args, `{"permissions":{"blockReadsOutsideWorkingDirectories":true},"allowedMcpServers":[{"serverUrl":"https://mcp.linear.app/*"}]}`,
		"no servers, no disabledMcpjsonServers key")

	for name, body := range map[string]string{"not JSON": `{"mcpServers":`, "not an object": `[]`, "servers not an object": `{"mcpServers":["a"]}`} {
		writeMCP(parent, body)
		_, err = l.Command()
		require.ErrorIs(t, err, debugger.ErrInvalid, name)
		assert.Contains(t, err.Error(), filepath.Join(parent, ".mcp.json"), name)
	}

	for name, create := range map[string]func(string) error{
		"a directory": func(p string) error { return os.Mkdir(p, 0o700) },
		"a FIFO":      func(p string) error { return syscall.Mkfifo(p, 0o600) },
	} {
		path := filepath.Join(parent, ".mcp.json")
		require.NoError(t, os.RemoveAll(path))
		require.NoError(t, create(path), name)
		err = commandWithin(t, l, 30*time.Second)
		require.ErrorIs(t, err, debugger.ErrInvalid, "%s is refused, and a FIFO is never opened", name)
		assert.Contains(t, err.Error(), "not a regular file", name)
	}
}

// commandWithin is l.Command()'s error, and fails the test when it has not
// returned within d: opening a FIFO with no writer blocks for good, so a
// launch that opened one would otherwise hang the suite to its timeout.
func commandWithin(t *testing.T, l debugger.Launch, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := l.Command()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("the launch did not return within %s: it opened a file it should only have stat'd", d)
		return nil
	}
}

// rejectedServers is the servers args' settings reject, read by the key's
// exact name: Claude Code ignores a key it does not know.
func rejectedServers(t *testing.T, args []string) []string {
	t.Helper()
	i := slices.Index(args, "--settings")
	require.GreaterOrEqual(t, i, 0, "the launch passes --settings")
	require.Less(t, i+1, len(args))
	var s map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(args[i+1]), &s))
	var names []string
	require.NoError(t, json.Unmarshal(s["disabledMcpjsonServers"], &names))
	return names
}

// TestCommandWithoutTheServerDir: a launch that does not know the daemon's
// directory still starts, says so in its facts, and allows and fences
// reading the repo alone: no allow rule and no working directory for it.
// The allow list is exactly the repo's read, `events` and Linear's reads,
// never the rule an empty server dir would make, Read(//**), which allows
// reading the whole filesystem.
func TestCommandWithoutTheServerDir(t *testing.T) {
	t.Parallel()
	l := launch
	l.ServerDir = ""
	got, err := l.Command()
	require.NoError(t, err)
	assertFenced(t, got.Args)
	assert.NotContains(t, got.Args, "--add-dir", "the repo, the directory it starts in, is its only working directory")
	assert.Equal(t, l.Repo, got.Dir)
	assert.Equal(t, []string{
		"Read(//home/u/src/assay/**)",
		"Bash(/opt/herdr/plugins/agentisan/bin/herdr-agentisan events:*)",
		"mcp__claude_ai_Linear__get_issue", "mcp__claude_ai_Linear__list_issues", "mcp__claude_ai_Linear__list_comments",
	}, between(t, got.Args, "--allowedTools", "--disallowedTools"))
	assert.False(t, slices.ContainsFunc(got.Args, func(a string) bool { return strings.Contains(a, "srv-") }))
	assert.True(t, slices.ContainsFunc(got.Args, func(a string) bool {
		return strings.HasSuffix(a, "daemon.log was not given to this session.")
	}))
}

// between is the arguments after from and before to, the first of each.
func between(t *testing.T, args []string, from, to string) []string {
	t.Helper()
	i, j := slices.Index(args, from), slices.Index(args, to)
	require.GreaterOrEqual(t, i, 0, "the launch passes %s", from)
	require.Greater(t, j, i, "the launch passes %s after %s", to, from)
	return args[i+1 : j]
}

// after is the arguments after flag, the first one, to the end: the
// launch's last list.
func after(t *testing.T, args []string, flag string) []string {
	t.Helper()
	i := slices.Index(args, flag)
	require.GreaterOrEqual(t, i, 0, "the launch passes %s", flag)
	return args[i+1:]
}

// TestCommandKeepsTheToolsItsRulesName: --tools keeps every built-in tool
// an allow rule names, or the rule would allow a tool the session does not
// have, and no deny rule names one of them bare, which would remove it.
// Linear's rules name MCP tools, which --tools does not reach.
func TestCommandKeepsTheToolsItsRulesName(t *testing.T) {
	t.Parallel()
	noServerDir := launch
	noServerDir.ServerDir = ""
	for name, l := range map[string]debugger.Launch{"with the server dir": launch, "without it": noServerDir} {
		got, err := l.Command()
		require.NoError(t, err, name)
		list := between(t, got.Args, "--tools", "--allowedTools")
		require.Len(t, list, 1, "%s: one comma-joined list", name)
		tools := strings.Split(list[0], ",")
		for _, rule := range between(t, got.Args, "--allowedTools", "--disallowedTools") {
			if strings.HasPrefix(rule, "mcp__") {
				continue
			}
			tool, _, _ := strings.Cut(rule, "(")
			assert.Contains(t, tools, tool, "%s: %s names a tool --tools keeps", name, rule)
		}
		for _, rule := range after(t, got.Args, "--disallowedTools") {
			assert.NotContains(t, tools, rule, "%s: a bare deny would remove %s", name, rule)
		}
	}
}

// linearTools is every tool Linear's claude.ai connector offered on
// 2026-10-23 (Claude Code 2.1.309), by its name after the prefix. Its reads
// begin get_, list_, search_ or extract_; every other verb writes.
const linearTools = `
create_attachment create_attachment_from_upload create_initiative_label create_issue_label
delete_attachment delete_comment delete_customer delete_customer_need delete_diff_comment delete_status_update
extract_images
get_agent_skill get_attachment get_diff get_diff_threads get_document get_initiative get_issue get_issue_status
get_milestone get_notifications get_project get_release get_release_note get_status_updates get_team get_template
get_triage_responsibility get_user get_workspace
list_agent_skills list_comments list_custom_views list_customer_needs list_customers list_cycles list_diffs
list_documents list_initiative_labels list_initiatives list_issue_labels list_issue_statuses list_issues
list_milestones list_project_labels list_projects list_release_notes list_release_pipelines list_releases
list_teams list_templates list_users
mark_notification merge_diff prepare_attachment_upload resolve_diff_thread
restore_initiative_label restore_issue_label restore_project_label
retire_initiative_label retire_issue_label retire_project_label
save_comment save_customer save_customer_need save_diff_comment save_document save_initiative
save_initiative_label save_issue save_issue_label save_milestone save_project save_project_label
save_release save_release_note save_status_update
search_documentation share_issue submit_diff_review unshare_issue update_diff`

// TestCommandDeniesLinearsWrites: the launch's Linear globs remove every
// tool of Linear's connector that writes, each by exactly one glob, and
// none that reads, the three the debugger may call among them; every glob
// removes one at least. A deny rule's glob must match the whole tool name,
// as path.Match does. A verb Linear adds later is matched by none: dontAsk
// still refuses its tools, since no rule allows them.
func TestCommandDeniesLinearsWrites(t *testing.T) {
	t.Parallel()
	got, err := launch.Command()
	require.NoError(t, err)
	first := debugger.LinearReadTools[0]
	prefix := first[:strings.LastIndex(first, "__")+2]
	var globs []string
	for _, rule := range after(t, got.Args, "--disallowedTools") {
		if strings.HasPrefix(rule, prefix) {
			globs = append(globs, rule)
		}
	}
	require.NotEmpty(t, globs)
	denying := func(tool string) []string {
		var m []string
		for _, g := range globs {
			ok, err := path.Match(g, tool)
			require.NoError(t, err, g)
			if ok {
				m = append(m, g)
			}
		}
		return m
	}
	used := map[string]bool{}
	for _, name := range strings.Fields(linearTools) {
		tool := prefix + name
		if slices.ContainsFunc([]string{"get_", "list_", "search_", "extract_"}, func(v string) bool { return strings.HasPrefix(name, v) }) {
			assert.Empty(t, denying(tool), "%s reads", tool)
			continue
		}
		m := denying(tool)
		assert.Len(t, m, 1, "%s writes: one glob removes it", tool)
		for _, g := range m {
			used[g] = true
		}
	}
	assert.Len(t, used, len(globs), "each of %v removes a tool Linear offers", globs)
	for _, tool := range debugger.LinearReadTools {
		assert.Contains(t, strings.Fields(linearTools), strings.TrimPrefix(tool, prefix), "%s is a tool Linear offers", tool)
		assert.Empty(t, denying(tool), "%s is allowed, and a deny would beat the allow", tool)
	}
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

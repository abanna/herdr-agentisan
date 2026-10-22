package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/codexhooks"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// oneJSONObject decodes out as exactly one JSON object and nothing else.
func oneJSONObject(t *testing.T, out string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var v map[string]any
	require.NoError(t, dec.Decode(&v), "stdout is a JSON object: %q", out)
	_, err := dec.Token()
	require.ErrorIs(t, err, io.EOF, "and nothing after it: %q", out)
	return v
}

// runOut runs args with stdout and stderr apart, as the skill's wrapper
// reads them, in the test lineage.
func runOut(t *testing.T, lookup func(string) (string, bool), args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := cli.Root()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err = root.ExecuteContext(cli.WithLookupEnv(cli.WithLineage(t.Context(), testLineage), lookup))
	return out.String(), errOut.String(), err
}

// wrapperReads is how the herdr-workforce skill's wrapper reads a command's
// --help (workforce.py unsupported): the command is available when the help
// exits 0 with a line, on stdout or stderr, that is or starts with
// `herdr-agentisan <group> <verb>`; a long flag the skill passes must then be
// on a stdout line of its own starting with "-". listed is those flag lines.
func wrapperReads(t *testing.T, e daemonEnv, cmd string) (available bool, listed string, out string) {
	t.Helper()
	out, errOut, err := runOut(t, e.lookup(nil), append(strings.Fields(cmd), "--help")...)
	usage := "herdr-agentisan " + cmd
	found := false
	for line := range strings.SplitSeq(out+"\n"+errOut, "\n") {
		if s := strings.TrimSpace(line); s == usage || strings.HasPrefix(s, usage+" ") {
			found = true
		}
	}
	var flagLines []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "-") {
			flagLines = append(flagLines, line)
		}
	}
	return err == nil && found, strings.Join(flagLines, "\n"), out + errOut
}

// flagOn matches flag f on a flag line, as a word of its own.
func flagOn(f string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(f) + `($|[^\w-])`)
}

// TestWorkforceHelpMeetsTheWrapper: every built workforce command reads as
// available to the skill's wrapper, with each long flag the skill passes.
func TestWorkforceHelpMeetsTheWrapper(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"team status":   {"--project", "--state-dir"},
		"team up":       {"--project", "--state-dir"},
		"worker spawn":  {"--group", "--count", "--model", "--project", "--state-dir"},
		"worker retire": {"--after-handoff", "--state-dir"},
	}
	e := newDaemonEnv(t)
	for cmd, flags := range tests {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			available, listed, out := wrapperReads(t, e, cmd)
			require.True(t, available, "%q reads as available in:\n%s", cmd, out)
			for _, f := range flags {
				assert.Regexp(t, flagOn(f), listed, "%s is on a flag line", f)
			}
		})
	}
}

// TestUnbuiltCommandsStayUnavailable: a command or flag the skill knows but
// this build lacks reads as unavailable to the wrapper, so the skill reports
// it and runs nothing: `boss handoff` (ADR-001 item 6, NERD-5262) and spawn's
// `--replaces` (item 5b, NERD-5261). Flip each as its part ships (QA #39
// item 3).
func TestUnbuiltCommandsStayUnavailable(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	available, _, out := wrapperReads(t, e, "boss handoff")
	assert.False(t, available, "boss handoff reads as unavailable:\n%s", out)

	available, listed, out := wrapperReads(t, e, "worker spawn")
	require.True(t, available, out)
	assert.NotRegexp(t, flagOn("--replaces"), listed, "spawn has no --replaces yet")
}

// TestWorkforceCommandsAlwaysAnswerInJSON: stdout is exactly one JSON object
// on every path the command line can take, and the exit is 1 for each
// refusal: a flag or argument error, an unknown verb, no daemon, and a herdr
// that fails finding the caller's pane, refused before the daemon is asked.
func TestWorkforceCommandsAlwaysAnswerInJSON(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	tests := map[string]struct {
		args []string
		env  map[string]string
		code string
	}{
		"an unknown flag":            {args: []string{"team", "status", "--bogus"}, code: "bad_request"},
		"a flag with no value":       {args: []string{"team", "status", "--project"}, code: "bad_request"},
		"an argument to team status": {args: []string{"team", "status", "now"}, code: "bad_request"},
		"an argument to team up":     {args: []string{"team", "up", "now"}, code: "bad_request"},
		"team up takes no group":     {args: []string{"team", "up", "--group", "coder"}, code: "bad_request"},
		"up with no daemon":          {args: []string{"team", "up"}, code: "daemon_unavailable"},
		"an unknown verb":            {args: []string{"team", "down"}, code: "bad_request"},
		"a group alone":              {args: []string{"team"}, code: "bad_request"},
		"retire with no id":          {args: []string{"worker", "retire"}, code: "bad_request"},
		"retire with two ids":        {args: []string{"worker", "retire", "a", "b"}, code: "bad_request"},
		"retire takes no project":    {args: []string{"worker", "retire", "ha-coder-1", "--project", "ha"}, code: "bad_request"},
		"an unknown worker verb":     {args: []string{"worker", "adopt"}, code: "bad_request"},
		"spawn with no daemon":       {args: []string{"worker", "spawn", "--group", "coder"}, code: "daemon_unavailable"},
		"retire with no daemon":      {args: []string{"worker", "retire", "ha-coder-1"}, code: "daemon_unavailable"},
		"no daemon":                  {args: []string{"team", "status"}, code: "daemon_unavailable"},
		"no state dir":               {args: []string{"team", "status"}, env: map[string]string{"HERDR_PLUGIN_STATE_DIR": ""}, code: "daemon_unavailable"},
		"no herdr":                   {args: []string{"team", "status"}, env: map[string]string{"HERDR_SOCKET_PATH": ""}, code: "daemon_unavailable"},
		"herdr failing":              {args: []string{"team", "status"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, code: "herdr_failed"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, _, err := runOut(t, e.lookup(tc.env), tc.args...)
			require.Error(t, err, "a refusal exits 1")
			v := oneJSONObject(t, out)
			assert.Equal(t, false, v["ok"])
			errBody, _ := v["error"].(map[string]any)
			assert.Equal(t, tc.code, errBody["code"])
			assert.NotEmpty(t, errBody["message"])
		})
	}
}

// TestWorkforceCommandsAskTheDaemon: each command sends its op with the
// request the daemon decodes, the caller's pane included as
// workforce.FindCallerPane confirms it, and prints the daemon's answer as
// the envelope: data when ok, its code when refused. Under --project, and
// for worker retire, which needs no team, herdr is never asked.
func TestWorkforceCommandsAskTheDaemon(t *testing.T) {
	t.Parallel()
	// theirs is a herdr whose only pane, w3:p1, is another team's: its shell
	// is not in the test lineage.
	theirs := func(r herdrtest.Request) herdrtest.Reply {
		if r.Method == "pane.list" {
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []map[string]any{{"pane_id": "w3:p1"}}}}
		}
		return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": "w3:p1", "shell_pid": 7000}}}
	}
	failing := func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "internal", Message: "boom"}}
	}
	const ok = `{"ok":true,"data":{"project":"ha"}}`
	const unknown = `{"ok":false,"error":{"code":"unknown_project","message":"\"nope\"; configured: ha"}}`
	tests := map[string]struct {
		args  []string
		reply string
		sent  string
		want  string
		// stateDir passes the daemon's state dir as --state-dir, with
		// HERDR_PLUGIN_STATE_DIR naming another.
		stateDir bool
		// pane is HERDR_PANE_ID, in a herdr where w14:p1 runs the test
		// lineage and w9:p9 is its alias, unless herdr is set.
		pane  string
		herdr herdrtest.Handler
	}{
		"team status, the caller's team": {
			args: []string{"team", "status"}, reply: ok, pane: "w14:p1",
			sent: `{"v":1,"op":"team.status","args":{"caller_pane":"w14:p1"}}`, want: ok,
		},
		"team status of a project, herdr unasked": {
			args: []string{"team", "status", "--project", "ha"}, reply: ok, pane: "w14:p1", herdr: failing,
			sent: `{"v":1,"op":"team.status","args":{"project":"ha"}}`, want: ok,
		},
		"a refusal is printed with its code": {
			args: []string{"team", "status", "--project", "nope"}, reply: unknown, pane: "w14:p1",
			sent: `{"v":1,"op":"team.status","args":{"project":"nope"}}`, want: unknown,
		},
		"team up, the caller's team": {
			args: []string{"team", "up"}, reply: ok, pane: "w14:p1",
			sent: `{"v":1,"op":"team.up","args":{"caller_pane":"w14:p1"}}`, want: ok,
		},
		"team up of a project": {
			args: []string{"team", "up", "--project", "ha"}, reply: ok, pane: "w14:p1", herdr: failing,
			sent: `{"v":1,"op":"team.up","args":{"project":"ha"}}`, want: ok,
		},
		"worker spawn, the caller's team": {
			args: []string{"worker", "spawn", "--group", "coder", "--count", "2", "--model", "sonnet"}, reply: ok, pane: "w9:p9",
			sent: `{"v":1,"op":"worker.spawn","args":{"caller_pane":"w14:p1","group":"coder","count":2,"model":"sonnet"}}`, want: ok,
		},
		"worker spawn's count defaults to 1": {
			args: []string{"worker", "spawn", "--group", "qa", "--project", "ha"}, reply: ok, pane: "w14:p1",
			sent: `{"v":1,"op":"worker.spawn","args":{"project":"ha","group":"qa","count":1}}`, want: ok,
		},
		"worker retire after its handoff, herdr unasked": {
			args: []string{"worker", "retire", "ha-coder-2", "--after-handoff"}, reply: ok, pane: "w14:p1", herdr: failing,
			sent: `{"v":1,"op":"worker.retire","args":{"target":"ha-coder-2","after_handoff":true}}`, want: ok,
		},
		"worker retire by pane id": {
			args: []string{"worker", "retire", "w14:p7"}, reply: ok, pane: "w14:p1",
			sent: `{"v":1,"op":"worker.retire","args":{"target":"w14:p7"}}`, want: ok,
		},
		"--state-dir beats the environment": {
			args: []string{"team", "status"}, reply: ok, stateDir: true, pane: "w14:p1",
			sent: `{"v":1,"op":"team.status","args":{"caller_pane":"w14:p1"}}`, want: ok,
		},
		"a frozen alias sends the pane's current id": {
			args: []string{"team", "status"}, reply: ok, pane: "w9:p9",
			sent: `{"v":1,"op":"team.status","args":{"caller_pane":"w14:p1"}}`, want: ok,
		},
		"an id reissued to another team's pane is not sent": {
			args: []string{"team", "status"}, reply: ok, pane: "w3:p1", herdr: theirs,
			sent: `{"v":1,"op":"team.status","args":{}}`, want: ok,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := tc.herdr
			if h == nil {
				h = paneHerdr("w14:p1", "w9:p9")
			}
			e := newDaemonEnvWith(t, h)
			sent := fakeDaemon(t, e, tc.reply)
			env := map[string]string{"HERDR_PANE_ID": tc.pane}
			args := tc.args
			if tc.stateDir {
				env["HERDR_PLUGIN_STATE_DIR"] = t.TempDir()
				args = append(slices.Clone(args), "--state-dir", e.stateDir)
			}

			out, _, err := runOut(t, e.lookup(env), args...)
			if strings.Contains(tc.reply, `"ok":true`) {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a refusal exits 1")
			}
			assert.JSONEq(t, tc.want, out)
			oneJSONObject(t, out)
			require.Len(t, sent, 1)
			assert.JSONEq(t, tc.sent, <-sent)
		})
	}
}

// TestDaemonRunNamesThePluginsDashboard: daemon run gives the workforce the
// plugin's dashboard pane as the manifest declares it
// (TestManifestDeclaresTheDashboardSplit), so team up opens it, and checks
// Codex's hooks in the Codex home its own environment names.
func TestDaemonRunNamesThePluginsDashboard(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	home := t.TempDir()
	var logs bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(cli.WithLookupEnv(daemonCtx(t, e, nil, &logs), e.lookup(map[string]string{"HERDR_PLUGIN_CONFIG_DIR": "/config", "CODEX_HOME": home})))

	o, err := cli.DaemonOptions(cmd, "", false)
	require.NoError(t, err)
	assert.Equal(t, workforce.DashboardPane{PluginID: plugin.ID, Entrypoint: plugin.DashboardPane, Title: plugin.DashboardTitle}, o.Team.Dashboard)
	require.NotNil(t, o.Team.CodexHooks)
	installed, err := o.Team.CodexHooks()
	require.NoError(t, err)
	assert.False(t, installed, "none in the home its environment names")
	_, err = codexhooks.Install(home)
	require.NoError(t, err)
	installed, err = o.Team.CodexHooks()
	require.NoError(t, err)
	assert.True(t, installed)
}

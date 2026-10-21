package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
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

// TestWorkforceHelpMeetsTheWrapper: the herdr-workforce skill's wrapper
// decides a command is available from its --help alone (workforce.py
// unsupported): exit 0, a line that is or starts with `herdr-agentisan
// <group> <verb>`, and each long flag the skill passes on a line of its own
// starting with "-". The same rule, applied here to every command.
func TestWorkforceHelpMeetsTheWrapper(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"team status":   {"--project", "--state-dir"},
		"worker retire": {"--after-handoff", "--state-dir"},
	}
	e := newDaemonEnv(t)
	for cmd, flags := range tests {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			out, _, err := runOut(t, e.lookup(nil), append(strings.Fields(cmd), "--help")...)
			require.NoError(t, err)

			usage := "herdr-agentisan " + cmd
			found := false
			var flagLines []string
			for line := range strings.SplitSeq(out, "\n") {
				if s := strings.TrimSpace(line); s == usage || strings.HasPrefix(s, usage+" ") {
					found = true
				}
				if strings.HasPrefix(strings.TrimLeft(line, " \t"), "-") {
					flagLines = append(flagLines, line)
				}
			}
			assert.True(t, found, "a %q usage line in:\n%s", usage, out)
			listed := strings.Join(flagLines, "\n")
			for _, f := range flags {
				assert.Regexp(t, regexp.MustCompile(`(^|[^\w-])`+regexp.QuoteMeta(f)+`($|[^\w-])`), listed, "%s is on a flag line", f)
			}
		})
	}
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
		"an unknown verb":            {args: []string{"team", "down"}, code: "bad_request"},
		"a group alone":              {args: []string{"team"}, code: "bad_request"},
		"retire with no id":          {args: []string{"worker", "retire"}, code: "bad_request"},
		"retire with two ids":        {args: []string{"worker", "retire", "a", "b"}, code: "bad_request"},
		"retire takes no project":    {args: []string{"worker", "retire", "ha-coder-1", "--project", "ha"}, code: "bad_request"},
		"an unknown worker verb":     {args: []string{"worker", "spawn"}, code: "bad_request"},
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

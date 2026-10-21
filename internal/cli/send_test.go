package cli_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// runStdin is runOut with stdin.
func runStdin(t *testing.T, lookup func(string) (string, bool), stdin string, args ...string) (stdout string, err error) {
	t.Helper()
	root := cli.Root()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err = root.ExecuteContext(cli.WithLookupEnv(cli.WithLineage(t.Context(), testLineage), lookup))
	return out.String(), err
}

// TestSendAsksTheDaemon: send sends its op with the recipient, the body,
// from its argument or, for -, stdin byte for byte, the caller's pane as
// workforce.FindCallerPane confirms it, and HERDR_AGENTISAN_WORKER; and
// prints the daemon's answer as the envelope.
func TestSendAsksTheDaemon(t *testing.T) {
	t.Parallel()
	const ok = `{"ok":true,"data":{"id":"01J","state":"queued","to":{"kind":"boss","id":"ha-boss"},"from":{"kind":"worker","id":"ha-coder-1"}}}`
	const unknown = `{"ok":false,"error":{"code":"unknown_recipient","message":"no live worker has the id \"ha-coder-9\""}}`
	atTheCap := strings.Repeat("é", store.MaxBody/2)
	tests := map[string]struct {
		args  []string
		stdin string
		env   map[string]string
		reply string
		sent  map[string]any
	}{
		"a body argument": {
			args: []string{"send", "boss", "done: #72"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, reply: ok,
			sent: map[string]any{"to": "boss", "body": "done: #72", "caller_pane": "w14:p1"},
		},
		"a body on stdin": {
			args: []string{"send", "ha-boss", "-"}, stdin: "line one\nline two\n", env: map[string]string{"HERDR_PANE_ID": "w9:p9"}, reply: ok,
			sent: map[string]any{"to": "ha-boss", "body": "line one\nline two\n", "caller_pane": "w14:p1"},
		},
		"a body on stdin at the cap": {
			args: []string{"send", "boss", "-"}, stdin: atTheCap, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, reply: ok,
			sent: map[string]any{"to": "boss", "body": atTheCap, "caller_pane": "w14:p1"},
		},
		"a body like a flag, after --": {
			args: []string{"send", "--", "boss", "-5 degrees"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, reply: ok,
			sent: map[string]any{"to": "boss", "body": "-5 degrees", "caller_pane": "w14:p1"},
		},
		"no pane, the worker's variable": {
			args: []string{"send", "boss", "hi"}, env: map[string]string{"HERDR_PANE_ID": "", "HERDR_AGENTISAN_WORKER": "ha-coder-1"}, reply: ok,
			sent: map[string]any{"to": "boss", "body": "hi", "worker": "ha-coder-1"},
		},
		"a refusal is printed with its code": {
			args: []string{"send", "ha-coder-9", "hi"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, reply: unknown,
			sent: map[string]any{"to": "ha-coder-9", "body": "hi", "caller_pane": "w14:p1"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnvWith(t, paneHerdr("w14:p1", "w9:p9"))
			sent := fakeDaemon(t, e, tc.reply)
			out, err := runStdin(t, e.lookup(tc.env), tc.stdin, tc.args...)
			if strings.Contains(tc.reply, `"ok":true`) {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "a refusal exits 1")
			}
			assert.JSONEq(t, tc.reply, out)
			oneJSONObject(t, out)
			require.Len(t, sent, 1)
			var req struct {
				V    int            `json:"v"`
				Op   string         `json:"op"`
				Args map[string]any `json:"args"`
			}
			require.NoError(t, json.Unmarshal([]byte(<-sent), &req))
			assert.Equal(t, 1, req.V)
			assert.Equal(t, "send", req.Op)
			assert.Equal(t, tc.sent, req.Args)
		})
	}
}

// TestSendRefusesBeforeAsking: a command line send cannot take, a body on
// stdin over the cap, no daemon, and a herdr that fails finding the caller's
// pane are refused in one JSON object, exit 1, and the daemon is never
// asked.
func TestSendRefusesBeforeAsking(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args   []string
		stdin  string
		env    map[string]string
		daemon bool
		code   string
	}{
		"no arguments":             {args: []string{"send"}, daemon: true, code: "bad_request"},
		"no body":                  {args: []string{"send", "boss"}, daemon: true, code: "bad_request"},
		"an unquoted body":         {args: []string{"send", "boss", "done", "now"}, daemon: true, code: "bad_request"},
		"an unknown flag":          {args: []string{"send", "boss", "hi", "--project", "ha"}, daemon: true, code: "bad_request"},
		"a body like a flag":       {args: []string{"send", "boss", "-5 degrees"}, daemon: true, code: "bad_request"},
		"stdin over the cap":       {args: []string{"send", "boss", "-"}, stdin: strings.Repeat("x", store.MaxBody+1), daemon: true, code: "too_large"},
		"no daemon":                {args: []string{"send", "boss", "hi"}, code: "daemon_unavailable"},
		"herdr failing":            {args: []string{"send", "boss", "hi"}, env: map[string]string{"HERDR_PANE_ID": "w14:p1"}, daemon: true, code: "herdr_failed"},
		"no herdr socket":          {args: []string{"send", "boss", "hi"}, env: map[string]string{"HERDR_SOCKET_PATH": ""}, code: "daemon_unavailable"},
		"no plugin state dir here": {args: []string{"send", "boss", "hi"}, env: map[string]string{"HERDR_PLUGIN_STATE_DIR": ""}, code: "daemon_unavailable"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnv(t)
			var asked <-chan string
			if tc.daemon {
				asked = fakeDaemon(t, e, `{"ok":true,"data":{}}`)
			}
			out, err := runStdin(t, e.lookup(tc.env), tc.stdin, tc.args...)
			require.Error(t, err, "a refusal exits 1")
			v := oneJSONObject(t, out)
			assert.Equal(t, false, v["ok"])
			errBody, _ := v["error"].(map[string]any)
			assert.Equal(t, tc.code, errBody["code"])
			assert.NotEmpty(t, errBody["message"])
			assert.Empty(t, asked, "the daemon is never asked")
		})
	}
}

// TestSendHelp: --help names the command's shape and its one flag.
func TestSendHelp(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	out, err := runStdin(t, e.lookup(nil), "", "send", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "herdr-agentisan send <to> <body|->")
	assert.Contains(t, out, "--state-dir")
}

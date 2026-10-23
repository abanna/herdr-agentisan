package cli_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
)

// TestBoardAsksTheDaemon (A60): each board command sends its op with what
// its flags say, and nothing says who reads: the daemon tells that by the
// kernel. It prints the daemon's answer as the envelope; a board wait whose
// timeout passed exits 3, a refusal 1.
func TestBoardAsksTheDaemon(t *testing.T) {
	t.Parallel()
	const read = `{"ok":true,"data":{"project":"ha","reader":"boss","posts":[],"more":0,"mark":4,"newest":4}}`
	const woke = `{"ok":true,"data":{"project":"ha","reader":"boss","mark":4,"newest":5,"unread":1}}`
	const timedOut = `{"ok":true,"data":{"project":"ha","reader":"boss","mark":4,"newest":4,"unread":0,"timed_out":true}}`
	const refused = `{"ok":false,"error":{"code":"not_allowed","message":"only a team's boss and the user's dashboard read the board; workers may only post"}}`
	tests := map[string]struct {
		args  []string
		reply string
		sent  string
		exit  int
	}{
		"read":                {args: []string{"board", "read"}, reply: read, sent: `{"v":1,"op":"board.read","args":{"limit":50}}`},
		"read all":            {args: []string{"board", "read", "--all", "--limit", "5"}, reply: read, sent: `{"v":1,"op":"board.read","args":{"limit":5,"all":true}}`},
		"read, refused":       {args: []string{"board", "read"}, reply: refused, sent: `{"v":1,"op":"board.read","args":{"limit":50}}`, exit: 1},
		"wait":                {args: []string{"board", "wait"}, reply: woke, sent: `{"v":1,"op":"board.wait","args":{"timeout":1800000000000}}`},
		"wait after a seq":    {args: []string{"board", "wait", "--after", "3", "--timeout", "10s"}, reply: woke, sent: `{"v":1,"op":"board.wait","args":{"after":3,"timeout":10000000000}}`},
		"wait after 0, given": {args: []string{"board", "wait", "--after", "0"}, reply: woke, sent: `{"v":1,"op":"board.wait","args":{"after":0,"timeout":1800000000000}}`},
		"wait, timed out":     {args: []string{"board", "wait", "--timeout", "1m"}, reply: timedOut, sent: `{"v":1,"op":"board.wait","args":{"timeout":60000000000}}`, exit: 3},
		"wait, superseded": {
			args: []string{"board", "wait"}, reply: `{"ok":false,"error":{"code":"superseded","message":"a later board wait from the same pane took this one's place"}}`,
			sent: `{"v":1,"op":"board.wait","args":{"timeout":1800000000000}}`, exit: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnvWith(t, failingHerdr)
			sent := fakeDaemon(t, e, tc.reply)
			out, _, err := runOut(t, e.lookup(map[string]string{"HERDR_PANE_ID": "w14:p1"}), tc.args...)
			assert.Equal(t, tc.exit, cli.ExitCode(err), "%v", err)
			assert.JSONEq(t, tc.reply, out)
			oneJSONObject(t, out)
			require.Len(t, sent, 1)
			assert.JSONEq(t, tc.sent, <-sent)
		})
	}
}

// TestExitCode: only a board wait that timed out exits 3.
func TestExitCode(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, cli.ExitCode(nil))
	assert.Equal(t, 1, cli.ExitCode(errors.New("refused")))
	assert.Equal(t, 3, cli.ExitCode(cli.ErrWaitTimedOut))
}

// TestBoardRefusesBeforeAsking: a command line the board commands cannot
// take, and no daemon, are refused in one JSON object, exit 1, and the
// daemon is never asked.
func TestBoardRefusesBeforeAsking(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args   []string
		daemon bool
		code   string
	}{
		"the group alone":       {args: []string{"board"}, daemon: true, code: "bad_request"},
		"an unknown verb":       {args: []string{"board", "post", "hi"}, daemon: true, code: "bad_request"},
		"read with an argument": {args: []string{"board", "read", "ha"}, daemon: true, code: "bad_request"},
		"read with no limit":    {args: []string{"board", "read", "--limit", "many"}, daemon: true, code: "bad_request"},
		"read takes no reader":  {args: []string{"board", "read", "--reader", "boss"}, daemon: true, code: "bad_request"},
		"wait with an argument": {args: []string{"board", "wait", "now"}, daemon: true, code: "bad_request"},
		"wait after no seq":     {args: []string{"board", "wait", "--after", "last"}, daemon: true, code: "bad_request"},
		"wait for no duration":  {args: []string{"board", "wait", "--timeout", "a while"}, daemon: true, code: "bad_request"},
		"wait takes no project": {args: []string{"board", "wait", "--project", "ha"}, daemon: true, code: "bad_request"},
		"read with no daemon":   {args: []string{"board", "read"}, code: "daemon_unavailable"},
		"wait with no daemon":   {args: []string{"board", "wait"}, code: "daemon_unavailable"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnv(t)
			var asked <-chan string
			if tc.daemon {
				asked = fakeDaemon(t, e, `{"ok":true,"data":{}}`)
			}
			out, _, err := runOut(t, e.lookup(nil), tc.args...)
			assert.Equal(t, 1, cli.ExitCode(err), "a refusal exits 1")
			v := oneJSONObject(t, out)
			assert.Equal(t, false, v["ok"])
			errBody, _ := v["error"].(map[string]any)
			assert.Equal(t, tc.code, errBody["code"])
			assert.Empty(t, asked, "the daemon is never asked")
		})
	}
}

// TestBoardHelp: each command's usage and flags.
func TestBoardHelp(t *testing.T) {
	t.Parallel()
	e := newDaemonEnv(t)
	for cmd, want := range map[string][]string{
		"board read": {"herdr-agentisan board read", "--limit int", "(default 50)", "1 to 200", "--all", "moving no mark", "--state-dir"},
		"board wait": {"herdr-agentisan board wait", "--after int", "--timeout duration", "(default 30m0s)", "at most 6h0m0s", "exits 3", "superseded", "--state-dir"},
	} {
		out, _, err := runOut(t, e.lookup(nil), append(strings.Fields(cmd), "--help")...)
		require.NoError(t, err)
		for _, w := range want {
			assert.Contains(t, out, w, cmd)
		}
	}
}

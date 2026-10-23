package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// TestACallerOnTheSharedAppServer (A64, NERD-5506): under Codex's shared
// app-server the command's HERDR_PANE_ID and HERDR_AGENTISAN_WORKER are the
// session's that started the daemon, w14:p1's ha-coder-1, and its lineage runs
// through w14:p1's shell (codexLineage(true)); once that TUI exits, it runs
// to init and confirms no pane (appServerAlone), and main sent as ha-coder-1.
// So send and handoff ready with no target are refused caller_unverified in
// one JSON object, exit 1, and the daemon is never asked; team status asks
// with no caller pane, so it takes its fallbacks. From the pane's own Codex
// (--no-daemon) each asks as before.
func TestACallerOnTheSharedAppServer(t *testing.T) {
	t.Parallel()
	const ok = `{"ok":true,"data":{}}`
	env := map[string]string{"HERDR_PANE_ID": "w14:p1", "HERDR_AGENTISAN_WORKER": "ha-coder-1", "CODEX_THREAD_ID": "019a"}
	shared, own := codexLineage(true), codexLineage(false)
	tests := map[string]struct {
		args    []string
		lineage func() *report.Lineage
		sent    string // "" when the daemon is never asked
	}{
		"send":                                 {args: []string{"send", "boss", "hi"}, lineage: shared},
		"handoff ready":                        {args: []string{"handoff", "ready"}, lineage: shared},
		"send, the starting TUI gone":          {args: []string{"send", "boss", "hi"}, lineage: appServerAlone},
		"handoff ready, the starting TUI gone": {args: []string{"handoff", "ready"}, lineage: appServerAlone},
		"team status":                          {args: []string{"team", "status"}, lineage: shared, sent: `{"v":1,"op":"team.status","args":{}}`},
		"send, the pane's own Codex":           {args: []string{"send", "boss", "hi"}, lineage: own, sent: `{"v":1,"op":"send","args":{"to":"boss","body":"hi","caller_pane":"w14:p1","worker":"ha-coder-1"}}`},
		"handoff ready, its own":               {args: []string{"handoff", "ready"}, lineage: own, sent: `{"v":1,"op":"handoff.ready","args":{"caller_pane":"w14:p1","worker":"ha-coder-1"}}`},
		"team status, its own Codex":           {args: []string{"team", "status"}, lineage: own, sent: `{"v":1,"op":"team.status","args":{"caller_pane":"w14:p1"}}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newDaemonEnvWith(t, paneHerdr("w14:p1"))
			sent := fakeDaemon(t, e, ok)
			root := cli.Root()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(io.Discard)
			root.SetArgs(tc.args)
			err := root.ExecuteContext(cli.WithLookupEnv(cli.WithLineage(t.Context(), tc.lineage), e.lookup(env)))
			got := oneJSONObject(t, out.String())
			if tc.sent == "" {
				require.Error(t, err, "a refusal exits 1")
				var refusal struct {
					Error struct{ Code, Message string } `json:"error"`
				}
				require.NoError(t, json.Unmarshal(out.Bytes(), &refusal))
				assert.Equal(t, "caller_unverified", refusal.Error.Code)
				assert.Contains(t, refusal.Error.Message, "--no-daemon")
				assert.Empty(t, sent, "the daemon is never asked")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, true, got["ok"])
			require.Len(t, sent, 1)
			assert.JSONEq(t, tc.sent, <-sent)
		})
	}
}

// appServerAlone is the shared app-server's lineage once the TUI that started
// it has exited: the command (5000) -> the app-server (4999) -> init. No
// process in it is w14:p1's shell, so no pane is confirmed (A22).
func appServerAlone() *report.Lineage {
	procs := map[int]report.Stat{5000: {PPID: 4999, Start: 90}, 4999: {PPID: 1, Start: 80}}
	argv := map[int][]string{5000: {"herdr-agentisan", "send", "boss", "hi"}, 4999: {"codex", "app-server", "--listen", "unix://"}}
	return report.ReadLineage(5000, func(pid int) (report.Stat, error) {
		s, ok := procs[pid]
		if !ok {
			return report.Stat{}, errors.New("no such process")
		}
		return s, nil
	}).WithCmdline(func(pid int) ([]string, error) { return argv[pid], nil })
}

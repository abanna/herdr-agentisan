package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// runTurn executes `report turn` with stdin and returns everything the
// command wrote to stdout and stderr together.
func runTurn(ctx context.Context, stdin string) (string, error) {
	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs([]string{"report", "turn"})
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// claudeLineage is testLineage with what each process runs under Claude: the
// hook, the shell Claude ran it through, claude and the pane shell.
func claudeLineage() *report.Lineage {
	argv := map[int][]string{5000: {"herdr-agentisan", "report", "turn"}, 4999: {"sh", "-c", "herdr-agentisan report turn"}, 900: {"claude"}, 600: {"-zsh"}}
	return testLineage().WithCmdline(func(pid int) ([]string, error) { return argv[pid], nil })
}

// TestReportTurnPushesItemAndStage: a Claude Stop hook's payload sets the
// stage its final line states, and a prompt's new item sets the item and
// clears the old stage; neither prints a byte, since Claude adds a prompt
// hook's stdout to the prompt.
func TestReportTurnPushesItemAndStage(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		stdin  string
		tokens map[string]string
		log    string
	}{
		"a Stop":             {stdin: `{"hook_event_name":"Stop","last_assistant_message":"Done.\n\nBOSS: PR #52 open"}`, tokens: map[string]string{"stage": "PR #52 open"}, log: `"stage":"PR #52 open"`},
		"a UserPromptSubmit": {stdin: `{"hook_event_name":"UserPromptSubmit","prompt":"item: NERD-5334\nGo."}`, tokens: map[string]string{"item": "NERD-5334", "stage": ""}, log: `"cleared":true`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, paneHerdr("w14:p1"))
			var logs bytes.Buffer

			out, err := runTurn(cli.WithLineage(reportCtx(t, srv.Path, "w14:p1", &logs), claudeLineage), tc.stdin)
			require.NoError(t, err)
			assert.Empty(t, out)
			assert.Equal(t, []map[string]string{tc.tokens}, stageTokens(t, srv.Requests()))
			assert.Contains(t, logs.String(), `"reported_to":"w14:p1"`)
			assert.Contains(t, logs.String(), tc.log)
		})
	}
}

// TestReportTurnDoesNothing: every refusal exits 0, prints nothing, calls no
// herdr and leaves its reason in the debug log.
func TestReportTurnDoesNothing(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		pane    string
		lineage func() *report.Lineage
		stdin   string
		want    error
	}{
		"outside a pane":            {stdin: `{"hook_event_name":"Stop","last_assistant_message":"BOSS: x"}`, want: report.ErrNotInPane},
		"a turn with no line":       {pane: "w14:p1", stdin: `{"hook_event_name":"Stop","last_assistant_message":"Done."}`, want: report.ErrNothingStated},
		"a subagent's hook":         {pane: "w14:p1", stdin: `{"hook_event_name":"Stop","agent_id":"a1","last_assistant_message":"BOSS: x"}`, want: report.ErrSubagent},
		"run by Codex's app-server": {pane: "w14:p1", lineage: codexLineage(true), stdin: `{"hook_event_name":"Stop","last_assistant_message":"BOSS: x"}`, want: report.ErrCodexDaemon},
		"not a hook payload":        {pane: "w14:p1", stdin: `[]`, want: report.ErrMalformed},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, paneHerdr("w14:p1"))
			var logs bytes.Buffer
			lineage := tc.lineage
			if lineage == nil {
				lineage = claudeLineage
			}

			out, err := runTurn(cli.WithLineage(reportCtx(t, srv.Path, tc.pane, &logs), lineage), tc.stdin)
			require.NoError(t, err)
			assert.Empty(t, out)
			assert.Empty(t, srv.Requests(), "no herdr call")
			assert.Contains(t, logs.String(), tc.want.Error(), "the reason goes to the debug log")
			assert.Contains(t, logs.String(), "report turn: nothing reported")
		})
	}
}

// TestClaudeHookSnippetRunsReportTurn keeps the documented Claude hooks
// (sample and README) runnable: both turn edges run the real `report turn`
// command, asynchronously, under a timeout.
func TestClaudeHookSnippetRunsReportTurn(t *testing.T) {
	t.Parallel()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "claude-hooks.example.json"))
	require.NoError(t, err)

	type handler struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
		Async   bool   `json:"async"`
	}
	type group struct {
		Hooks []handler `json:"hooks"`
	}
	var cfg struct {
		Hooks struct {
			UserPromptSubmit []group `json:"UserPromptSubmit"`
			Stop             []group `json:"Stop"`
		} `json:"hooks"`
	}
	dec := json.NewDecoder(bytes.NewReader(sample))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&cfg))

	for event, groups := range map[string][]group{"UserPromptSubmit": cfg.Hooks.UserPromptSubmit, "Stop": cfg.Hooks.Stop} {
		require.Len(t, groups, 1, event)
		require.Len(t, groups[0].Hooks, 1, event)
		h := groups[0].Hooks[0]
		assert.Equal(t, handler{Type: "command", Command: "herdr-agentisan report turn", Timeout: h.Timeout, Async: true}, h,
			"%s: async, so no stdout reaches Claude's prompt and herdr's latency stays out of the turn", event)
		assert.Positive(t, h.Timeout, event)
	}
	sub, _, err := cli.Root().Find([]string{"report", "turn"})
	require.NoError(t, err)
	assert.Equal(t, "turn", sub.Name())

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	blocks := regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(string(readme), -1)
	require.Len(t, blocks, 1, "README shows the Claude hooks")
	assert.Equal(t, string(sample), blocks[0][1], "README and the sample show the same hooks")
}

// TestReportTurnTakesCodexsPayload: Codex's Stop, with its null final
// message on a turn that ended with none, goes through the same command.
func TestReportTurnTakesCodexsPayload(t *testing.T) {
	t.Parallel()
	for message, tokens := range map[string][]map[string]string{
		strconv.Quote("item: NERD-5334\nSTATUS: waiting on QA"): {{"item": "NERD-5334", "stage": "waiting on QA"}},
		"null": nil,
	} {
		srv := herdrtest.Start(t, paneHerdr("w15:p4"))
		var logs bytes.Buffer
		stdin := `{"session_id":"019a","turn_id":"t1","hook_event_name":"Stop","model":"gpt-5.5","last_assistant_message":` + message + `}`

		out, err := runTurn(cli.WithLineage(reportCtx(t, srv.Path, "w15:p4", &logs), codexLineage(false)), stdin)
		require.NoError(t, err)
		assert.Empty(t, out)
		assert.Equal(t, tokens, stageTokens(t, srv.Requests()), message)
	}
}

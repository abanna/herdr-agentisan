package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// runCodex executes `report codex` with stdin and returns everything the
// command wrote to stdout and stderr together.
func runCodex(ctx context.Context, stdin string) (string, error) {
	root := cli.Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs([]string{"report", "codex"})
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// codexLineage is testLineage with what each process runs: the hook, the
// shell Codex ran it through, an embedded codex, and the pane shell; or,
// with daemon, a Codex app-server in place of the shell.
func codexLineage(daemon bool) func() *report.Lineage {
	argv := map[int][]string{5000: {"herdr-agentisan", "report", "codex"}, 4999: {"sh", "-lc", "herdr-agentisan report codex"}, 900: {"codex", "--no-daemon"}, 600: {"-zsh"}}
	if daemon {
		argv[4999] = []string{"codex", "app-server", "--listen", "unix://"}
	}
	return func() *report.Lineage {
		return testLineage().WithCmdline(func(pid int) ([]string, error) { return argv[pid], nil })
	}
}

// codexHook is a Stop hook payload naming a rollout whose latest token count
// is last tokens of a 258,400-token window.
func codexHook(t *testing.T, last int, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	line := `{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":` +
		strconv.Itoa(last) + `},"model_context_window":258400}}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))
	return `{"session_id":"s","hook_event_name":"Stop",` + extra + `"transcript_path":` + strconv.Quote(path) + `}`
}

func TestReportCodexPushesCtx(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, paneHerdr("w15:p4"))
	var logs bytes.Buffer

	out, err := runCodex(cli.WithLineage(reportCtx(t, srv.Path, "w15:p4", &logs), codexLineage(false)), codexHook(t, 24610, ""))
	require.NoError(t, err)
	assert.Empty(t, out, "a hook's stdout is Codex's to read: it must print nothing")

	reqs := srv.Requests()
	require.Len(t, reqs, 2)
	assert.Equal(t, "pane.report_metadata", reqs[1].Method)
	assert.JSONEq(t, `{"pane_id":"w15:p4","source":"agentisan","tokens":{"ctx":"5"},"ttl_ms":180000}`, string(reqs[1].Params))
	assert.Contains(t, logs.String(), `"ctx":5`)
}

// TestReportCodexDoesNothing: every refusal exits 0, prints nothing, calls
// no herdr and leaves its reason in the debug log.
func TestReportCodexDoesNothing(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pane     string
		noSocket bool
		daemon   bool
		extra    string
		stdin    string
		want     error
	}{
		"outside a pane":               {want: report.ErrNotInPane},
		"HERDR_SOCKET_PATH unset":      {pane: "w15:p4", noSocket: true, want: report.ErrNotInPane},
		"run by the app-server daemon": {pane: "w15:p4", daemon: true, want: report.ErrCodexDaemon},
		"fired inside a subagent":      {pane: "w15:p4", extra: `"agent_id":"a1",`, want: report.ErrSubagent},
		"a payload that is not a hook": {pane: "w15:p4", stdin: `{"transcript_path":7}`, want: report.ErrMalformed},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, paneHerdr("w15:p4"))
			var logs bytes.Buffer
			stdin := tc.stdin
			if stdin == "" {
				stdin = codexHook(t, 24610, tc.extra)
			}

			socket := srv.Path
			if tc.noSocket {
				socket = ""
			}

			out, err := runCodex(cli.WithLineage(reportCtx(t, socket, tc.pane, &logs), codexLineage(tc.daemon)), stdin)
			require.NoError(t, err)
			assert.Empty(t, out)
			assert.Empty(t, srv.Requests(), "no herdr call")
			assert.Contains(t, logs.String(), tc.want.Error(), "the reason goes to the debug log")
		})
	}
}

// TestCodexHookSnippetRunsReportCodex keeps the documented Codex hooks
// (sample and README) runnable: both events run the real `report codex`
// command, asynchronously, under a timeout.
func TestCodexHookSnippetRunsReportCodex(t *testing.T) {
	t.Parallel()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "codex-hooks.example.toml"))
	require.NoError(t, err)

	type handler struct {
		Type    string `toml:"type"`
		Command string `toml:"command"`
		Timeout int    `toml:"timeout"`
		Async   bool   `toml:"async"`
	}
	type group struct {
		Matcher *string   `toml:"matcher"`
		Hooks   []handler `toml:"hooks"`
	}
	var cfg struct {
		Hooks struct {
			PostToolUse []group `toml:"PostToolUse"`
			Stop        []group `toml:"Stop"`
		} `toml:"hooks"`
	}
	dec := toml.NewDecoder(bytes.NewReader(sample))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&cfg))

	require.Len(t, cfg.Hooks.PostToolUse, 1)
	require.NotNil(t, cfg.Hooks.PostToolUse[0].Matcher)
	assert.Equal(t, "*", *cfg.Hooks.PostToolUse[0].Matcher, "every tool, not just the shell")
	require.Len(t, cfg.Hooks.Stop, 1)
	for _, g := range [][]handler{cfg.Hooks.PostToolUse[0].Hooks, cfg.Hooks.Stop[0].Hooks} {
		require.Len(t, g, 1)
		assert.Equal(t, "command", g[0].Type)
		assert.Equal(t, "herdr-agentisan report codex", g[0].Command)
		assert.True(t, g[0].Async, "a Stop hook that is not async fails on any stray stdout")
		assert.Positive(t, g[0].Timeout)
	}

	sub, _, err := cli.Root().Find([]string{"report", "codex"})
	require.NoError(t, err)
	assert.Equal(t, "codex", sub.Name())

	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	var block string
	for _, b := range regexp.MustCompile("(?s)```toml\n(.*?)```").FindAllStringSubmatch(string(readme), -1) {
		if strings.Contains(b[1], "[[hooks.Stop]]") {
			block = b[1]
		}
	}
	require.NotEmpty(t, block, "README shows the Codex hooks")
	_, hooks, ok := strings.Cut(string(sample), "[[hooks.PostToolUse]]")
	require.True(t, ok)
	assert.Equal(t, "[[hooks.PostToolUse]]"+hooks, block, "README and the sample show the same hooks")
}

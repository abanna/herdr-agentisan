package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/fang"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// runStage executes `report stage` with args and returns what the command
// wrote to stdout and to stderr, separately: stdout must stay silent.
func runStage(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	root := cli.Root()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(append([]string{"report", "stage"}, args...))
	err = root.ExecuteContext(ctx)
	return out.String(), errOut.String(), err
}

// stageTokens returns the tokens of every pane.report_metadata request.
func stageTokens(t *testing.T, reqs []herdrtest.Request) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, r := range reqs {
		if r.Method != "pane.report_metadata" {
			continue
		}
		var p struct {
			Tokens map[string]string `json:"tokens"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &p))
		out = append(out, p.Tokens)
	}
	return out
}

// TestReportStagePushesItemAndStage is the caller contract of
// agentisan-skills' herdr_report.py: the argv it runs pushes both tokens to
// the reporting pane with source agentisan and the 24 h TTL, prints nothing
// and exits 0.
func TestReportStagePushesItemAndStage(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, paneHerdr("w14:p1"))
	var logs bytes.Buffer

	stdout, stderr, err := runStage(reportCtx(t, srv.Path, "w14:p1", &logs), "--item=NERD-5253", "--stage=build_test")
	require.NoError(t, err)
	assert.Empty(t, stdout, "a step transition runs this: it must print nothing")
	assert.Empty(t, stderr)

	reqs := srv.Requests()
	require.Len(t, reqs, 2, "check the pane runs this process, then report")
	assert.Equal(t, "pane.process_info", reqs[0].Method)
	assert.Equal(t, "pane.report_metadata", reqs[1].Method)
	assert.JSONEq(t, `{"pane_id":"w14:p1","source":"agentisan","tokens":{"item":"NERD-5253","stage":"build_test"},"ttl_ms":86400000}`, string(reqs[1].Params))
	assert.Contains(t, logs.String(), `"reported_to":"w14:p1"`)
	assert.Contains(t, logs.String(), `"item":"NERD-5253"`)
	assert.Contains(t, logs.String(), `"stage":"build_test"`)
}

// TestReportStageValuesThatLookLikeFlags: the caller passes --item=<v> so a
// value starting with "-" stays a value. herdr_report.py's own test reports a
// stage of "--help"; it must be pushed, not answered with help.
func TestReportStageValuesThatLookLikeFlags(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args        []string
		item, stage string
	}{
		"--item=-x":                        {args: []string{"--item=-x", "--stage=-y"}, item: "-x", stage: "-y"},
		"--stage=--help":                   {args: []string{"--item=GH-891", "--stage=--help"}, item: "GH-891", stage: "--help"},
		"--item=--stage=review":            {args: []string{"--item=--stage=review", "--stage=pr"}, item: "--stage=review", stage: "pr"},
		"--item=-h":                        {args: []string{"--item=-h", "--stage=review"}, item: "-h", stage: "review"},
		"space form with a dashed value":   {args: []string{"--item", "-x", "--stage", "--version"}, item: "-x", stage: "--version"},
		"value with an equals sign":        {args: []string{"--item=a=b", "--stage=review"}, item: "a=b", stage: "review"},
		"stage before item":                {args: []string{"--stage=review", "--item=NERD-5253"}, item: "NERD-5253", stage: "review"},
		"repeated flag: the last one wins": {args: []string{"--item=NERD-1", "--item=NERD-2", "--stage=review"}, item: "NERD-2", stage: "review"},
		"shell metacharacters stay literal": {
			args: []string{"--item=$(id);`id`", "--stage=a|b&c*"}, item: "$(id);`id`", stage: "a|b&c*",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, paneHerdr("w14:p1"))
			var logs bytes.Buffer

			stdout, _, err := runStage(reportCtx(t, srv.Path, "w14:p1", &logs), tc.args...)
			require.NoError(t, err)
			assert.Empty(t, stdout, "no help, no version, nothing")
			assert.Equal(t, []map[string]string{{"item": tc.item, "stage": tc.stage}}, stageTokens(t, srv.Requests()))
		})
	}
}

// TestReportStageSendsValuesAsGiven: a value that passes is sent exactly as
// argv carried it, never normalized, re-cased or cut: herdr stores what the
// caller wrote. NFC and NFD spellings stay distinct values.
func TestReportStageSendsValuesAsGiven(t *testing.T) {
	t.Parallel()

	for name, v := range map[string]string{
		"NFC":                             "caf\u00e9",
		"NFD":                             "cafe\u0301",
		"exactly 80 four-byte characters": strings.Repeat("\U0001d4c1", 80),
		"exactly 80 ASCII":                strings.Repeat("x", 80),
		"CJK":                             "\u5be9\u67fb",
		"case kept":                       "nerd-5253",
		"interior space":                  "build test",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, paneHerdr("w14:p1"))
			var logs bytes.Buffer

			_, _, err := runStage(reportCtx(t, srv.Path, "w14:p1", &logs), "--item="+v, "--stage="+v)
			require.NoError(t, err)
			assert.Equal(t, []map[string]string{{"item": v, "stage": v}}, stageTokens(t, srv.Requests()))
		})
	}
}

// TestReportStageDoesNothing covers every case that must do nothing: exit 0,
// no herdr call and no output. The socket points at herdrtest wherever it is
// set, so "no herdr call" is observed, not assumed.
func TestReportStageDoesNothing(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 81)
	tests := map[string]struct {
		noSocket bool
		pane     string
		setEmpty string // a variable set, but to ""
		args     []string
		want     error
	}{
		"HERDR_SOCKET_PATH set but empty":     {pane: "w1:p1", setEmpty: "HERDR_SOCKET_PATH", args: []string{"--item=NERD-1", "--stage=review"}, want: report.ErrNotInPane},
		"HERDR_PANE_ID set but empty":         {pane: "w1:p1", setEmpty: "HERDR_PANE_ID", args: []string{"--item=NERD-1", "--stage=review"}, want: report.ErrNotInPane},
		"HERDR_SOCKET_PATH unset":             {noSocket: true, pane: "w1:p1", args: []string{"--item=NERD-1", "--stage=review"}, want: report.ErrNotInPane},
		"HERDR_PANE_ID unset":                 {args: []string{"--item=NERD-1", "--stage=review"}, want: report.ErrNotInPane},
		"both unset":                          {noSocket: true, args: []string{"--item=NERD-1", "--stage=review"}, want: report.ErrNotInPane},
		"--item missing":                      {pane: "w1:p1", args: []string{"--stage=review"}, want: report.ErrInvalidValue},
		"--stage missing":                     {pane: "w1:p1", args: []string{"--item=NERD-1"}, want: report.ErrInvalidValue},
		"both flags missing":                  {pane: "w1:p1", want: report.ErrInvalidValue},
		"empty item":                          {pane: "w1:p1", args: []string{"--item=", "--stage=review"}, want: report.ErrInvalidValue},
		"empty stage":                         {pane: "w1:p1", args: []string{"--item=NERD-1", "--stage="}, want: report.ErrInvalidValue},
		"blank stage":                         {pane: "w1:p1", args: []string{"--item=NERD-1", "--stage=   "}, want: report.ErrInvalidValue},
		"81-character item":                   {pane: "w1:p1", args: []string{"--item=" + long, "--stage=review"}, want: report.ErrInvalidValue},
		"81-character stage":                  {pane: "w1:p1", args: []string{"--item=NERD-1", "--stage=" + long}, want: report.ErrInvalidValue},
		"newline in item":                     {pane: "w1:p1", args: []string{"--item=NERD-1\nNERD-2", "--stage=review"}, want: report.ErrInvalidValue},
		"terminal escape in stage":            {pane: "w1:p1", args: []string{"--item=NERD-1", "--stage=\x1b[2Jreview"}, want: report.ErrInvalidValue},
		"NUL in item":                         {pane: "w1:p1", args: []string{"--item=NERD\x001", "--stage=review"}, want: report.ErrInvalidValue},
		"non-UTF-8 argv":                      {pane: "w1:p1", args: []string{"--item=NERD-\xff", "--stage=review"}, want: report.ErrInvalidValue},
		"trailing CRLF from a Windows editor": {pane: "w1:p1", args: []string{"--item=NERD-1", "--stage=review\r\n"}, want: report.ErrInvalidValue},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
			})
			socket := srv.Path
			if tc.noSocket {
				socket = ""
			}
			var logs bytes.Buffer
			ctx := reportCtx(t, socket, tc.pane, &logs)
			if tc.setEmpty != "" {
				env := map[string]string{"HERDR_SOCKET_PATH": socket, "HERDR_PANE_ID": tc.pane, tc.setEmpty: ""}
				ctx = cli.WithLookupEnv(ctx, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
			}

			stdout, stderr, err := runStage(ctx, tc.args...)
			require.NoError(t, err, "a no-op still exits 0")
			assert.Empty(t, stdout)
			assert.Empty(t, stderr)
			assert.Empty(t, srv.Requests(), "no herdr call")
			assert.Contains(t, logs.String(), tc.want.Error(), "the reason goes to the debug log")
			assert.Contains(t, logs.String(), "report stage: nothing reported")
		})
	}
}

// TestReportStageRejectsArguments: an argv no caller can mean is a usage
// error, as it is for `report statusline` (TestReportRejectsArguments). It
// fails rather than exiting 0, so agentisan never counts it as reported, and
// it still makes no herdr call and writes nothing to stdout.
func TestReportStageRejectsArguments(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"unknown flag":                {"--item=NERD-1", "--stage=review", "--bogus"},
		"unknown flag with a value":   {"--item=NERD-1", "--stage=review", "--ttl=5"},
		"unknown shorthand":           {"--item=NERD-1", "--stage=review", "-x"},
		"positional argument":         {"--item=NERD-1", "--stage=review", "extra"},
		"operand after --":            {"--item=NERD-1", "--stage=review", "--", "--json"},
		"flag with no value":          {"--stage=review", "--item"},
		"single-dash long flag":       {"-item=NERD-1", "-stage=review"},
		"case variant of a flag name": {"--Item=NERD-1", "--stage=review"},
		"non-ASCII flag name":         {"--\u00edtem=NERD-1", "--stage=review"},
		"non-UTF-8 flag name":         {"--item\xff=NERD-1", "--stage=review"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, paneHerdr("w1:p1"))
			var logs bytes.Buffer

			stdout, _, err := runStage(reportCtx(t, srv.Path, "w1:p1", &logs), args...)
			require.Error(t, err)
			assert.Empty(t, stdout)
			assert.Empty(t, srv.Requests(), "a rejected invocation must not reach herdr")
		})
	}
}

// TestReportStageSwallowsHerdrErrors: a herdr failure, or a pane that cannot
// be proved ours, is logged at debug level and nowhere else, and the command
// still exits 0.
func TestReportStageSwallowsHerdrErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		handler herdrtest.Handler
		closed  bool // nothing listens at HERDR_SOCKET_PATH any more
		want    string
	}{
		"no herdr at the socket path": {handler: paneHerdr("w1:p1"), closed: true, want: "herdr is unavailable"},
		"herdr errors": {
			handler: func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "busy", Message: "try later"}}
			},
			want: "busy",
		},
		"herdr drops the connection": {
			handler: func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} },
			want:    "herdr is unavailable",
		},
		"no pane runs the process": {
			handler: func(r herdrtest.Request) herdrtest.Reply {
				if r.Method == "pane.list" {
					return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []map[string]any{}}}
				}
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane w1:p1 not found"}}
			},
			want: report.ErrPaneUnresolved.Error(),
		},
		"the report itself fails": {
			handler: func(r herdrtest.Request) herdrtest.Reply {
				if r.Method == "pane.report_metadata" {
					return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "metadata_token_limit", Message: "pane metadata may contain at most 32 tokens"}}
				}
				return paneHerdr("w1:p1")(r)
			},
			want: "metadata_token_limit",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)
			if tc.closed {
				srv.Close()
			}
			var logs bytes.Buffer

			stdout, stderr, err := runStage(reportCtx(t, srv.Path, "w1:p1", &logs), "--item=NERD-1", "--stage=review")
			require.NoError(t, err)
			assert.Empty(t, stdout)
			assert.Empty(t, stderr)
			assert.Equal(t, tc.closed, len(srv.Requests()) == 0, "herdr is asked whenever it listens")
			assert.Contains(t, logs.String(), tc.want)
			assert.Contains(t, logs.String(), "report stage: nothing reported")
		})
	}
}

// TestReportStageLogsWhereAStalePaneReported: a stale HERDR_PANE_ID never
// receives the tokens. They land on the pane the process runs in, and the
// debug log names both.
func TestReportStageLogsWhereAStalePaneReported(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, paneHerdr("wN:p2"))
	var logs bytes.Buffer

	stdout, _, err := runStage(reportCtx(t, srv.Path, "wP:p1", &logs), "--item=NERD-5253", "--stage=review")
	require.NoError(t, err)
	assert.Empty(t, stdout)

	var reported []string
	for _, r := range srv.Requests() {
		if r.Method == "pane.report_metadata" {
			reported = append(reported, string(r.Params))
		}
	}
	require.Len(t, reported, 1)
	assert.Contains(t, reported[0], `"pane_id":"wN:p2"`)
	assert.Contains(t, logs.String(), `"pane":"wP:p1"`)
	assert.Contains(t, logs.String(), `"reported_to":"wN:p2"`)
}

// TestReportStageReadsTheLineageOnce: like statusline, the lineage is read
// once, before herdr is asked anything.
func TestReportStageReadsTheLineageOnce(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, paneHerdr("w14:p1"))
	var logs bytes.Buffer
	reads := 0
	ctx := cli.WithLineage(reportCtx(t, srv.Path, "w14:p1", &logs), func() *report.Lineage {
		reads++
		assert.Empty(t, srv.Requests(), "the lineage must be read before any herdr call")
		return testLineage()
	})

	_, _, err := runStage(ctx, "--item=NERD-1", "--stage=review")
	require.NoError(t, err)
	assert.Equal(t, 1, reads)
	assert.Len(t, srv.Requests(), 2)
}

// TestReportStageFromACodexPane runs the command against the real process
// table with no CLAUDE_PID, as in a Codex pane. The fake pane's shell is this
// test binary's parent, so the report reaches its pane only by walking up
// through its parents: no anchor helps it. Off Linux there is no lineage and
// the report trusts HERDR_PANE_ID.
func TestReportStageFromACodexPane(t *testing.T) {
	t.Parallel()
	parent := os.Getppid()
	srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
		if r.Method == "pane.process_info" {
			return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{
				"pane_id": "w3:p2", "shell_pid": parent, "foreground_process_group_id": nil,
			}}}
		}
		return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
	})
	var logs bytes.Buffer
	// reportCtx sets no CLAUDE_PID; a nil reader means the real lineage.
	ctx := cli.WithLineage(reportCtx(t, srv.Path, "w3:p2", &logs), nil)

	_, _, err := runStage(ctx, "--item=NERD-5253", "--stage=build_test")
	require.NoError(t, err)

	want := []string{"pane.process_info", "pane.report_metadata"}
	if runtime.GOOS != "linux" {
		want = []string{"pane.report_metadata"}
	}
	var got []string
	for _, r := range srv.Requests() {
		got = append(got, r.Method)
	}
	assert.Equal(t, want, got)
	assert.Contains(t, logs.String(), `"reported_to":"w3:p2"`)
}

// TestReportStageSurvivesACd: the tokens belong to the pane, not to a
// directory. Reports made from two different working directories go to the
// same pane with the same values, and the request carries nothing about the
// working directory for herdr to key them by. t.Chdir changes the process's
// directory, so this test cannot run in parallel.
func TestReportStageSurvivesACd(t *testing.T) {
	srv := herdrtest.Start(t, paneHerdr("w14:p1"))
	var logs bytes.Buffer
	ctx := reportCtx(t, srv.Path, "w14:p1", &logs)

	var dirs []string
	for _, dir := range []string{t.TempDir(), t.TempDir()} {
		t.Chdir(dir)
		wd, err := os.Getwd()
		require.NoError(t, err)
		dirs = append(dirs, wd)
		_, _, err = runStage(ctx, "--item=NERD-5253", "--stage=review")
		require.NoError(t, err)
	}
	require.NotEqual(t, dirs[0], dirs[1], "the two reports ran in different directories")

	var params []string
	for _, r := range srv.Requests() {
		if r.Method != "pane.report_metadata" {
			continue
		}
		params = append(params, string(r.Params))
		var p map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(r.Params, &p))
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		assert.Equal(t, []string{"pane_id", "source", "tokens", "ttl_ms"}, keys, "nothing but the pane keys the tokens")
	}
	require.Len(t, params, 2)
	assert.JSONEq(t, `{"pane_id":"w14:p1","source":"agentisan","tokens":{"item":"NERD-5253","stage":"review"},"ttl_ms":86400000}`, params[0])
	assert.Equal(t, params[0], params[1], "the report made after the cd is the same report")
}

// TestReportStageHelpProbe is agentisan-skills' support probe (herdr.py
// supports): `report stage --help` must exit 0 and print, on stdout, a line
// whose first three words are exactly "herdr-agentisan report stage". It runs
// through fang with main's options, because fang replaces cobra's help, and
// with stdout a buffer, as the probe captures it through a pipe. fang reads
// the colour profile from the process environment, so the environment is set
// to a herdr pane's: a truecolor terminal declared, and neither variable that
// forces colour into a pipe (CLICOLOR_FORCE=1 would, and the probe would then
// fail on the escapes; herdr sets neither). That rules out t.Parallel.
func TestReportStageHelpProbe(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("TTY_FORCE", "")

	probe := func(t *testing.T, stdout string) {
		t.Helper()
		var found bool
		for line := range strings.Lines(stdout) {
			if f := strings.Fields(line); len(f) >= 3 && slices.Equal(f[:3], []string{"herdr-agentisan", "report", "stage"}) {
				found = true
			}
		}
		assert.True(t, found, "no line starts with the words herdr-agentisan report stage:\n%s", stdout)
		assert.NotContains(t, stdout, "\x1b[", "piped help carries no ANSI escapes")
	}

	run := func(t *testing.T, useFang bool, args ...string) (string, string) {
		t.Helper()
		srv := herdrtest.Start(t, paneHerdr("w1:p1"))
		var logs bytes.Buffer
		root := cli.Root()
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)
		ctx := reportCtx(t, srv.Path, "w1:p1", &logs)
		if useFang {
			require.NoError(t, fang.Execute(ctx, root, fang.WithVersion(config.Version), fang.WithCommit(config.Commit)))
		} else {
			require.NoError(t, root.ExecuteContext(ctx))
		}
		assert.Empty(t, srv.Requests(), "help never reaches herdr")
		return stdout.String(), stderr.String()
	}

	t.Run("fang, as the binary runs", func(t *testing.T) {
		stdout, stderr := run(t, true, "report", "stage", "--help")
		probe(t, stdout)
		assert.Empty(t, stderr)
	})
	t.Run("fang, -h", func(t *testing.T) {
		stdout, _ := run(t, true, "report", "stage", "-h")
		probe(t, stdout)
	})
	t.Run("cobra alone", func(t *testing.T) {
		stdout, _ := run(t, false, "report", "stage", "--help")
		probe(t, stdout)
	})
	// The probe exists because a binary without the subcommand prints its
	// parent's help and exits 0; that help must not pass for this one.
	t.Run("a missing subcommand does not pass the probe", func(t *testing.T) {
		stdout, _ := run(t, true, "report", "--help")
		for line := range strings.Lines(stdout) {
			f := strings.Fields(line)
			assert.False(t, len(f) >= 3 && slices.Equal(f[:3], []string{"herdr-agentisan", "report", "stage"}),
				"the report group's help must not look like stage's: %q", line)
		}
	})
}

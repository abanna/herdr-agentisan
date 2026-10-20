package report_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// stageParams decodes one pane.report_metadata request as herdr reads it.
type stageParams struct {
	PaneID string            `json:"pane_id"`
	Source string            `json:"source"`
	Tokens map[string]string `json:"tokens"`
	TTLMs  uint64            `json:"ttl_ms"`
}

// TestStagePushesTheTokenContract is the token contract for item and stage
// (ADR-001 A5): both keys in one report, source agentisan, herdr's maximum
// TTL of 86,400,000 ms, on the reporting pane only. The pane has no lineage,
// as on an OS without /proc, so the report trusts HERDR_PANE_ID and costs
// one call.
func TestStagePushesTheTokenContract(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, okReply)
	pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}

	got, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, "NERD-5253", "build_test")
	require.NoError(t, err)
	assert.Equal(t, report.StageResult{Item: "NERD-5253", Stage: "build_test", PaneID: "w14:p1"}, got)

	reqs := srv.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "pane.report_metadata", reqs[0].Method)
	assert.JSONEq(t, `{"pane_id":"w14:p1","source":"agentisan","tokens":{"item":"NERD-5253","stage":"build_test"},"ttl_ms":86400000}`, string(reqs[0].Params))
	assert.Equal(t, uint64(86_400_000), uint64(report.StageTTL/time.Millisecond), "herdr's maximum TTL")
}

// TestStageSetsTheTTLAgainOnEveryCall: every report carries the full 24 h, so
// a pipeline that keeps stepping keeps its tokens, and one that stops loses
// them a day after its last step.
func TestStageSetsTheTTLAgainOnEveryCall(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, okReply)
	pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1"}

	for _, stage := range []string{"build_test", "review", "review"} {
		_, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, "NERD-5253", stage)
		require.NoError(t, err)
	}

	reqs := srv.Requests()
	require.Len(t, reqs, 3)
	for i, r := range reqs {
		var p stageParams
		require.NoError(t, json.Unmarshal(r.Params, &p))
		assert.Equal(t, uint64(86_400_000), p.TTLMs, "report %d", i)
	}
}

// TestCheckValueClasses walks the values an item or stage can hold. A value is
// pushed only if herdr would store it exactly as sent: herdr trims it, drops
// control characters, cuts it at 80 characters and turns a blank one into a
// clear, so each of those is refused here rather than changed there.
// Unprintable characters herdr would keep (format characters such as a bidi
// override, separators, non-ASCII spaces) are refused too: a sidebar token is
// drawn into every client's terminal.
func TestCheckValueClasses(t *testing.T) {
	t.Parallel()

	accepted := map[string]string{
		"ticket key":                      "NERD-5253",
		"step name":                       "build_test",
		"GitHub key":                      "GH-891",
		"leading dash":                    "-x",
		"looks like --help":               "--help",
		"looks like a flag with a value":  "--stage=review",
		"interior space":                  "build test",
		"shell metacharacters":            "a;b|c&d*e?$(f)`g`'h'\"i\"",
		"secret-looking":                  "password=hunter2",
		"non-ASCII letters":               "étape-révision",
		"CJK":                             "審査",
		"emoji with a skin-tone modifier": "👍🏽",
		"NFC é":                           "caf\u00e9",
		"NFD e and combining acute":       "cafe\u0301",
		"exactly 80 ASCII":                strings.Repeat("x", 80),
		"exactly 80 two-byte characters":  strings.Repeat("é", 80),
		"exactly 80 CJK characters":       strings.Repeat("審", 80),
		"exactly 80 four-byte characters": strings.Repeat("𝔁", 80),
		"80 code points of 40 NFD pairs":  strings.Repeat("e\u0301", 40),
		"one character":                   "x",
		"CON, a Windows-reserved name":    "CON",
		"case variant of a key":           "nerd-5253",
	}
	for name, v := range accepted {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.NoError(t, report.CheckValue(v))
		})
	}

	refused := map[string]string{
		"empty":                          "",
		"one space":                      " ",
		"whitespace only":                " \t\n",
		"leading space":                  " NERD-1",
		"trailing space":                 "NERD-1 ",
		"trailing newline":               "review\n",
		"trailing CRLF":                  "review\r\n",
		"lone CR":                        "re\rview",
		"embedded LF":                    "re\nview",
		"tab inside":                     "re\tview",
		"NUL inside":                     "re\x00view",
		"DEL":                            "re\x7fview",
		"ESC sequence":                   "\x1b[31mred\x1b[0m",
		"C1 NEL":                         "re\u0085view",
		"bidi override":                  "re\u202eview",
		"zero-width space":               "re\u200bview",
		"zero-width joiner":              "👨\u200d👩",
		"byte-order mark":                "\ufeffreview",
		"line separator":                 "re\u2028view",
		"no-break space":                 "re\u00a0view",
		"ideographic space":              "re\u3000view",
		"private use":                    "re\ue000view",
		"81 ASCII":                       strings.Repeat("x", 81),
		"81 two-byte characters":         strings.Repeat("é", 81),
		"81 four-byte characters":        strings.Repeat("𝔁", 81),
		"82 code points of 41 NFD pairs": strings.Repeat("e\u0301", 41),
		"128 KiB":                        strings.Repeat("x", 128<<10),
		"invalid UTF-8":                  "re\xffview",
		"truncated multibyte":            "review\xe2\x82",
		"UTF-8-encoded lone surrogate":   "re\xed\xa0\x80view",
		"UTF-16LE bytes":                 "r\x00e\x00",
		"Latin-1 bytes":                  "caf\xe9",
		"mixed valid and invalid":        "étape\xe9",
	}
	for name, v := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, report.CheckValue(v), report.ErrInvalidValue)
		})
	}
}

// TestCheckValueNeverEchoesTheValue: the reason goes to the debug log, so it
// names what is wrong and never repeats the value, which may be 128 KiB or
// carry terminal escapes.
func TestCheckValueNeverEchoesTheValue(t *testing.T) {
	t.Parallel()

	for name, v := range map[string]string{
		"too long":      strings.Repeat("secretsecret", 10),
		"escape":        "\x1b]0;pwned\x07",
		"invalid UTF-8": "pwned\xff",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := report.CheckValue(v)
			require.ErrorIs(t, err, report.ErrInvalidValue)
			assert.NotContains(t, err.Error(), "secret")
			assert.NotContains(t, err.Error(), "pwned")
		})
	}
}

// TestStageDoesNothingOutsideAPane: without HERDR_SOCKET_PATH or
// HERDR_PANE_ID there is no pane to report to, so herdr is never called,
// whatever the values.
func TestStageDoesNothingOutsideAPane(t *testing.T) {
	t.Parallel()

	for name, pane := range map[string]func(sock string) report.Pane{
		"socket unset": func(string) report.Pane { return report.Pane{PaneID: "w1:p1"} },
		"pane unset":   func(sock string) report.Pane { return report.Pane{SocketPath: sock} },
		"both unset":   func(string) report.Pane { return report.Pane{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)

			_, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, pane(srv.Path), "NERD-5253", "review")
			require.ErrorIs(t, err, report.ErrNotInPane)
			assert.Empty(t, srv.Requests(), "no herdr call outside a pane")
		})
	}
}

// TestStageRefusesInvalidValues: a value herdr would change is refused before
// any herdr call, the pane lookup included, and the error names the key.
func TestStageRefusesInvalidValues(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		item, stage string
		key         string
	}{
		"empty item":              {item: "", stage: "review", key: "item"},
		"empty stage":             {item: "NERD-5253", stage: "", key: "stage"},
		"81-character item":       {item: strings.Repeat("x", 81), stage: "review", key: "item"},
		"81-character stage":      {item: "NERD-5253", stage: strings.Repeat("x", 81), key: "stage"},
		"control character item":  {item: "NERD-5253\n", stage: "review", key: "item"},
		"control character stage": {item: "NERD-5253", stage: "re\x1bview", key: "stage"},
		"blank stage":             {item: "NERD-5253", stage: "  ", key: "stage"},
		"invalid UTF-8 item":      {item: "NERD-\xff", stage: "review", key: "item"},
		"invalid UTF-8 stage":     {item: "NERD-5253", stage: "re\xe2\x82", key: "stage"},
		"NUL in stage":            {item: "NERD-5253", stage: "re\x00view", key: "stage"},
		"both invalid":            {item: "", stage: "", key: "item"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w1:p1", Lineage: ours()}

			_, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, tc.item, tc.stage)
			require.ErrorIs(t, err, report.ErrInvalidValue)
			assert.True(t, strings.HasPrefix(err.Error(), tc.key+": "), "error %q names the key %q", err, tc.key)
			assert.Empty(t, srv.Requests(), "no herdr call for an invalid value")
		})
	}
}

// TestStageResolvesTheReportingPane is NERD-5268's resolution, reused: item
// and stage land on the pane the reporting process runs in, whatever
// HERDR_PANE_ID says, and nowhere when no pane provably runs it.
func TestStageResolvesTheReportingPane(t *testing.T) {
	t.Parallel()

	mine := fakePane{id: "w14:p1", shell: 600, pgid: 900, fg: []uint32{900, 901}}
	other := fakePane{id: "w2:p1", shell: 700, pgid: 701, fg: []uint32{701}}

	tests := map[string]struct {
		envPane   string
		noLineage bool
		table     map[int]report.Stat // oursProcs when nil
		anchors   []int
		mutate    func(*procTable) // changes the table after the lineage was read
		panes     []fakePane
		want      string // "" means nothing is reported
		err       error
		calls     []string
	}{
		"valid id costs exactly two calls": {
			envPane: "w14:p1", panes: []fakePane{mine, other},
			want: "w14:p1", calls: []string{process, reportM},
		},
		"stale id finds the moved pane": {
			envPane: "wP:p1", panes: []fakePane{other, {id: "wN:p2", shell: 600, pgid: 900}},
			want: "wN:p2", calls: []string{process, list, process, process, reportM},
		},
		"stale id through herdr's alias reports to the current id": {
			envPane: "wP:p1", panes: []fakePane{other, {id: "wN:p2", aliases: []string{"wP:p1"}, shell: 600}},
			want: "wN:p2", calls: []string{process, reportM},
		},
		"stale id reissued to another pane never gets the report": {
			envPane: "wZ:p1", panes: []fakePane{{id: "wZ:p1", shell: 700, pgid: 701}, {id: "wN:p2", shell: 600}},
			want: "wN:p2", calls: []string{process, list, process, process, reportM},
		},
		"reissued id and our pane gone: nothing is written": {
			envPane: "wZ:p1", panes: []fakePane{{id: "wZ:p1", shell: 700, pgid: 701}},
			err: report.ErrPaneUnresolved, calls: []string{process, list, process},
		},
		"two panes claiming the process get nothing": {
			envPane: "wP:p1", panes: []fakePane{{id: "wN:p2", shell: 600}, {id: "wM:p3", fg: []uint32{900}}},
			err: report.ErrAmbiguousPane, calls: []string{process, list, process, process},
		},
		"no lineage trusts HERDR_PANE_ID": {
			envPane: "w14:p1", noLineage: true, panes: []fakePane{mine, other},
			want: "w14:p1", calls: []string{reportM},
		},
		// In a Claude pane CLAUDE_PID names claude, which still reaches the
		// pane when the reporter's own parents no longer do.
		"re-parented, anchored by CLAUDE_PID": {
			envPane: "w14:p1", table: map[int]report.Stat{5000: {PPID: 1, Start: 900}, 900: {PPID: 600, Start: 500}},
			anchors: []int{900}, panes: []fakePane{mine, other},
			want: "w14:p1", calls: []string{process, reportM},
		},
		// claude (900) exited after the lineage was read and another pane's
		// new process took its pid: the pid matches, its start time does not.
		"a pid reused since the lineage was read is not ours": {
			envPane: "w14:p1", mutate: func(p *procTable) { p.set(900, report.Stat{PPID: 7000, Start: 990}) },
			panes: []fakePane{{id: "w2:p1", fg: []uint32{900}}},
			err:   report.ErrPaneUnresolved, calls: []string{process, list, process},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr(tc.panes))
			pane := report.Pane{SocketPath: srv.Path, PaneID: tc.envPane}
			if !tc.noLineage {
				procs := tc.table
				if procs == nil {
					procs = oursProcs()
				}
				table := newProcTable(procs)
				pane.Lineage = report.ReadLineage(5000, table.stat, tc.anchors...)
				if tc.mutate != nil {
					tc.mutate(table)
				}
			}

			got, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, "NERD-5253", "review")
			reqs := srv.Requests()
			assert.Equal(t, tc.calls, methods(reqs))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Empty(t, reportedTo(t, reqs), "nothing may be reported to a pane that is not provably ours")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, report.StageResult{Item: "NERD-5253", Stage: "review", PaneID: tc.want}, got)
			assert.Equal(t, []string{tc.want}, reportedTo(t, reqs))
		})
	}
}

// codexProcs is a Codex pane's process table: report stage (5000), run by
// agentisan's python (4990) with start_new_session, so it leads its own
// session and process group; uv (4980); the shell Codex ran the step in
// (4970); codex (800), the pane's foreground job; the pane shell (600); herdr
// (100). Codex sets no CLAUDE_PID, so nothing anchors the lineage: it is the
// parent walk alone.
func codexProcs() map[int]report.Stat {
	return map[int]report.Stat{
		5000: {PPID: 4990, Start: 950}, 4990: {PPID: 4980, Start: 940}, 4980: {PPID: 4970, Start: 930},
		4970: {PPID: 800, Start: 920}, 800: {PPID: 600, Start: 500}, 600: {PPID: 100, Start: 400}, 100: {PPID: 1, Start: 100},
	}
}

// codexArgv is what codexProcs' processes run: an embedded Codex TUI
// (codex --no-daemon) at 800, so the host check passes (A25).
var codexArgv = map[int][]string{
	5000: {"herdr-agentisan", "report", "stage", "--item=NERD-5253", "--stage=build_test"}, 4990: {"python3", "agentisan_workflow.py"},
	4980: {"uv", "run"}, 4970: {"/bin/zsh", "-lc", "python3 agentisan_workflow.py"}, 800: {"/opt/codex/vendor/codex", "--no-daemon"},
	600: {"-zsh"}, 100: {"herdr", "server"},
}

// TestStageFromACodexPane: Codex sets no CLAUDE_PID, and the reporter runs in
// a session of its own, so no pane names it in its foreground job. The parent
// walk still reaches codex and the pane shell, because agentisan waits for
// the report and every ancestor is alive while it runs. The report carries
// CODEX_THREAD_ID, so it reports only once the host check has found the
// pane's own Codex among those ancestors (A25).
func TestStageFromACodexPane(t *testing.T) {
	t.Parallel()

	anchors := report.AnchorsFrom(func(string) (string, bool) { return "", false })
	require.Empty(t, anchors, "a Codex pane has no CLAUDE_PID, so there is no anchor")

	codexPane := fakePane{id: "w3:p2", shell: 600, pgid: 800, fg: []uint32{800}}
	other := fakePane{id: "w2:p1", shell: 700, pgid: 701, fg: []uint32{701}}

	tests := map[string]struct {
		envPane string
		table   func() map[int]report.Stat
		panes   []fakePane
		want    string
		err     error
		calls   []string
	}{
		"codex pane, valid id": {
			envPane: "w3:p2", table: codexProcs, panes: []fakePane{other, codexPane},
			want: "w3:p2", calls: []string{process, reportM},
		},
		"codex pane matched through codex alone": {
			envPane: "w3:p2", table: codexProcs, panes: []fakePane{{id: "w3:p2", pgid: 800}},
			want: "w3:p2", calls: []string{process, reportM},
		},
		"codex pane matched through the pane shell alone": {
			envPane: "w3:p2", table: codexProcs, panes: []fakePane{{id: "w3:p2", shell: 600, pgid: 777}},
			want: "w3:p2", calls: []string{process, reportM},
		},
		"codex pane, stale id": {
			envPane: "wP:p1", table: codexProcs, panes: []fakePane{other, {id: "wN:p4", shell: 600, pgid: 800}},
			want: "wN:p4", calls: []string{process, list, process, process, reportM},
		},
		"codex pane, id reissued to another pane": {
			envPane: "w2:p1", table: codexProcs, panes: []fakePane{other, codexPane},
			want: "w3:p2", calls: []string{process, list, process, process, reportM},
		},
		// Were agentisan to exit before the report read its parents, the
		// reporter would be re-parented to init with no anchor to reach the
		// pane, and no codex among its ancestors: it reports nowhere, and
		// asks herdr nothing, rather than trusting the id.
		"codex reporter orphaned before reading its lineage": {
			envPane: "w3:p2",
			table:   func() map[int]report.Stat { return map[int]report.Stat{5000: {PPID: 1, Start: 950}} },
			panes:   []fakePane{codexPane}, err: report.ErrNoCodexHost, calls: []string{},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr(tc.panes))
			lineage := report.ReadLineage(5000, newProcTable(tc.table()).stat, anchors...).WithCmdline(argv(codexArgv))
			pane := report.Pane{SocketPath: srv.Path, PaneID: tc.envPane, Lineage: lineage, CodexThread: "019a"}

			got, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, "NERD-5253", "build_test")
			reqs := srv.Requests()
			assert.Equal(t, tc.calls, methods(reqs))
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Empty(t, reportedTo(t, reqs))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.PaneID)
			assert.Equal(t, []string{tc.want}, reportedTo(t, reqs))
		})
	}
}

// TestStageBoundsTheCallByCallTimeout: one herdr.CallTimeout bounds the whole
// report, the pane lookup included, so a herdr that never answers cannot keep
// a step transition waiting.
func TestStageBoundsTheCallByCallTimeout(t *testing.T) {
	t.Parallel()

	rep := &deadlineReporter{}
	lineage := report.ReadLineage(5000, newProcTable(map[int]report.Stat{5000: {PPID: 600, Start: 9}, 600: {PPID: 1, Start: 5}}).stat)
	start := time.Now()
	got, err := report.Stage(context.Background(), rep,
		report.Pane{SocketPath: "/x", PaneID: "w1:p1", Lineage: lineage}, "NERD-5253", "review")
	require.NoError(t, err)
	assert.Equal(t, "w2:p1", got.PaneID)

	require.Len(t, rep.deadlines, 4, "process_info, list, process_info, report")
	for i, d := range rep.deadlines {
		require.False(t, d.IsZero(), "call %d must run under a deadline", i)
		assert.Equal(t, rep.deadlines[0], d, "call %d must share the one report deadline", i)
	}
	assert.WithinDuration(t, start.Add(herdr.CallTimeout), rep.deadlines[0], time.Second)
}

// TestStageSurfacesHerdrErrors: a herdr failure comes back wrapped, with its
// sentinel, for the command to log.
func TestStageSurfacesHerdrErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		reply herdrtest.Reply
		want  error
	}{
		"herdr answers an error":  {reply: herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "busy", Message: "try later"}}, want: herdr.ErrAPI},
		"herdr drops the request": {reply: herdrtest.Reply{Silent: true}, want: herdr.ErrUnavailable},
		"herdr rejects the TTL":   {reply: herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "invalid_metadata_ttl", Message: "metadata ttl_ms must be 86400000 or less"}}, want: herdr.ErrAPI},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return tc.reply })

			_, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path},
				report.Pane{SocketPath: srv.Path, PaneID: "w1:p1"}, "NERD-5253", "review")
			require.ErrorIs(t, err, tc.want)
		})
	}
	t.Run("no herdr listening", func(t *testing.T) {
		t.Parallel()
		srv := herdrtest.Start(t, okReply)
		srv.Close()

		_, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path},
			report.Pane{SocketPath: srv.Path, PaneID: "w1:p1"}, "NERD-5253", "review")
		require.ErrorIs(t, err, herdr.ErrUnavailable)
	})
}

// TestStageConcurrentReportsAreIndependent: two step transitions can overlap
// (a step closes as the next opens). Each arrives as its own complete request
// carrying both of its own values.
func TestStageConcurrentReportsAreIndependent(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, fakeHerdr([]fakePane{{id: "w2:p1", shell: 700}, {id: "wN:p2", shell: 600}}))

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			pane := report.Pane{SocketPath: srv.Path, PaneID: "wP:p1", Lineage: ours()}
			got, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, pane,
				"NERD-"+strconv.Itoa(i), "step_"+strconv.Itoa(i))
			assert.NoError(t, err)
			assert.Equal(t, "wN:p2", got.PaneID)
		})
	}
	wg.Wait()

	pairs := map[string]bool{}
	for _, r := range srv.Requests() {
		if r.Method != reportM {
			continue
		}
		var p stageParams
		require.NoError(t, json.Unmarshal(r.Params, &p))
		assert.Equal(t, "wN:p2", p.PaneID)
		require.True(t, strings.HasPrefix(p.Tokens["item"], "NERD-"))
		assert.Equal(t, "step_"+strings.TrimPrefix(p.Tokens["item"], "NERD-"), p.Tokens["stage"], "a report never mixes two transitions")
		pairs[p.Tokens["item"]] = true
	}
	assert.Len(t, pairs, n, "every report carries its own values")
}

// errReporter fails every call with err; it checks Stage wraps what the
// Reporter returns rather than replacing it.
type errReporter struct{ err error }

func (r errReporter) ReportPaneMetadata(context.Context, herdr.PaneMetadata) error { return r.err }
func (r errReporter) ListPanes(context.Context) ([]herdr.PaneInfo, error)          { return nil, r.err }

func (r errReporter) PaneProcessInfo(context.Context, string) (herdr.ProcessInfo, error) {
	return herdr.ProcessInfo{}, r.err
}

func TestStageWrapsTheReportError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	_, err := report.Stage(t.Context(), errReporter{err: boom}, report.Pane{SocketPath: "/x", PaneID: "w1:p1"}, "NERD-5253", "review")
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "w1:p1")
}

// TestStageUnderCodexRefuses: a report run by a Codex tool command lands on
// its own Codex's pane or nowhere (A25). Under the shared app-server daemon
// it inherits the daemon's HERDR_PANE_ID, w9:p1 here, the pane whose TUI
// started the daemon; the walk even reaches that pane through the TUI. In
// Codex's network sandbox seccomp denies connect; in its pid namespace the
// walk sees no codex at all. Each refuses before asking herdr anything.
// Without CODEX_THREAD_ID nothing changes: no lineage still trusts the id.
func TestStageUnderCodexRefuses(t *testing.T) {
	t.Parallel()

	// report (5000) -> python (4990) -> zsh (4970) -> app-server (4000) ->
	// pid-update-loop (3000) -> the TUI that started it (900) -> w9:p1's shell (700).
	daemon := map[int]report.Stat{
		5000: {PPID: 4990, Start: 950}, 4990: {PPID: 4970, Start: 940}, 4970: {PPID: 4000, Start: 920},
		4000: {PPID: 3000, Start: 700}, 3000: {PPID: 900, Start: 600}, 900: {PPID: 700, Start: 500}, 700: {PPID: 1, Start: 400},
	}
	daemonArgv := map[int][]string{
		5000: codexArgv[5000], 4990: codexArgv[4990], 4970: codexArgv[4970],
		4000: {"/home/u/.codex/packages/bin/codex", "app-server", "--listen", "unix://"},
		3000: {"codex", "app-server", "daemon", "pid-update-loop"}, 900: {"codex"}, 700: {"-zsh"},
	}
	// Inside bubblewrap's pid namespace: report (3) -> python (2) -> the
	// namespace's init (1), which ends the walk; no codex is visible.
	sandboxed := map[int]report.Stat{3: {PPID: 2, Start: 90}, 2: {PPID: 1, Start: 80}}
	sandboxedArgv := map[int][]string{3: codexArgv[5000], 2: codexArgv[4990]}
	lineage := func(pid int, procs map[int]report.Stat, cmds map[int][]string) *report.Lineage {
		return report.ReadLineage(pid, newProcTable(procs).stat).WithCmdline(argv(cmds))
	}
	started := fakePane{id: "w9:p1", shell: 700, pgid: 900, fg: []uint32{900}}

	tests := map[string]struct {
		pane report.Pane
		want error
	}{
		"the shared app-server daemon":    {pane: report.Pane{PaneID: "w9:p1", Lineage: lineage(5000, daemon, daemonArgv), CodexThread: "019a"}, want: report.ErrCodexDaemon},
		"Codex's network sandbox":         {pane: report.Pane{PaneID: "w3:p2", Lineage: lineage(5000, codexProcs(), codexArgv), CodexThread: "019a", CodexNetworkSandboxed: true}, want: report.ErrCodexSandboxed},
		"Codex's pid namespace":           {pane: report.Pane{PaneID: "w3:p2", Lineage: lineage(3, sandboxed, sandboxedArgv), CodexThread: "019a"}, want: report.ErrNoCodexHost},
		"no lineage (off Linux)":          {pane: report.Pane{PaneID: "w9:p1", CodexThread: "019a"}, want: report.ErrHostUnverified},
		"an unreadable ancestor":          {pane: report.Pane{PaneID: "w3:p2", Lineage: lineage(5000, codexProcs(), map[int][]string{5000: codexArgv[5000]}), CodexThread: "019a"}, want: report.ErrHostUnverified},
		"not Codex, no lineage (as ever)": {pane: report.Pane{PaneID: "w9:p1"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, fakeHerdr([]fakePane{started, {id: "w3:p2", shell: 600, pgid: 800, fg: []uint32{800}}}))
			tc.pane.SocketPath = srv.Path

			got, err := report.Stage(t.Context(), herdr.Client{SocketPath: srv.Path}, tc.pane, "NERD-5253", "build_test")
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Empty(t, srv.Requests(), "nothing reaches herdr")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "w9:p1", got.PaneID)
		})
	}
}

package report_test

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/team"
)

func TestIsAgentArgv(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		argv []string
		want bool
	}{
		"claude":                 {[]string{"claude", "--model", "x"}, true},
		"claude by path":         {[]string{"/home/u/.local/bin/claude"}, true},
		"codex":                  {[]string{"/opt/codex/vendor/codex", "--no-daemon"}, true},
		"node running codex":     {[]string{"node", "/usr/bin/codex"}, true},
		"node running codex.js":  {[]string{"node", "/x/codex.js"}, true},
		"node running something": {[]string{"node", "/x/server.js"}, false},
		"a shell":                {[]string{"/bin/sh", "-c", "claude"}, false},
		"a name holding claude":  {[]string{"/usr/bin/claudette", "--resume"}, false},
		"claude as an argument":  {[]string{"/usr/bin/env", "claude"}, false},
		"the reporter":           {[]string{"herdr-agentisan", "report", "codex"}, false},
		"no arguments":           {nil, false},
	} {
		assert.Equal(t, tc.want, report.IsAgentArgv(tc.argv), name)
	}
}

func TestCtxOwnerValues(t *testing.T) {
	t.Parallel()
	owner := []report.Proc{{PID: 900, Start: 500}, {PID: 899, Start: 31337}}
	assert.Equal(t, "1761066000900/42@900:500,899:31337", report.FormatCtxOwner(reportedAt(), "42", owner))
	assert.Empty(t, report.FormatCtxOwner(reportedAt(), "42", nil), "none clears the token")
	at, ctx, got, ok := report.ParseCtxOwner("1761066000900/42@900:500,899:31337")
	require.True(t, ok)
	assert.Equal(t, owner, got)
	assert.Equal(t, "42", ctx, "the ctx it belongs to")
	assert.True(t, at.Equal(reportedAt()), "the stamp it belongs to")
	// The widest value, a darwin start in microseconds, fits herdr's 80.
	wide := report.Proc{PID: 9999999, Start: 1761066000123456}
	assert.LessOrEqual(t, len(report.FormatCtxOwner(reportedAt(), "100", []report.Proc{wide, wide})), report.MaxValueChars)

	const stamp = "1761066000900/42@"
	for _, bad := range []string{
		"", "900:500", "@900:500", "1761066000900", "1761066000900@900:500", "1761066000900/@900:500", "1761066000900/042@900:500", "1761066000900/1234@900:500", "1761066000900/-1@900:500", "1761066000900/4 @900:500", "1761066000900/4/2@900:500",
		stamp, stamp + "900", stamp + "900:", stamp + ":500", stamp + "0:500", stamp + "900:0", stamp + "-1:500", stamp + "0900:500", stamp + "900:05", stamp + "900:500,", stamp + "a:b", stamp + "1:1,2:2,3:3", stamp + "900:500 ", "01761066000900/42@900:500", "0/42@900:500", stamp + "900:500;899:1",
	} {
		_, _, _, ok := report.ParseCtxOwner(bad)
		assert.False(t, ok, "%q", bad)
	}
}

// TestAgentProcs: the lineage's agents, nearest first, each still the
// process the lineage read; none without a cmdline reader.
func TestAgentProcs(t *testing.T) {
	t.Parallel()
	procs := map[int]report.Stat{
		5000: {PPID: 4998, Start: 900}, 4998: {PPID: 900, Start: 800}, 900: {PPID: 899, Start: 500}, 899: {PPID: 600, Start: 499}, 600: {PPID: 100, Start: 10}, 100: {Start: 1},
	}
	cmds := map[int][]string{
		5000: {"herdr-agentisan", "report", "codex"}, 4998: {"/bin/sh", "-lc", "x"},
		900: {"/vendor/codex", "--no-daemon"}, 899: {"node", "/usr/bin/codex"}, 600: {"-zsh"}, 100: {"herdr", "server"},
	}
	table := newProcTable(procs)
	l := report.ReadLineage(5000, table.stat).WithCmdline(argv(cmds))
	assert.Equal(t, []report.Proc{{PID: 900, Start: 500}, {PID: 899, Start: 499}}, l.AgentProcs(), "a launcher and its native child")

	table.set(899, report.Stat{PPID: 600, Start: 7})
	assert.Equal(t, []report.Proc{{PID: 900, Start: 500}}, l.AgentProcs(), "a reused pid is not the agent")
	table.exit(900)
	assert.Empty(t, l.AgentProcs(), "an exited agent")

	assert.Empty(t, report.ReadLineage(5000, newProcTable(procs).stat).AgentProcs(), "no cmdline reader")
	var none *report.Lineage
	assert.Empty(t, none.AgentProcs())

	many := map[int]report.Stat{}
	manyCmds := map[int][]string{}
	for pid, ppid := 50, 49; pid > 44; pid, ppid = pid-1, ppid-1 {
		many[pid] = report.Stat{PPID: ppid, Start: uint64(pid)} //nolint:gosec // small test pids
		manyCmds[pid] = []string{"codex"}
	}
	got := report.ReadLineage(50, newProcTable(many).stat).WithCmdline(argv(manyCmds)).AgentProcs()
	assert.Len(t, got, report.MaxOwnerProcs, "a pathological chain is cut")
	assert.Equal(t, 50, got[0].PID)
}

// TestAgentProcsNameOnlyTheNearestKind: a Codex run by a Claude tool, or a
// Claude by a Codex, reports as itself, never as the agent that runs it
// (review of cf0df58, P1 1).
func TestAgentProcsNameOnlyTheNearestKind(t *testing.T) {
	t.Parallel()
	procs := map[int]report.Stat{
		5000: {PPID: 4998, Start: 900}, 4998: {PPID: 900, Start: 800}, 900: {PPID: 600, Start: 500}, 600: {PPID: 100, Start: 400}, 100: {Start: 1},
	}
	for name, tc := range map[string]struct {
		inner, outer string
		want         report.Proc
	}{
		"a Codex under a Claude": {"/vendor/codex", "/usr/bin/claude", report.Proc{PID: 900, Start: 500}},
		"a Claude under a Codex": {"/usr/bin/claude", "/vendor/codex", report.Proc{PID: 900, Start: 500}},
	} {
		cmds := map[int][]string{5000: {"herdr-agentisan"}, 4998: {"/bin/sh"}, 900: {tc.inner}, 600: {tc.outer}, 100: {"herdr"}}
		l := report.ReadLineage(5000, newProcTable(procs).stat).WithCmdline(argv(cmds))
		assert.Equal(t, []report.Proc{tc.want}, l.AgentProcs(), name)
	}
}

// TestReportsNameTheirWriter: a report that carries ctx names the agent it
// ran under in the same report, and one that cannot clears the previous
// owner, so a ctx never stands beside another agent's owner.
func TestReportsNameTheirWriter(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lineage *report.Lineage
		want    string
	}{
		"under an agent":  {codexLineage(embeddedProcs(), embeddedArgv), "1761066000900/42@900:500,899:500"},
		"no agent in it":  {codexLineage(embeddedProcs(), map[int][]string{5000: {"herdr-agentisan"}}), ""},
		"no lineage read": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)
			pane := report.Pane{SocketPath: srv.Path, PaneID: "w14:p1", Lineage: tc.lineage, Now: reportedAt}
			if tc.lineage != nil {
				srv = herdrtest.Start(t, fakeHerdr([]fakePane{{id: "w14:p1", shell: 600, pgid: 899, fg: []uint32{899, 900}}}))
				pane.SocketPath = srv.Path
			}
			_, err := report.Statusline(t.Context(), herdr.Client{SocketPath: srv.Path}, pane, strings.NewReader(statusline("42")))
			require.NoError(t, err)
			reqs := srv.Requests()
			assert.Contains(t, string(reqs[len(reqs)-1].Params), `"ctx_owner":"`+tc.want+`"`)
		})
	}
}

// TestHelperProcessTree is not a test: the processes of
// TestOwnerIsTheAgentTheDaemonVerifies re-execute the test binary into it.
// Each names the next of HA_TREE, which runs under the name it is given, and
// the last one prints the owner the real lineage reader finds.
func TestHelperProcessTree(t *testing.T) {
	rest := os.Getenv("HA_TREE")
	if rest == "" {
		return
	}
	name, tail, _ := strings.Cut(rest, ",")
	if name == "reporter" {
		l := report.SelfLineage(func(string) (string, bool) { return "", false })
		_, _ = os.Stdout.WriteString(report.FormatCtxOwner(time.Now(), "1", l.AgentProcs()) + "\n")
		// Stay until the test has read the whole tree.
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Args = []string{filepath.Join(os.TempDir(), name), "-test.run=^TestHelperProcessTree$"}
	cmd.Env = append(os.Environ(), "HA_TREE="+tail)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}

// TestOwnerIsTheAgentTheDaemonVerifies runs the real lineage reader in a
// real process tree, a fake agent as the parent of the reporter, and checks
// that what the report would write is what the daemon's gate matches: the
// agent's pid and its start as report.SystemStat reads it, the daemon's own
// reader.
func TestOwnerIsTheAgentTheDaemonVerifies(t *testing.T) {
	t.Parallel()
	stat := report.SystemStat()
	if stat == nil || runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no process table to read a lineage from")
	}
	self, err := os.Executable()
	require.NoError(t, err)
	for name, tc := range map[string]struct{ tree string }{
		"claude":                   {"claude,hook,reporter"},
		"codex through a launcher": {"codex,codex,hook,reporter"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), self)
			first, _, _ := strings.Cut(tc.tree, ",")
			cmd.Args = []string{filepath.Join(os.TempDir(), first), "-test.run=^TestHelperProcessTree$"}
			_, tail, _ := strings.Cut(tc.tree, ",")
			cmd.Env = append(os.Environ(), "HA_TREE="+tail)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			in, err := cmd.StdinPipe()
			require.NoError(t, err)
			out, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			line, readErr := bufio.NewReader(out).ReadString('\n')
			require.NoError(t, readErr)
			outer := cmd.Process.Pid
			// Every process of the tree is alive: read the outer agent as the
			// daemon would.
			st, statErr := stat(outer)
			require.NoError(t, statErr)
			require.NoError(t, in.Close())
			require.NoError(t, cmd.Wait(), stderr.String())

			_, _, owner, ok := report.ParseCtxOwner(strings.TrimSpace(line))
			require.True(t, ok, "owner %q", line)
			// Whoever runs the tests is an ancestor too, so only the tree's
			// own agents, the nearest ones, are asserted.
			inTree := strings.Count(tc.tree, "claude") + strings.Count(tc.tree, "codex")
			require.GreaterOrEqual(t, len(owner), inTree)
			assert.Equal(t, report.Proc{PID: outer, Start: st.Start}, owner[inTree-1], "the outermost agent of the tree, as the daemon reads it")
			pane := team.Pane{Ctx: "70", CtxOwner: owner}
			assert.Equal(t, "70", team.CtxGate{Agent: report.Proc{PID: outer, Start: st.Start}}.Ctx(pane))
			assert.Empty(t, team.CtxGate{Agent: report.Proc{PID: outer, Start: st.Start + 1}}.Ctx(pane), "a reused pid")
		})
	}
}

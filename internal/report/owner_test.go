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
		"the reporter":           {[]string{"herdr-agentisan", "report", "codex"}, false},
		"no arguments":           {nil, false},
	} {
		assert.Equal(t, tc.want, report.IsAgentArgv(tc.argv), name)
	}
}

func TestCtxOwnerValues(t *testing.T) {
	t.Parallel()
	owner := []report.Proc{{PID: 900, Start: 500}, {PID: 899, Start: 31337}}
	assert.Equal(t, "900:500,899:31337", report.FormatCtxOwner(owner))
	assert.Empty(t, report.FormatCtxOwner(nil), "none clears the token")
	got, ok := report.ParseCtxOwner("900:500,899:31337")
	require.True(t, ok)
	assert.Equal(t, owner, got)
	// The widest value, a darwin start in microseconds, fits herdr's 80.
	wide := report.Proc{PID: 9999999, Start: 1761066000123456}
	assert.LessOrEqual(t, len(report.FormatCtxOwner([]report.Proc{wide, wide, wide})), report.MaxValueChars)

	for _, bad := range []string{"", "900", "900:", ":500", "0:500", "900:0", "-1:500", "0900:500", "900:05", "900:500,", "a:b", "1:1,2:2,3:3,4:4", "900:500 ", "900:500;899:1"} {
		_, ok := report.ParseCtxOwner(bad)
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

// TestReportsNameTheirWriter: a report that carries ctx names the agent it
// ran under in the same report, and one that cannot clears the previous
// owner, so a ctx never stands beside another agent's owner.
func TestReportsNameTheirWriter(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lineage *report.Lineage
		want    string
	}{
		"under an agent":  {codexLineage(embeddedProcs(), embeddedArgv), "900:500,899:500"},
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
		_, _ = os.Stdout.WriteString(report.FormatCtxOwner(l.AgentProcs()) + "\n")
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

			owner, ok := report.ParseCtxOwner(strings.TrimSpace(line))
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

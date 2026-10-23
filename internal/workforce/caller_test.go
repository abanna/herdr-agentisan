package workforce_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// countingFinder is a paneFinder that counts every call made to it.
type countingFinder struct {
	paneFinder
	calls *atomic.Int32
}

func (f countingFinder) PaneProcessInfo(ctx context.Context, id string) (herdr.ProcessInfo, error) {
	f.calls.Add(1)
	return f.paneFinder.PaneProcessInfo(ctx, id) //nolint:wrapcheck // the fake's own error
}

func (f countingFinder) ListPanes(ctx context.Context) ([]herdr.PaneInfo, error) {
	f.calls.Add(1)
	return f.paneFinder.ListPanes(ctx) //nolint:wrapcheck // the fake's own error
}

// callerLineage reads pid 5000's lineage from procs, with cmds as each
// process's arguments; a pid missing from either cannot be read.
func callerLineage(procs map[int]report.Stat, cmds map[int][]string) *report.Lineage {
	return report.ReadLineage(5000, func(pid int) (report.Stat, error) {
		s, ok := procs[pid]
		if !ok {
			return report.Stat{}, errors.New("no such process")
		}
		return s, nil
	}).WithCmdline(func(pid int) ([]string, error) {
		a, ok := cmds[pid]
		if !ok {
			return nil, report.ErrProcCmdline
		}
		return a, nil
	})
}

// TestFindCallerUnderTheSharedAppServer (A64, NERD-5506): a Codex session on
// the shared app-server inherits the environment of the session that started
// the daemon, ha-coder-1's in w1:p1, and while that TUI lives its lineage runs
// through w1:p1's shell, which herdr would confirm (A22). So a command run
// there names neither w1:p1 nor ha-coder-1: it is ErrCallerUnverified, and
// herdr is never asked. The pane's own Codex (--no-daemon) and Claude find
// their pane and their HERDR_AGENTISAN_WORKER as before.
func TestFindCallerUnderTheSharedAppServer(t *testing.T) {
	t.Parallel()
	// send (5000) -> bash (4998) -> app-server (4000) -> the TUI that
	// started it (900) -> w1:p1's shell (600); once that TUI exits, the
	// app-server's parent is init.
	alive := map[int]report.Stat{
		5000: {PPID: 4998, Start: 900}, 4998: {PPID: 4000, Start: 800}, 4000: {PPID: 900, Start: 700},
		900: {PPID: 600, Start: 500}, 600: {PPID: 1, Start: 400},
	}
	gone := map[int]report.Stat{5000: {PPID: 4998, Start: 900}, 4998: {PPID: 4000, Start: 800}, 4000: {PPID: 1, Start: 700}}
	daemonArgv := map[int][]string{
		5000: {"herdr-agentisan", "send", "boss", "hi"}, 4998: {"/bin/bash", "-lc", "herdr-agentisan send boss hi"},
		4000: {"codex", "app-server", "--listen", "unix://"}, 900: {"codex"}, 600: {"-zsh"},
	}
	// The pane's own Codex: send (5000) -> bash (4998) -> codex --no-daemon
	// (900) -> w1:p1's shell (600).
	embedded := map[int]report.Stat{5000: {PPID: 4998, Start: 900}, 4998: {PPID: 900, Start: 800}, 900: {PPID: 600, Start: 500}, 600: {PPID: 1, Start: 400}}
	embeddedArgv := map[int][]string{5000: daemonArgv[5000], 4998: daemonArgv[4998], 900: {"codex", "--no-daemon"}, 600: {"-zsh"}}
	claudeArgv := map[int][]string{5000: daemonArgv[5000], 4998: daemonArgv[4998], 900: {"claude", "--name", "ha-coder-1"}, 600: {"-zsh"}}
	w1 := paneFinder{panes: map[string]herdr.ProcessInfo{"w1:p1": {PaneID: "w1:p1", ShellPID: 600}}, listed: []herdr.PaneInfo{{PaneID: "w1:p1"}}}
	tests := map[string]struct {
		pane report.Pane
		want workforce.Caller
		err  error
	}{
		"a tool command, the starting TUI alive": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a", Lineage: callerLineage(alive, daemonArgv)}, err: report.ErrCodexDaemon,
		},
		"a hook, the starting TUI alive": {
			pane: report.Pane{PaneID: "w1:p1", Lineage: callerLineage(alive, daemonArgv)}, err: report.ErrCodexDaemon,
		},
		"a tool command, the starting TUI gone": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a", Lineage: callerLineage(gone, daemonArgv)}, err: report.ErrCodexDaemon,
		},
		"the TUI gone, its HERDR_PANE_ID unset": {
			pane: report.Pane{CodexThread: "019a", Lineage: callerLineage(gone, daemonArgv)}, err: report.ErrCodexDaemon,
		},
		"under Codex, no lineage": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a"}, err: report.ErrHostUnverified,
		},
		"the pane's own Codex": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a", Lineage: callerLineage(embedded, embeddedArgv)},
			want: workforce.Caller{Pane: "w1:p1", Worker: "ha-coder-1"},
		},
		"Claude": {
			pane: report.Pane{PaneID: "w1:p1", Lineage: callerLineage(embedded, claudeArgv)},
			want: workforce.Caller{Pane: "w1:p1", Worker: "ha-coder-1"},
		},
		"no lineage, not under Codex": {
			pane: report.Pane{PaneID: "w1:p1"},
			want: workforce.Caller{Worker: "ha-coder-1"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := countingFinder{paneFinder: w1, calls: &atomic.Int32{}}
			tc.pane.SocketPath = "unused"
			var workerReads atomic.Int32
			env := func(k string) (string, bool) {
				if k == workforce.WorkerEnv {
					workerReads.Add(1)
					return "ha-coder-1", true
				}
				return "", false
			}
			got, err := workforce.FindCaller(t.Context(), h, tc.pane, env)
			if tc.err == nil {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.EqualValues(t, 1, workerReads.Load(), "the verified caller reads its worker once")
				return
			}
			require.ErrorIs(t, err, workforce.ErrCallerUnverified)
			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, workforce.Caller{}, got, "neither the pane nor the worker")
			assert.Zero(t, workerReads.Load(), "a refused caller never reads the inherited worker")
			assert.Zero(t, h.calls.Load(), "herdr is never asked")

			pane, err := workforce.FindCallerPane(t.Context(), h, tc.pane)
			require.ErrorIs(t, err, workforce.ErrCallerUnverified)
			assert.Empty(t, pane)
			assert.Zero(t, h.calls.Load(), "herdr is never asked")
		})
	}
}

// TestFindCallerInTheSandboxNamespace (A64, A25): a command in Codex's
// network sandbox whose walk ends in the sandbox's pid namespace, with no
// Codex in it, is unverified and also report.ErrCodexSandboxed, so it is told
// why and who to ask, and herdr is never asked; the team ops take their
// fallbacks. The sandbox's variable alone never makes it so: the shared
// app-server with it stays only unverified, as does no Codex in a walk
// without it, and the pane's own Codex with it, an escalation, finds its pane.
func TestFindCallerInTheSandboxNamespace(t *testing.T) {
	t.Parallel()
	// send (5000) -> bash (4998), the sandbox's pid namespace starting there.
	namespace := map[int]report.Stat{5000: {PPID: 4998, Start: 900}, 4998: {PPID: 1, Start: 800}}
	namespaceArgv := map[int][]string{5000: {"herdr-agentisan", "send", "boss", "hi"}, 4998: {"/bin/bash", "-lc", "herdr-agentisan send boss hi"}}
	// send (5000) -> bash (4998) -> app-server (4000) -> init.
	shared := map[int]report.Stat{5000: {PPID: 4998, Start: 900}, 4998: {PPID: 4000, Start: 800}, 4000: {PPID: 1, Start: 700}}
	sharedArgv := map[int][]string{5000: namespaceArgv[5000], 4998: namespaceArgv[4998], 4000: {"codex", "app-server", "--listen", "unix://"}}
	// send (5000) -> bash (4998) -> codex --no-daemon (900) -> w1:p1's shell (600).
	embedded := map[int]report.Stat{5000: {PPID: 4998, Start: 900}, 4998: {PPID: 900, Start: 800}, 900: {PPID: 600, Start: 500}, 600: {PPID: 1, Start: 400}}
	embeddedArgv := map[int][]string{5000: namespaceArgv[5000], 4998: namespaceArgv[4998], 900: {"codex", "--no-daemon"}, 600: {"-zsh"}}
	w1 := paneFinder{panes: map[string]herdr.ProcessInfo{"w1:p1": {PaneID: "w1:p1", ShellPID: 600}}, listed: []herdr.PaneInfo{{PaneID: "w1:p1"}}}
	tests := map[string]struct {
		pane      report.Pane
		want      workforce.Caller
		err       error
		sandboxed bool // the error is also report.ErrCodexSandboxed
	}{
		"in the sandbox's pid namespace": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a", CodexNetworkSandboxed: true, Lineage: callerLineage(namespace, namespaceArgv)},
			err:  report.ErrNoCodexHost, sandboxed: true,
		},
		"the shared app-server, the sandbox's variable set": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a", CodexNetworkSandboxed: true, Lineage: callerLineage(shared, sharedArgv)},
			err:  report.ErrCodexDaemon,
		},
		"no Codex in the walk, outside the sandbox": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a", Lineage: callerLineage(namespace, namespaceArgv)},
			err:  report.ErrNoCodexHost,
		},
		"the pane's own Codex, the sandbox's variable set": {
			pane: report.Pane{PaneID: "w1:p1", CodexThread: "019a", CodexNetworkSandboxed: true, Lineage: callerLineage(embedded, embeddedArgv)},
			want: workforce.Caller{Pane: "w1:p1", Worker: "ha-coder-1"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := countingFinder{paneFinder: w1, calls: &atomic.Int32{}}
			tc.pane.SocketPath = "unused"
			var workerReads atomic.Int32
			env := func(k string) (string, bool) {
				if k == workforce.WorkerEnv {
					workerReads.Add(1)
					return "ha-coder-1", true
				}
				return "", false
			}
			got, err := workforce.FindCaller(t.Context(), h, tc.pane, env)
			if tc.err == nil {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.EqualValues(t, 1, workerReads.Load(), "the verified caller reads its worker once")
				return
			}
			require.ErrorIs(t, err, workforce.ErrCallerUnverified)
			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, tc.sandboxed, errors.Is(err, report.ErrCodexSandboxed), "told the sandbox is why: %v", err)
			assert.Equal(t, tc.sandboxed, !strings.Contains(err.Error(), "--no-daemon"), "told to start Codex with --no-daemon: %v", err)
			assert.Equal(t, workforce.Caller{}, got, "neither the pane nor the worker")
			assert.Zero(t, workerReads.Load(), "a refused caller never reads the inherited worker")

			sel, err := workforce.SelectorFor(t.Context(), h, "", tc.pane)
			require.NoError(t, err)
			assert.Equal(t, workforce.Selector{}, sel, "the team ops take their fallbacks")
			assert.Zero(t, h.calls.Load(), "herdr is never asked")
		})
	}
}

// TestSelectorForAnUnverifiedCaller (A64): a team op run on the shared
// app-server confirms no pane, so it takes its fallbacks: the pane that
// started the daemon never chooses the team, and herdr is never asked;
// --project still names the team.
func TestSelectorForAnUnverifiedCaller(t *testing.T) {
	t.Parallel()
	procs := map[int]report.Stat{5000: {PPID: 4000, Start: 900}, 4000: {PPID: 900, Start: 700}, 900: {PPID: 600, Start: 500}, 600: {PPID: 1, Start: 400}}
	cmds := map[int][]string{5000: {"herdr-agentisan", "team", "status"}, 4000: {"codex", "app-server"}, 900: {"codex"}, 600: {"-zsh"}}
	pane := report.Pane{SocketPath: "unused", PaneID: "w1:p1", CodexThread: "019a", Lineage: callerLineage(procs, cmds)}
	h := countingFinder{paneFinder: paneFinder{panes: map[string]herdr.ProcessInfo{"w1:p1": {PaneID: "w1:p1", ShellPID: 600}}}, calls: &atomic.Int32{}}

	got, err := workforce.SelectorFor(t.Context(), h, "", pane)
	require.NoError(t, err)
	assert.Equal(t, workforce.Selector{}, got)
	got, err = workforce.SelectorFor(t.Context(), h, "ha", pane)
	require.NoError(t, err)
	assert.Equal(t, workforce.Selector{Project: "ha"}, got)
	assert.Zero(t, h.calls.Load(), "herdr is never asked")
}

// TestFindCallerAfterAFailedPaneLookup: a verified host whose pane lookup
// fails preserves the cause and ErrHerdr, without reading its worker.
func TestFindCallerAfterAFailedPaneLookup(t *testing.T) {
	t.Parallel()
	procs := map[int]report.Stat{5000: {PPID: 900, Start: 900}, 900: {PPID: 600, Start: 500}, 600: {PPID: 1, Start: 400}}
	cmds := map[int][]string{5000: {"herdr-agentisan", "send", "boss", "hi"}, 900: {"codex", "--no-daemon"}, 600: {"-zsh"}}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			h := countingFinder{paneFinder: paneFinder{failing: cause}, calls: &atomic.Int32{}}
			pane := report.Pane{SocketPath: "unused", PaneID: "w1:p1", CodexThread: "019a", Lineage: callerLineage(procs, cmds)}
			var workerReads atomic.Int32
			lookup := func(k string) (string, bool) {
				if k == workforce.WorkerEnv {
					workerReads.Add(1)
					return "ha-coder-1", true
				}
				return "", false
			}
			got, err := workforce.FindCaller(t.Context(), h, pane, lookup)
			require.ErrorIs(t, err, workforce.ErrHerdr)
			require.ErrorIs(t, err, cause)
			assert.NotErrorIs(t, err, workforce.ErrCallerUnverified, "the host passed verification")
			assert.Equal(t, workforce.Caller{}, got)
			assert.EqualValues(t, 1, h.calls.Load(), "the verified host reached the pane lookup")
			assert.Zero(t, workerReads.Load(), "a failed pane lookup never reads the worker")
		})
	}
}

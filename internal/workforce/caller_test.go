package workforce_test

import (
	"context"
	"errors"
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
	env := func(k string) (string, bool) {
		if k == workforce.WorkerEnv {
			return "ha-coder-1", true
		}
		return "", false
	}
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
			got, err := workforce.FindCaller(t.Context(), h, tc.pane, env)
			if tc.err == nil {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				return
			}
			require.ErrorIs(t, err, workforce.ErrCallerUnverified)
			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, workforce.Caller{}, got, "neither the pane nor the worker")
			assert.Zero(t, h.calls.Load(), "herdr is never asked")

			pane, err := workforce.FindCallerPane(t.Context(), h, tc.pane)
			require.ErrorIs(t, err, workforce.ErrCallerUnverified)
			assert.Empty(t, pane)
			assert.Zero(t, h.calls.Load(), "herdr is never asked")
		})
	}
}

// TestSelectorForAnUnverifiedCaller (A64): a team op run on the shared
// app-server confirms no pane, so it takes its fallbacks, never the team of
// the pane that started the daemon, and herdr is never asked; --project still
// names the team.
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

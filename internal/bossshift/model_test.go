package bossshift_test

import (
	"io/fs"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/bossshift"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// NERD-5262: expiry at the TTL boundary is independent of both start cutoffs.
func TestCheckModelExpiresAtTTL(t *testing.T) {
	t.Parallel()
	fixture := newModelFixture()
	fixture.stamp = fixture.now.Add(-report.CtxTTL + time.Millisecond)
	verdict, err := fixture.check(t)
	require.NoError(t, err)
	require.Equal(t, "verified", verdict.Reason)

	fixture.stamp = fixture.stamp.Add(-time.Millisecond)
	verdict, err = fixture.check(t)
	require.ErrorIs(t, err, bossshift.ErrModelProof)
	require.Equal(t, "stale_report", verdict.Reason)
}

func TestCheckModelRejectsPaneSwapAfterAgentGet(t *testing.T) {
	t.Parallel()
	fixture := newModelFixture()
	fixture.paneTerminal = "replacement-terminal"
	verdict, err := fixture.check(t)
	require.ErrorIs(t, err, bossshift.ErrModelProof)
	require.Equal(t, "candidate_changed", verdict.Reason)
}

func TestCheckModelRejectsReplacementPID(t *testing.T) {
	t.Parallel()
	fixture := newModelFixture()
	// Two processes may start in the same clock tick.
	fixture.foregroundPID++
	verdict, err := fixture.check(t)
	require.ErrorIs(t, err, bossshift.ErrModelProof)
	require.Equal(t, "candidate_changed", verdict.Reason)
}

func TestCheckModelRejectsUnreadableProcess(t *testing.T) {
	t.Parallel()
	fixture := newModelFixture()
	read := fixture.stat
	first := true
	fixture.stat = func(pid int) (report.Stat, error) {
		if first {
			first = false
			return report.Stat{}, fs.ErrPermission
		}
		return read(pid)
	}
	verdict, err := fixture.check(t)
	require.ErrorIs(t, err, bossshift.ErrModelProof)
	require.Equal(t, "claude_unavailable", verdict.Reason)
	require.Zero(t, verdict.PID)
	require.Zero(t, verdict.Start)
}

type modelFixture struct {
	target        bossshift.Candidate
	now           time.Time
	stamp         time.Time
	started       time.Time
	paneTerminal  string
	foregroundPID uint32
	stat          report.StatFunc
}

func newModelFixture() modelFixture {
	now := time.Unix(1_800_000_000, 0)
	launched := now.Add(-2 * report.CtxTTL)
	const pid, start = 100, 200
	return modelFixture{
		target: bossshift.Candidate{
			Pane: "p", Terminal: "term", Name: "next", Model: "fable",
			LaunchedAt: launched.UnixMilli(), PID: pid, Start: start,
		},
		now: now, stamp: now, started: launched.Add(time.Second),
		paneTerminal: "term", foregroundPID: pid,
		stat: func(int) (report.Stat, error) { return report.Stat{Start: start}, nil },
	}
}

func (f modelFixture) check(t *testing.T) (bossshift.ModelVerdict, error) {
	t.Helper()
	srv := herdrtest.Start(t, func(req herdrtest.Request) herdrtest.Reply {
		switch req.Method {
		case "agent.get":
			agent := herdr.Agent{PaneID: f.target.Pane, TerminalID: f.target.Terminal, Name: f.target.Name, Agent: "claude"}
			return herdrtest.Reply{Result: map[string]any{"type": "agent_info", "agent": agent}}
		case "pane.process_info":
			info := herdr.ProcessInfo{PaneID: f.target.Pane, ForegroundProcesses: []herdr.Process{
				{PID: f.foregroundPID, Argv: []string{"claude"}},
			}}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": info}}
		case "pane.get":
			pane := herdr.PaneInfo{PaneID: f.target.Pane, TerminalID: f.paneTerminal, Agent: "claude", Tokens: map[string]string{
				report.ModelKey: f.target.Model, report.ModelAtKey: strconv.FormatInt(f.stamp.UnixMilli(), 10),
			}}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_info", "pane": pane}}
		default:
			return herdrtest.Reply{}
		}
	})
	return bossshift.CheckModelWithReaders(t.Context(), herdr.Client{SocketPath: srv.Path}, f.target, f.stat,
		func(uint64) (time.Time, error) { return f.started, nil }, func() time.Time { return f.now })
}

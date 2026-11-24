package bossshift_test

import (
	"fmt"
	"io/fs"
	"strconv"
	"testing"
	"testing/fstest"
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
	fixture.foreground[0].PID++
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

// An unreaped Claude has the same start ticks as the live process (NERD-5262).
func TestCheckModelRejectsZombieAtEitherRead(t *testing.T) {
	t.Parallel()
	for _, deathAt := range []int{1, 2} {
		t.Run(strconv.Itoa(deathAt), func(t *testing.T) {
			t.Parallel()
			fixture := newModelFixture()
			reads := 0
			fixture.stat = func(pid int) (report.Stat, error) {
				reads++
				state := "S"
				if reads >= deathAt {
					state = "Z"
				}
				raw := fmt.Sprintf("%d (claude) %s 99 1 1 0 -1 4194304 1 0 0 0 1 2 0 0 20 0 1 0 200", pid, state)
				return report.ProcStat(fstest.MapFS{strconv.Itoa(pid) + "/stat": {Data: []byte(raw)}})(pid)
			}
			verdict, err := fixture.check(t)
			require.ErrorIs(t, err, bossshift.ErrModelProof)
			require.Equal(t, []string{"claude_unavailable", "candidate_changed"}[deathAt-1], verdict.Reason)
		})
	}
}

// The first call names the Claude the launch id belongs to, before any report.
func TestCheckModelIdentifiesTheLaunchedClaude(t *testing.T) {
	t.Parallel()
	fixture := newModelFixture()
	fixture.target.PID, fixture.target.Start = 0, 0
	fixture.noTokens = true
	verdict, err := fixture.check(t)
	require.NoError(t, err)
	require.Equal(t, "identified", verdict.Reason)
	require.Equal(t, uint32(100), verdict.PID)
	require.Equal(t, uint64(200), verdict.Start)
}

// The launch id must be the value of --session-id, in either form.
func TestCheckModelRequiresTheLaunchSessionID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		argv   []string
		reason string
	}{
		{"space form", []string{"claude", "--session-id", "launch-1"}, "verified"},
		{"equals form", []string{"claude", "--session-id=launch-1"}, "verified"},
		{"no id", []string{"claude", "--model", "fable"}, "candidate_changed"},
		{"another id", []string{"claude", "--session-id", "launch-2"}, "candidate_changed"},
		{"the id as another flag's value", []string{"claude", "--name", "launch-1"}, "candidate_changed"},
		{"the id with nothing after the flag", []string{"claude", "--session-id"}, "candidate_changed"},
		{"a longer id", []string{"claude", "--session-id=launch-12"}, "candidate_changed"},
	}
	for _, tc := range tests {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s pinned=%t", tc.name, pinned), func(t *testing.T) {
				t.Parallel()
				fixture := newModelFixture()
				fixture.foreground[0].Argv = tc.argv
				want := tc.reason
				if !pinned {
					fixture.target.PID, fixture.target.Start = 0, 0
					if want == "verified" {
						want = "identified"
					}
				}
				verdict, err := fixture.check(t)
				require.Equal(t, want, verdict.Reason)
				if want == "candidate_changed" {
					require.ErrorIs(t, err, bossshift.ErrModelProof)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

// NERD-5262: vary one source of proof while the remaining reads agree.
func TestCheckModelRefusesBrokenProof(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, reason string
		change       func(*modelFixture)
	}{
		{"two_Claude_processes", "claude_unavailable", func(f *modelFixture) {
			f.foreground = append(f.foreground, herdr.Process{PID: 101, Argv: []string{"claude"}})
		}},
		{"exited_Claude", "claude_unavailable", func(f *modelFixture) {
			f.foreground = nil
			f.target.PID, f.target.Start = 0, 0
		}},
		{"agent_is_not_Claude", "claude_unavailable", func(f *modelFixture) { f.agent.Agent = "codex" }},
		{"pane_agent_is_not_Claude", "candidate_changed", func(f *modelFixture) { f.paneAgent = "codex" }},
		{"process_changes_during_read", "candidate_changed", func(f *modelFixture) {
			start := f.target.Start
			f.stat = func(int) (report.Stat, error) {
				p := report.Stat{Start: start}
				start++
				return p, nil
			}
		}},
		{"wall_clock_unreadable", "claude_unavailable", func(f *modelFixture) {
			f.wall = func(uint64) (time.Time, error) { return time.Time{}, fs.ErrPermission }
		}},
		{"pid_without_start", "invalid_candidate", func(f *modelFixture) { f.target.Start = 0 }},
		{"no_launch_id", "invalid_candidate", func(f *modelFixture) { f.target.SessionID = "" }},
		{"zero_launch", "invalid_candidate", func(f *modelFixture) { f.target.LaunchedAt = 0 }},
		{"different_agent_name", "candidate_changed", func(f *modelFixture) { f.agent.Name = "stranger" }},
		{"different_agent_terminal", "candidate_changed", func(f *modelFixture) { f.agent.TerminalID = "stranger" }},
		{"different_process_info_pane", "candidate_changed", func(f *modelFixture) { f.processPane = "stranger" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newModelFixture()
			tc.change(&fixture)
			verdict, err := fixture.check(t)
			require.ErrorIs(t, err, bossshift.ErrModelProof)
			require.Equal(t, tc.reason, verdict.Reason)
		})
	}
}

type modelFixture struct {
	target                  bossshift.Candidate
	now                     time.Time
	stamp                   time.Time
	agent                   herdr.Agent
	paneTerminal, paneAgent string
	noTokens                bool
	processPane             string
	foreground              []herdr.Process
	stat                    report.StatFunc
	wall                    report.StartedAtFunc
}

func newModelFixture() modelFixture {
	now := time.Unix(1_800_000_000, 0)
	launched := now.Add(-2 * report.CtxTTL)
	const pid, start = 100, 200
	return modelFixture{
		target: bossshift.Candidate{
			Pane: "p", Terminal: "term", Name: "next", Model: "fable",
			LaunchedAt: launched.UnixMilli(), SessionID: "launch-1", PID: pid, Start: start,
		},
		now: now, stamp: now,
		agent:        herdr.Agent{PaneID: "p", TerminalID: "term", Name: "next", Agent: "claude"},
		paneTerminal: "term", paneAgent: "claude", processPane: "p",
		foreground: []herdr.Process{{PID: pid, Argv: []string{"claude", "--session-id", "launch-1"}}},
		stat:       func(int) (report.Stat, error) { return report.Stat{Start: start}, nil },
		wall:       func(uint64) (time.Time, error) { return launched.Add(time.Second), nil },
	}
}

func (f modelFixture) check(t *testing.T) (bossshift.ModelVerdict, error) {
	t.Helper()
	srv := herdrtest.Start(t, func(req herdrtest.Request) herdrtest.Reply {
		switch req.Method {
		case "agent.get":
			return herdrtest.Reply{Result: map[string]any{"type": "agent_info", "agent": f.agent}}
		case "pane.process_info":
			info := herdr.ProcessInfo{PaneID: f.processPane, ForegroundProcesses: f.foreground}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": info}}
		case "pane.get":
			pane := herdr.PaneInfo{PaneID: f.target.Pane, TerminalID: f.paneTerminal, Agent: f.paneAgent, Tokens: map[string]string{
				report.ModelKey: f.target.Model, report.ModelAtKey: strconv.FormatInt(f.stamp.UnixMilli(), 10),
			}}
			if f.noTokens {
				pane.Tokens = nil
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_info", "pane": pane}}
		default:
			return herdrtest.Reply{}
		}
	})
	return bossshift.CheckModelWithReaders(t.Context(), herdr.Client{SocketPath: srv.Path}, f.target, f.stat,
		f.wall, func() time.Time { return f.now })
}

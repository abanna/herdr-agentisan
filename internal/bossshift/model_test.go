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

// The launch id must be the value of --session-id, in either form. The first
// call, before the pin, names the Claude and reads no report.
func TestCheckModelRequiresTheLaunchSessionID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		argv     []string
		reason   string
		unpinned bool // also run before the pin
	}{
		{"space form", []string{"claude", "--session-id", "launch-1"}, "verified", true},
		{"equals form", []string{"claude", "--session-id=launch-1"}, "verified", false},
		{"no id", []string{"claude", "--model", "fable"}, "candidate_changed", true},
		{"another id", []string{"claude", "--session-id", "launch-2"}, "candidate_changed", false},
		{"the id as another flag's value", []string{"claude", "--name", "launch-1"}, "candidate_changed", false},
		{"the id with nothing after the flag", []string{"claude", "--session-id"}, "candidate_changed", false},
		{"a longer id", []string{"claude", "--session-id=launch-12"}, "candidate_changed", false},
	}
	for _, tc := range tests {
		for _, pinned := range []bool{true, false} {
			if !pinned && !tc.unpinned {
				continue
			}
			t.Run(fmt.Sprintf("%s pinned=%t", tc.name, pinned), func(t *testing.T) {
				t.Parallel()
				fixture := newModelFixture()
				fixture.foreground[0].Argv = tc.argv
				want := tc.reason
				if !pinned {
					fixture.target.PID, fixture.target.Start, fixture.noTokens = 0, 0, true
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
		{"pane_swap_after_agent_get", "candidate_changed", func(f *modelFixture) { f.paneTerminal = "replacement-terminal" }},
		{"replacement_PID_in_the_same_tick", "candidate_changed", func(f *modelFixture) { f.foreground[0].PID++ }},
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
		{"report_from_another_session", "wrong_session", func(f *modelFixture) { f.reportSession = "launch-2" }},
		{"report_names_no_session", "wrong_session", func(f *modelFixture) { f.reportSession = "" }},
		{"same_pid_different_start", "candidate_changed", func(f *modelFixture) {
			f.stat = func(int) (report.Stat, error) { return report.Stat{Start: f.target.Start + 1}, nil }
		}},
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
	reportSession           string
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
		now: now, stamp: now, reportSession: "launch-1",
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
			pane.Tokens[report.ModelSessionKey] = f.reportSession
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

package cli_test

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/report"
)

// NERD-5262: argv deliberately promises Fable in every case. Only the
// current Claude's pane report can approve the shift.
func TestBossModelReadbackRejectsLaunchFallbackAndStaleToken(t *testing.T) {
	t.Parallel()
	stat, wall := report.SystemStat(), report.SystemStartedAt()
	if stat == nil || wall == nil {
		t.Skip("process start clock unavailable")
	}
	proc, err := stat(os.Getpid())
	require.NoError(t, err)
	started, err := wall(proc.Start)
	require.NoError(t, err)
	startedMS := started.UnixMilli()
	launch := startedMS - 60_000
	fresh := time.Now().UnixMilli()
	tests := []struct {
		name, model, requested, terminal, reason string
		stamp, launch                            int64
		pin                                      uint64
		session                                  string
	}{
		{"argv only", "", "fable", "term", "missing_model", 0, launch, 0, ""},
		{"before launch", "fable", "fable", "term", "stale_report", startedMS, startedMS + 1, 0, ""},
		{"before Claude", "fable", "fable", "term", "stale_report", startedMS - 1, launch, 0, ""},
		{"future", "fable", "fable", "term", "invalid_report_time", fresh + 60000, launch, 0, ""},
		{"first observation names the launched Claude", "", "fable", "term", "identified", 0, launch, 0, ""},
		{"another launch id", "fable", "fable", "term", "candidate_changed", fresh, launch, 0, "someone-else"},
		{"wrong terminal", "fable", "fable", "different", "candidate_changed", fresh, launch, 0, ""},
		{"reused PID", "fable", "fable", "term", "candidate_changed", fresh, launch, proc.Start + 1, ""},
		{"substring", "claude-unfable-5-2", "fable", "term", "wrong_model", fresh, launch, 0, ""},
		{"suffix", "claude-fable-5-2-sonnet", "fable", "term", "wrong_model", fresh, launch, 0, ""},
		{"exact", "fable", "fable", "term", "verified", fresh, launch, proc.Start, ""},
		{"family ID", "claude-fable-5-2", "fable", "term", "verified", fresh, launch, 0, ""},
		{"one-million-context Fable ID", "claude-fable-5-2[1m]", "fable", "term", "verified", fresh, launch, 0, ""},
		{"uppercase Fable suffix", "claude-fable-5-2[1M]", "fable", "term", "wrong_model", fresh, launch, 0, ""},
		{"other Fable context window", "claude-fable-5-2[200k]", "fable", "term", "wrong_model", fresh, launch, 0, ""},
		{"date-suffixed Fable ID", "claude-fable-5-2-20261123", "fable", "term", "verified", fresh, launch, 0, ""},
		{"other family ID", "claude-sonnet-4-6-20261123", "fable", "term", "wrong_model", fresh, launch, 0, ""},
		{"configured other family", "claude-sonnet-4-6-20261123", "claude-sonnet-4-6-20261123", "term", "verified", fresh, launch, 0, ""},
		{"configured ID is exact", "claude-fable-5-2-20261123", "claude-fable-5-2", "term", "wrong_model", fresh, launch, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agent := herdr.Agent{PaneID: "p", TerminalID: tc.terminal, Name: "next", Agent: "claude"}
			pane := herdr.PaneInfo{PaneID: "p", TerminalID: tc.terminal, Agent: "claude", Tokens: map[string]string{
				"model": tc.model, "model_at": strconv.FormatInt(tc.stamp, 10), "model_session": "launch-1",
			}}
			info := herdr.ProcessInfo{PaneID: "p", ForegroundProcesses: []herdr.Process{
				{PID: uint32(os.Getpid()), Argv: []string{"claude", "--model", "fable", "--session-id", "launch-1"}}, // #nosec G115 -- OS PID fits in a uint32
			}}
			srv := herdrtest.Start(t, func(req herdrtest.Request) herdrtest.Reply {
				switch req.Method {
				case "agent.get":
					return herdrtest.Reply{Result: map[string]any{"type": "agent_info", "agent": agent}}
				case "pane.get":
					return herdrtest.Reply{Result: map[string]any{"type": "pane_info", "pane": pane}}
				case "pane.process_info":
					return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": info}}
				default:
					return herdrtest.Reply{}
				}
			})
			lookup := func(key string) (string, bool) { return srv.Path, key == "HERDR_SOCKET_PATH" }
			args := []string{
				"boss", "shift", "check-model", "--pane=p", "--terminal=term", "--name=next", "--model=" + tc.requested,
				fmt.Sprintf("--launched-at=%d", tc.launch), "--session-id=" + cmp.Or(tc.session, "launch-1"),
			}
			if tc.reason != "identified" {
				args = append(args, fmt.Sprintf("--pid=%d", os.Getpid()), fmt.Sprintf("--start=%d", cmp.Or(tc.pin, proc.Start)))
			}
			out, _, err := runOut(t, lookup, args...)
			verdict := oneJSONObject(t, out)
			require.Equal(t, tc.reason, verdict["reason"])
			require.Equal(t, tc.requested, verdict["requested_model"])
			if tc.reason != "candidate_changed" {
				require.Equal(t, tc.model, verdict["observed_model"])
				require.Equal(t, float64(os.Getpid()), verdict["pid"], "record the process even when the model is missing")
			}
			if tc.reason == "verified" || tc.reason == "identified" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				if tc.reason == "wrong_model" {
					require.ErrorContains(t, err, "select the requested model with /model")
				}
				if tc.reason == "candidate_changed" {
					require.ErrorContains(t, err, reopenShiftHint)
				}
			}
		})
	}
}

// NERD-5262: exercise the real adapter on the frozen preimage with isolated
// stand-ins. The plugin's actual CLI is tested separately above.
func TestShiftOpenRecordsTheLaunchBeforeStartingClaude(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	session := s.state("session")
	require.Regexp(t, `^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`, session)
	require.Regexp(t, `^[0-9]{13}$`, s.state("launched_at"))
	require.Equal(t, "split-terminal", s.state("terminal"))
	require.Equal(t, "old-terminal", s.state("old_terminal"))
	require.Equal(t, "new", s.state("new"))
	require.Equal(t, "0", s.state("model_pid"))
	require.Contains(t, s.read("starts"), " --session-id "+session)
	require.NoFileExists(t, s.path("calls"), "open asks the plugin nothing")
	require.NoFileExists(t, s.path("sleeps"), "open never polls for a report")
	require.Error(t, s.run("old", "verified", "open"), "a second open is refused")
}

func TestShiftOpenKeepsAStubWhenTheSplitFails(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	_, err := s.invoke("old", "verified", "open", "SPLIT_FAIL=1")
	require.Error(t, err)
	require.NotContains(t, s.read("current/state"), "new=")
	before := s.snapshot()
	out, err := s.invoke("old", "verified", "status")
	require.NoError(t, err)
	require.Contains(t, out, "no candidate pane was recorded")
	require.Equal(t, before, s.snapshot())
	require.Error(t, s.run("old", "verified", "open"), "a stub counts as an open shift")
	out, err = s.invoke("old", "verified", "abort", "split", "lost")
	require.NoError(t, err)
	require.Contains(t, out, "no pane was recorded")
	require.NoFileExists(t, s.path("close"))
	require.NoFileExists(t, s.path("current"))
	require.FileExists(t, s.path("last-aborted/state"))
}

func TestShiftOpenStopsAtASplitWithoutAUsableReply(t *testing.T) {
	t.Parallel()
	for mode, abortSays := range map[string]string{"SPLIT_WITHOUT_TERMINAL=1": "not closed", "SPLIT_JUNK=1": "no pane was recorded"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s := setupShiftScript(t)
			out, err := s.invoke("old", "verified", "open", mode)
			require.Error(t, err)
			require.Contains(t, out, "by hand")
			require.NotContains(t, out, "Traceback")
			require.NoFileExists(t, s.path("starts"))
			// A pane that has gone reads as no terminal, which must not match a record with none.
			out, err = s.invoke("old", "verified", "abort", "NEW_GONE=1")
			require.NoError(t, err)
			require.Contains(t, out, abortSays)
			require.NoFileExists(t, s.path("close"), "an unconfirmed terminal is never closed")
		})
	}
}

func TestShiftOpenRefusesAnOutgoingAgentWithoutATerminal(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.Error(t, s.runEnv("open", "OLD_NO_TERMINAL=1"))
	require.NoFileExists(t, s.path("split"))
	require.NoFileExists(t, s.path("current"))
}

func TestShiftOpenStopsWhenTheShiftEndsDuringTheSplit(t *testing.T) {
	t.Parallel()
	// An abort that has removed current, and one that has written ABORTED but not yet moved current.
	for name, hook := range map[string]string{"current removed": `rm "$SHIFT_ROOT/current"`, "ABORTED written": `touch "$SHIFT_ROOT/current/ABORTED"`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := setupShiftScript(t)
			out, err := s.invoke("old", "verified", "open", "ON_SPLIT="+hook)
			require.Error(t, err)
			require.Contains(t, out, "ended during the split")
			require.NoFileExists(t, s.path("starts"))
			require.Equal(t, "new\n", s.read("close"), "the pane the split made is closed")
		})
	}
}

// An aborted open whose split replies late must not touch the shift that replaced it,
// even when the abort and the reopen happen in the same second.
func TestShiftOpenOfAnAbortedShiftNeverTouchesItsReplacement(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	reopen := `[ -e "$SHIFT_ROOT/nested" ] || { touch "$SHIFT_ROOT/nested"; bash "$SHIFT_ROOT/shift.sh" abort; bash "$SHIFT_ROOT/shift.sh" open; } >/dev/null 2>&1`
	_, err := s.invoke("old", "verified", "open", "ON_SPLIT="+reopen)
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(s.read("starts"), "agent start"), "only the replacement launches Claude")
	replacement, err := os.Readlink(s.path("current"))
	require.NoError(t, err)
	require.Regexp(t, `/20261124-000000-[A-Za-z0-9]{6}$`, replacement)
}

func TestShiftOpenLeavesAnActiveShiftsRecordAlone(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	won := `w="$SHIFT_ROOT/20261124-000000"; mkdir -p "$w"; echo WINNER > "$w/state"; ln -s "$w" "$SHIFT_ROOT/current"`
	_, err := s.invoke("old", "verified", "open", "ON_GET="+won, "SPLIT_FAIL=1")
	require.Error(t, err)
	require.Equal(t, "WINNER\n", s.read("current/state"))
	require.NoFileExists(t, s.path("split"))
	require.Empty(t, s.glob("20261124-000000-*"), "the loser's empty folder is removed")
}

func TestShiftOpenChecksPluginBeforeSplit(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing", "old"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := setupShiftScript(t)
			binary := s.path(name)
			if name == "old" {
				binary = filepath.Join(shiftStubDir, "legacy-plugin")
			}
			out, err := s.invoke("old", "verified", "open", "SHIFT_PLUGIN="+binary)
			require.Error(t, err)
			require.NoFileExists(t, s.path("split"))
			require.NoFileExists(t, s.path("current"))
			require.Contains(t, out, "install a herdr-agentisan build with boss shift check-model")
		})
	}
}

// The candidate is identified once and its model proved before any instructions,
// on every brief.
func TestShiftBriefPinsTheLaunchedClaudeOnce(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.NoError(t, s.run("old", "verified", "brief"))
	require.Equal(t, "100", s.state("model_pid"))
	require.Equal(t, "200", s.state("model_start"))
	require.Contains(t, s.read("calls"), "--session-id "+s.state("session")+" --pid 0 --start 0")
	require.Contains(t, s.read("calls"), "--pid 100 --start 200", "the model is proved after the pin")
	require.True(t, strings.HasPrefix(s.read("cues"), "1 "), "the pin was saved before the cue: %q", s.read("cues"))
	_, err := s.invoke("old", "verified", "brief", "PLUGIN_PID=999")
	require.NoError(t, err)
	require.Equal(t, "100", s.state("model_pid"), "a pinned Claude is never pinned again")
	require.Equal(t, 1, strings.Count(s.read("calls"), "--pid 0"))
	require.Equal(t, 2, strings.Count(s.read("cues"), "boss-shift.md"))
	require.Empty(t, s.glob("current/state.*"))
	require.Error(t, s.run("old", "candidate_changed", "brief"), "a retry rechecks the process")
	require.Equal(t, 2, strings.Count(s.read("cues"), "boss-shift.md"))
}

func TestShiftBriefWithholdsTheCueUntilTheModelIsProved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, env, verdict, pid string }{
		{"not identified", "IDENTIFY_VERDICT=candidate_changed", "verified", "0"},
		{"wrong model", "X=1", "wrong_model", "100"},
		{"no report yet", "X=1", "missing_model", "100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := setupShiftScript(t)
			require.NoError(t, s.run("old", "verified", "open"))
			_, err := s.invoke("old", tc.verdict, "brief", tc.env)
			require.Error(t, err)
			require.Equal(t, tc.pid, s.state("model_pid"), "a refused identity is never saved")
			require.NoFileExists(t, s.path("cues"))
			require.Empty(t, s.glob("current/state.*"))
			require.NoError(t, s.run("old", "verified", "brief"), "brief reruns")
			require.Equal(t, "100", s.state("model_pid"))
			require.FileExists(t, s.path("cues"))
		})
	}
}

// A brief that is already running follows its own shift, not whichever one `current` names later.
func TestShiftBriefStaysWithItsOwnShift(t *testing.T) {
	t.Parallel()
	swap := `rm -f "$SHIFT_ROOT/current"; ln -s "$SHIFT_ROOT/b" "$SHIFT_ROOT/current"`
	for name, when := range map[string]string{"after the identity call": `[[ " $* " = *" --pid 0 "* ]]`, "after the model proof": `[[ " $* " != *" --pid 0 "* ]]`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := setupShiftScript(t)
			require.NoError(t, s.run("old", "verified", "open"))
			require.NoError(t, os.Mkdir(s.path("b"), 0o700))
			s.write("b/state", "replacement\n")
			_, err := s.invoke("old", "verified", "brief", "ON_CHECK="+when+" && "+swap)
			require.Error(t, err)
			require.Equal(t, "replacement\n", s.read("b/state"), "the replacement's record is never written")
			require.NoFileExists(t, s.path("cues"))
		})
	}
}

// go and takeover stay with the shift they started in: when another replaces it under them,
// they write, cue and close nothing for the replacement.
func TestShiftCommandsStayWithTheirOwnShift(t *testing.T) {
	t.Parallel()
	swap := `[[ " $* " != *" --pid 0 "* ]] && { rm -f "$SHIFT_ROOT/current"; ln -s "$SHIFT_ROOT/b" "$SHIFT_ROOT/current"; }`
	for verb, pane := range map[string]string{"go": "old", "takeover": "new"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			s := setupShiftScript(t)
			require.NoError(t, s.run("old", "verified", "open"))
			require.NoError(t, s.run("old", "verified", "brief"))
			s.account()
			if verb == "takeover" {
				require.NoError(t, s.run("old", "verified", "go"))
			}
			replacement := "old=old\nold_terminal=old-terminal\nboss=ha-boss\nnext=ha-boss-next\nnew=new\n"
			require.NoError(t, os.Mkdir(s.path("b"), 0o700))
			s.write("b/state", replacement)
			_, err := s.invoke(pane, "verified", verb, "ON_CHECK="+swap)
			require.Error(t, err)
			require.Equal(t, replacement, s.read("b/state"))
			for _, f := range []string{"b/GO", "b/GO.tmp", "b/GO-CUED", "close", "rename"} {
				require.NoFileExists(t, s.path(f))
			}
		})
	}
}

// An abort that starts while a brief holds the shift waits for it, so the cue goes to the shift
// it was proved for: herdr resolves the prompt's target by name, when the prompt arrives.
func TestShiftAbortWaitsForARunningBrief(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	abort := `[[ " $* " != *" --pid 0 "* ]] && { bash "$SHIFT_ROOT/shift.sh" abort 9>&- >"$SHIFT_ROOT/abort.out" 2>&1 & /bin/sleep 0.5; }`
	_, err := s.invoke("old", "verified", "brief", "ON_CHECK="+abort)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(s.read("cues"), "boss-shift.md"))
	require.Eventually(t, func() bool { _, err := os.Lstat(s.path("last-aborted")); return err == nil }, 10*time.Second, 50*time.Millisecond)
}

func TestShiftAbortQueuedBehindATakeoverClosesNothing(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.NoError(t, s.run("old", "verified", "brief"))
	s.account()
	require.NoError(t, s.run("old", "verified", "go"))
	abort := `[[ " $* " != *" --pid 0 "* ]] && { bash "$SHIFT_ROOT/shift.sh" abort 9>&- >"$SHIFT_ROOT/abort.out" 2>&1 & /bin/sleep 0.5; }`
	_, err := s.invoke("new", "verified", "takeover", "ON_CHECK="+abort)
	require.NoError(t, err)
	require.Eventually(t, func() bool { b, _ := os.ReadFile(s.path("abort.out")); return len(b) > 0 }, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, "old\n", s.read("close"), "only the old pane was closed")
}

func TestShiftOpenClosesItsPaneWhenTheShiftEndedDuringTheStart(t *testing.T) {
	t.Parallel()
	hook := `touch "$SHIFT_ROOT/pane-get-fails"; bash "$SHIFT_ROOT/shift.sh" abort >/dev/null 2>&1; rm "$SHIFT_ROOT/pane-get-fails"`
	for _, fail := range []string{"0", "1"} { // the launch reply may be lost
		s := setupShiftScript(t)
		out, err := s.invoke("old", "verified", "open", "ON_START="+hook, "START_FAIL="+fail)
		require.Error(t, err)
		require.Contains(t, out, "ended while Claude started", "start failing: "+fail)
		require.Equal(t, "new\n", s.read("close"))
	}
}

// A launch that fails with no abort leaves the pane for the operator and says so; a
// cleanup that finds another terminal on the pane closes nothing.
func TestShiftOpenFailedStartAndCleanupGuards(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	out, err := s.invoke("old", "verified", "open", "START_FAIL=1")
	require.Error(t, err)
	require.Contains(t, out, "did not start")
	require.NotContains(t, out, "opened:")
	require.NoFileExists(t, s.path("close"))

	s = setupShiftScript(t)
	hook := `touch "$SHIFT_ROOT/pane-get-fails"; bash "$SHIFT_ROOT/shift.sh" abort >/dev/null 2>&1; rm "$SHIFT_ROOT/pane-get-fails"`
	_, err = s.invoke("old", "verified", "open", "ON_START="+hook, "NEW_TERMINAL=reused-terminal")
	require.Error(t, err)
	require.NoFileExists(t, s.path("close"), "another terminal's pane is not closed")
}

// A late open, whose shift was promoted while it waited, must not close the new boss.
func TestShiftOpenLeavesAPromotedShiftsPaneAlone(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.runEnv("open", `ON_START=rm -f "$SHIFT_ROOT/current"`))
	require.NoFileExists(t, s.path("close"))
}

func TestShiftTakeoverProvesTheCandidateAfterLookingUpTheOldPane(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.NoError(t, s.run("old", "verified", "brief"))
	s.account()
	require.NoError(t, s.run("old", "verified", "go"))
	_, err := s.invoke("new", "verified", "takeover", `ON_GET_OLD=touch "$SHIFT_ROOT/dead"`)
	require.Error(t, err)
	require.NoFileExists(t, s.path("close"))
}

// The stand-ins are written once, by TestMain: one written in a parallel test can be
// inherited, open for writing, by another test's fork (ETXTBSY, NERD-5645).
func TestShiftStandInsAreWrittenOnce(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(setupShiftScript(t).dir)
	require.NoError(t, err)
	for _, e := range entries {
		info, err := e.Info()
		require.NoError(t, err)
		require.Zero(t, info.Mode()&0o111, "%s is an executable written by a test", e.Name())
	}
}

func TestShiftGoWritesOnlyAfterAPinnedProof(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	s.account()
	out, err := s.invoke("old", "verified", "go")
	require.Error(t, err)
	require.Contains(t, out, "no candidate process pin")
	require.NoFileExists(t, s.path("current/GO"))
	require.NoFileExists(t, s.path("calls"), "nothing is pinned by go")
	require.NoError(t, s.run("old", "verified", "brief"))
	for _, verdict := range []string{"wrong_model", "missing_model", "stale_report", "candidate_changed"} {
		require.Error(t, s.run("old", verdict, "go"), verdict)
		require.NoFileExists(t, s.path("current/GO"))
	}
	require.NoError(t, s.run("old", "verified", "go"))
	require.FileExists(t, s.path("current/GO"))
	require.FileExists(t, s.path("current/GO-CUED"))
	require.NoFileExists(t, s.path("current/GO.tmp"))
	calls := s.read("calls")
	require.Contains(t, calls, "boss shift check-model --pane new --terminal split-terminal --name ha-boss-next --model fable --launched-at ")
	require.Contains(t, calls, "--pid 100 --start 200")
	require.NotContains(t, calls, "pinned-at")
	require.Error(t, s.run("old", "verified", "go"), "a recorded GO cannot be repeated")
}

func TestShiftGoNeverCuesWithoutAWrittenGO(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.NoError(t, s.run("old", "verified", "brief"))
	s.account()
	require.NoError(t, os.Mkdir(s.path("current/GO.tmp"), 0o700), "GO.tmp cannot be written")
	out, err := s.invoke("old", "verified", "go")
	require.Error(t, err)
	require.Contains(t, out, "GO could not be written")
	require.Zero(t, strings.Count(s.read("cues"), "takeover"))
	require.NoFileExists(t, s.path("current/GO"))
	require.NoFileExists(t, s.path("current/GO-CUED"))
}

func TestShiftGoRetriesAFailedCue(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.NoError(t, s.run("old", "verified", "brief"))
	s.account()
	_, err := s.invoke("old", "verified", "go", "FAIL_PROMPT=1")
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(s.read("cues"), "boss-shift.md"))
	require.FileExists(t, s.path("current/GO"), "the failed cue left the GO it recorded")
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(s.path("current/GO"), old, old))
	out, err := s.invoke("old", "wrong_model", "go")
	require.Error(t, err)
	require.NotContains(t, out, "GO not written", "the recorded GO is still there")
	require.NoError(t, s.run("old", "verified", "go"))
	info, err := os.Stat(s.path("current/GO"))
	require.NoError(t, err)
	require.True(t, info.ModTime().Equal(old), "the retry kept the GO it found")
	require.Equal(t, 1, strings.Count(s.read("cues"), "takeover"))
	require.Error(t, s.run("old", "verified", "go"), "a delivered cue cannot be repeated")
}

func TestShiftTakeoverClosesOnlyAVerifiedOldTerminal(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.NoError(t, s.run("old", "verified", "brief"))
	s.account()
	require.NoError(t, s.run("old", "verified", "go"))
	for _, verdict := range []string{"wrong_model", "candidate_changed", "missing_model"} {
		require.Error(t, s.run("new", verdict, "takeover"), verdict)
		require.NoFileExists(t, s.path("close"))
	}
	_, err := s.invoke("new", "verified", "takeover", "OLD_TERMINAL=reused-terminal")
	require.Error(t, err, "the old pane id now holds another terminal")
	require.NoFileExists(t, s.path("close"))
	require.NoFileExists(t, s.path("rename"))
	require.NoError(t, s.run("new", "verified", "takeover"))
	require.Equal(t, "old\n", s.read("close"))
	require.Equal(t, "ha-boss-next ha-boss\n", s.read("rename"))
	require.Contains(t, s.read("calls"), "--pid 100 --start 200")
	require.NoFileExists(t, s.path("current"))
}

func TestShiftAbortClosesOnlyTheRecordedTerminal(t *testing.T) {
	t.Parallel()
	t.Run("a reused pane id", func(t *testing.T) {
		t.Parallel()
		s := setupShiftScript(t)
		require.NoError(t, s.run("old", "verified", "open"))
		out, err := s.invoke("old", "verified", "abort", "NEW_TERMINAL=reused-terminal")
		require.NoError(t, err)
		require.Contains(t, out, "not closed")
		require.NotContains(t, out, "by hand", "a refused terminal may be someone else's pane")
		require.NotContains(t, out, "incoming pane closed")
		require.NoFileExists(t, s.path("close"))
		require.NoFileExists(t, s.path("current"), "the shift record still ends")
	})
	t.Run("the candidate's terminal", func(t *testing.T) {
		t.Parallel()
		s := setupShiftScript(t)
		require.NoError(t, s.run("old", "verified", "open"))
		out, err := s.invoke("old", "verified", "abort")
		require.NoError(t, err)
		require.Equal(t, "new\n", s.read("close"))
		require.Contains(t, out, "closed candidate pane new")
	})
}

func TestShiftStatusShowsTheVerdictAndWritesNothing(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	before := s.snapshot()
	out, err := s.invoke("old", "verified", "status")
	require.NoError(t, err)
	require.Contains(t, out, "model_pid=0")
	require.Equal(t, before, s.snapshot(), "status never pins")
	require.NoError(t, s.run("old", "verified", "brief"))
	before = s.snapshot()
	out, err = s.invoke("old", "wrong_model", "status")
	require.NoError(t, err)
	require.Contains(t, out, `"reason":"wrong_model"`)
	require.Equal(t, before, s.snapshot())
}

// The script asks the verbs for its own folder and the role, and what it sends is what the real verbs take.
func TestShiftScriptAsksTheRealVerbsWhatTheyTake(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.NoError(t, os.Mkdir(s.path("roles"), 0o700))
	s.write("roles/boss-shift.md", "role\n")
	require.NoError(t, s.run("old", "verified", "brief"))
	own, err := os.Readlink(s.path("current"))
	require.NoError(t, err)
	id := filepath.Base(own)
	account := "attempt: " + id + "\nround: 1\n\nAccount.\n<!-- end shift round -->\n"
	s.write("current/account-1.md", account)
	_, _ = s.invoke("old", "verified", "wait", "1")
	_, _ = s.invoke("old", "verified", "wait")
	for _, fail := range []string{"BRIEF_FAIL=1", "BRIEF_FAIL=2"} { // the verb refuses, or names no role
		_, err = s.invoke("old", "verified", "brief", fail)
		require.Error(t, err, fail)
	}
	require.Equal(t, 1, strings.Count(s.read("cues"), "boss-shift.md"), "a refused brief sends nothing")
	require.Contains(t, s.read("cues"), s.path("roles/boss-shift.md"))
	none := func(string) (string, bool) { return "", false }
	asked := strings.Fields(strings.SplitN(s.read("briefs"), "\n", 2)[0])
	require.Equal(t, []string{"boss", "shift", "brief", "--record", own, "--attempt", id, "--role", s.path("roles/boss-shift.md")}, asked)
	out, _, err := runOut(t, none, asked...)
	require.NoError(t, err)
	require.Equal(t, s.path("roles/boss-shift.md"), oneJSONObject(t, out)["role"])
	waits := strings.Split(strings.TrimSpace(s.read("waits")), "\n")
	require.Equal(t, []string{
		"boss shift wait --record " + own + " --attempt " + id + " --round 1 --timeout 2m",
		"boss shift wait --record " + own + " --attempt " + id + " --timeout 2m",
	}, waits)
	sum := sha256.Sum256([]byte(account))
	for _, line := range waits {
		out, _, err := runOut(t, none, strings.Fields(line)...)
		require.NoError(t, err, line)
		verdict := oneJSONObject(t, out)
		require.Equal(t, "published", verdict["status"], line)
		require.Equal(t, account, verdict["text"], line)
		require.Equal(t, fmt.Sprintf("%x", sum), verdict["sha256"], line)
	}
	_, _, err = runOut(t, none, "boss", "shift", "wait", "--record", "relative", "--attempt", id)
	require.ErrorContains(t, err, "invalid boss shift record")
}

// The adapter is a patch of the released one: it must apply to the installed script, whose model
// default is the boss's own line, and only to the script v1 left.
func TestShiftV2PatchAppliesToTheInstalledScript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "shift.sh")
	preimage, err := os.ReadFile(filepath.Join("testdata", "shift-v0.sh"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(script, preimage, 0o600))
	applyShiftPatch(t, script, "boss-shift-v1.patch")
	v1, err := os.ReadFile(script)
	require.NoError(t, err)
	require.Equal(t, "ce75b6c4d4cd9d666e259dc41f8623ff9b8537696e504407941d59e5055fb1c1", fmt.Sprintf("%x", sha256.Sum256(v1)))
	local := strings.NewReplacer("MODEL=${SHIFT_MODEL:-fable}", "MODEL=${SHIFT_MODEL:-claude-opus-5-5}", "# The boss runs on Fable", "# The boss runs on Opus").Replace(string(v1))
	require.NotEqual(t, string(v1), local)
	require.NoError(t, os.WriteFile(script, []byte(local), 0o600))
	applyShiftPatch(t, script, "boss-shift-v2.patch")
	out, err := os.ReadFile(script)
	require.NoError(t, err)
	require.Contains(t, string(out), "claude-opus-5-5")
	require.NoError(t, os.WriteFile(script, preimage, 0o600))
	patch, err := os.ReadFile(filepath.Join("..", "..", "docs", "boss-shift-v2.patch"))
	require.NoError(t, err)
	apply := exec.CommandContext(t.Context(), "patch", "--batch", "--dry-run", "--fuzz=0", script) // #nosec G204 -- scratch script only
	apply.Stdin = bytes.NewReader(patch)
	require.Error(t, apply.Run(), "v2 applies only to the script v1 left")
}

// A lowercase `started` in the caller's environment must not make a good start look failed.
func TestShiftOpenIgnoresAnExportedStarted(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	s.write("bashenv", "export started=exported\n")
	out, err := s.invoke("old", "verified", "open", "BASH_ENV="+s.path("bashenv"))
	require.NoError(t, err)
	require.Contains(t, out, "opened:")
}

// The install blocks in the guide run verbatim, and a second run changes nothing: GNU patch
// would otherwise take the applied patch for a reversed one and undo it.
func TestShiftGuideInstallBlocksAreSafeToRepeat(t *testing.T) {
	t.Parallel()
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "boss-shift-model.md"))
	require.NoError(t, err)
	preimage, err := os.ReadFile(filepath.Join("testdata", "shift-v0.sh"))
	require.NoError(t, err)
	script := filepath.Join(t.TempDir(), "shift.sh")
	require.NoError(t, os.WriteFile(script, preimage, 0o600))
	for _, name := range []string{"boss-shift-v1.patch", "boss-shift-v2.patch"} {
		var block string
		for b := range strings.SplitSeq(string(guide), "```sh\n") {
			if b, _, ok := strings.Cut(b, "```"); ok && strings.Contains(b, "docs/"+name) {
				block = b
			}
		}
		require.NotEmpty(t, block, name)
		var lines []string
		for line := range strings.SplitSeq(block, "\n") {
			if strings.HasPrefix(line, "script=") {
				line = "script=" + script
			}
			lines = append(lines, line)
		}
		install := func() error {
			cmd := exec.CommandContext(t.Context(), "bash", "-c", strings.Join(lines, "\n")) // #nosec G204 -- the guide's own block, on a scratch script
			cmd.Dir = filepath.Join("..", "..")
			out, err := cmd.CombinedOutput()
			t.Logf("%s: %s", name, out)
			return err
		}
		require.NoError(t, install(), name)
		applied, err := os.ReadFile(script)
		require.NoError(t, err)
		require.Error(t, install(), name+" run twice")
		again, err := os.ReadFile(script)
		require.NoError(t, err)
		require.Equal(t, string(applied), string(again), name+" run twice changed the script")
	}
}

// The hooks job strips trailing whitespace: a patch with any would be rewritten and stop applying.
func TestShiftPatchesSurviveTheWhitespaceHook(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"boss-shift-v1.patch", "boss-shift-v2.patch"} {
		patch, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
		require.NoError(t, err)
		for n, line := range strings.Split(strings.TrimSuffix(string(patch), "\n"), "\n") {
			require.Equal(t, strings.TrimRight(line, " \t"), line, "%s line %d", name, n+1)
		}
	}
}

const reopenShiftHint = "abort and open again"

type shiftScript struct {
	t   *testing.T
	dir string
}

// shiftStubDir holds the stand-in herdr, herdr-agentisan, date and sleep, and a plugin
// that predates check-model. TestMain writes them once, before any test can fork: a file
// written while another test forks stays open for writing in the child until it execs,
// and running it then fails with "Text file busy" (ETXTBSY, NERD-5645).
var shiftStubDir string

var shiftStubs = map[string]string{
	"date": `#!/bin/bash
[ "$1" != +%Y%m%d-%H%M%S ] || { echo 20261124-000000; exit; }
exec /bin/date "$@"
`,
	"sleep": "#!/bin/bash\nprintf '%s\\n' \"$1\" >> \"$SHIFT_ROOT/sleeps\"\n",
	"herdr": `#!/bin/bash
case "$1 $2" in
  "agent get") [ ! -e "$SHIFT_ROOT/close" ] || exit 1
               if [ "$3" = ha-boss ]; then
                 [ "$OLD_NO_TERMINAL" = 1 ] && echo '{"result":{"agent":{"pane_id":"old"}}}' || echo '{"result":{"agent":{"pane_id":"old","terminal_id":"old-terminal"}}}'
                 eval "$ON_GET"
               else echo '{"result":{"agent":{"pane_id":"new","tokens":{"model":"fable"}}}}'; fi ;;
  "pane split") touch "$SHIFT_ROOT/split"
                eval "$ON_SPLIT"
                [ "${SPLIT_FAIL:-0}" = 0 ] || exit 7
                if [ "${SPLIT_JUNK:-0}" = 1 ]; then
                  echo junk
                elif [ "${SPLIT_WITHOUT_TERMINAL:-0}" = 1 ]; then
                  echo '{"result":{"pane":{"pane_id":"new"}}}'
                else
                  echo '{"result":{"pane":{"pane_id":"new","terminal_id":"split-terminal"}}}'
                fi ;;
  "pane get") case "$3" in
                old) eval "$ON_GET_OLD"; echo "{\"result\":{\"pane\":{\"pane_id\":\"old\",\"terminal_id\":\"${OLD_TERMINAL:-old-terminal}\"}}}" ;;
                new) [ "${NEW_GONE:-0}" = 0 ] && [ ! -e "$SHIFT_ROOT/pane-get-fails" ] || exit 1
                     echo "{\"result\":{\"pane\":{\"pane_id\":\"new\",\"terminal_id\":\"${NEW_TERMINAL:-split-terminal}\"}}}" ;;
                *) exit 1 ;;
              esac ;;
  "agent start") printf '%s\n' "$*" >> "$SHIFT_ROOT/starts"; echo "starting candidate" >&2; eval "$ON_START"; [ "${START_FAIL:-0}" = 0 ] ;;
  "agent prompt") [ "${FAIL_PROMPT:-0}" = 0 ] || exit 9
                  echo "$(grep -c '^model_pid=100$' "$SHIFT_ROOT/current/state") $*" >> "$SHIFT_ROOT/cues" ;;
  "pane close") echo "$3" >> "$SHIFT_ROOT/close" ;;
  "agent rename") echo "$3 $4" >> "$SHIFT_ROOT/rename" ;;
  *) exit 9 ;;
esac
`,
	"herdr-agentisan": `#!/bin/bash
[ "$4" != --help ] || { printf "%s\n" "--session-id"; exit 0; }
rec=; role=; prev=; for a in "$@"; do case "$prev" in --record) rec=$a ;; --role) role=$a ;; esac; prev=$a; done
case "$1 $2 $3" in
  "boss shift wait") printf '%s\n' "$*" >> "$SHIFT_ROOT/waits"; echo '{"status":"timeout"}'; exit 1 ;;
  "boss shift brief") printf '%s\n' "$*" >> "$SHIFT_ROOT/briefs"; case "${BRIEF_FAIL:-0}" in 1) exit 1 ;; 2) echo '{}'; exit 0 ;; esac
    printf '{"record":"%s","attempt":"%s","role":"%s"}\n' "$rec" "${rec##*/}" "$role"; exit 0 ;;
  "boss shift check-model") ;;
  *) exit 9 ;;
esac
printf '%s\n' "$*" >> "$SHIFT_ROOT/calls"
eval "$ON_CHECK"
verdict=$MODEL_VERDICT
[ ! -e "$SHIFT_ROOT/dead" ] || verdict=claude_unavailable
[[ " $* " != *" --pid 0 "* ]] || verdict=${IDENTIFY_VERDICT:-identified}
printf '{"reason":"%s","pid":%s,"start":200}\n' "$verdict" "${PLUGIN_PID:-100}"
[[ "$verdict" = verified || "$verdict" = identified ]] || { echo "probe refused" >&2; exit 1; }
`,
	"legacy-plugin": "#!/bin/bash\necho legacy-help\n",
}

func writeShiftStubs(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("shift stand-ins: %w", err)
	}
	for name, body := range shiftStubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // executable stand-ins
			return fmt.Errorf("shift stand-in %s: %w", name, err)
		}
	}
	return nil
}

func setupShiftScript(t *testing.T) shiftScript {
	t.Helper()
	s := shiftScript{t: t, dir: t.TempDir()}
	preimage, err := os.ReadFile(filepath.Join("testdata", "shift-v0.sh"))
	require.NoError(t, err)
	script := s.path("shift.sh")
	require.NoError(t, os.WriteFile(script, preimage, 0o600))
	applyShiftPatch(t, script, "boss-shift-v1.patch")
	applyShiftPatch(t, script, "boss-shift-v2.patch")
	return s
}

func applyShiftPatch(t *testing.T, script, name string) {
	t.Helper()
	patch, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
	require.NoError(t, err)
	apply := exec.CommandContext(t.Context(), "patch", "--batch", "--fuzz=0", script) // #nosec G204 -- scratch script only
	apply.Stdin = bytes.NewReader(patch)
	out, err := apply.CombinedOutput()
	require.NoError(t, err, "%s: %s", name, out)
}

func (s shiftScript) path(name string) string {
	return filepath.Join(s.dir, name)
}

func (s shiftScript) run(pane, verdict, verb string) error {
	s.t.Helper()
	_, err := s.invoke(pane, verdict, verb)
	return err
}

// invoke runs one shift.sh verb. Overrides of the form NAME=value reach the
// stand-ins; anything else is an argument to the verb.
func (s shiftScript) invoke(pane, verdict, verb string, overrides ...string) (string, error) {
	s.t.Helper()
	var env, args []string
	for _, o := range overrides {
		if name, _, ok := strings.Cut(o, "="); ok && name == strings.ToUpper(name) {
			env = append(env, o)
		} else {
			args = append(args, o)
		}
	}
	cmd := exec.CommandContext(s.t.Context(), "bash", append([]string{s.path("shift.sh"), verb}, args...)...) // #nosec G204 -- scratch script only
	cmd.Env = append([]string{
		"HERDR_SOCKET_PATH=" + s.path("herdr.sock"), "SHIFT_REPO=" + s.dir,
		"PATH=" + shiftStubDir + ":/usr/bin:/bin", "HERDR_PANE_ID=" + pane, "SHIFT_ROOT=" + s.dir,
		"SHIFT_PLUGIN=" + filepath.Join(shiftStubDir, "herdr-agentisan"), "MODEL_VERDICT=" + verdict,
	}, env...)
	out, err := cmd.CombinedOutput()
	s.t.Logf("%s %s: %s", verb, verdict, out)
	return string(out), err
}

func (s shiftScript) write(name, content string) {
	s.t.Helper()
	require.NoError(s.t, os.WriteFile(s.path(name), []byte(content), 0o600))
}

func (s shiftScript) read(name string) string {
	s.t.Helper()
	contents, err := os.ReadFile(s.path(name))
	require.NoError(s.t, err)
	return string(contents)
}

func (s shiftScript) runEnv(verb string, env ...string) error {
	s.t.Helper()
	_, err := s.invoke("old", "verified", verb, env...)
	return err
}

func (s shiftScript) glob(pattern string) []string {
	s.t.Helper()
	found, err := filepath.Glob(s.path(pattern))
	require.NoError(s.t, err)
	return found
}

// snapshot is every file of the open shift, by name, with its bytes.
func (s shiftScript) snapshot() map[string]string {
	s.t.Helper()
	files := map[string]string{}
	for _, name := range s.glob("current/*") {
		contents, err := os.ReadFile(name) // #nosec G304 -- the test's own scratch directory
		require.NoError(s.t, err)
		files[filepath.Base(name)] = string(contents)
	}
	return files
}

func (s shiftScript) state(key string) string {
	s.t.Helper()
	var found string
	for line := range strings.SplitSeq(s.read("current/state"), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			found = value
		}
	}
	require.NotEmpty(s.t, found, "missing %s in state", key)
	return found
}

func (s shiftScript) account() {
	s.t.Helper()
	s.write("current/account-1.md", "Account.\n")
}

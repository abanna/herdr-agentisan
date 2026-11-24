package cli_test

import (
	"bytes"
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
	fresh := time.Now().UnixMilli()
	tests := []struct {
		name, model, requested, terminal, reason string
		stamp, launch                            int64
		pin                                      uint64
	}{
		{"argv only", "", "fable", "term", "missing_model", 0, fresh, 0},
		{"before launch", "fable", "fable", "term", "stale_report", fresh - 1, fresh, 0},
		{"before Claude", "fable", "fable", "term", "stale_report", started.UnixMilli() - 1, started.UnixMilli() - 2, 0},
		{"future", "fable", "fable", "term", "invalid_report_time", fresh + 60000, fresh, 0},
		{"wrong terminal", "fable", "fable", "different", "candidate_changed", fresh, fresh, 0},
		{"reused PID", "fable", "fable", "term", "candidate_changed", fresh, fresh, proc.Start + 1},
		{"substring", "claude-unfable-5-2", "fable", "term", "wrong_model", fresh, fresh, 0},
		{"suffix", "claude-fable-5-2-sonnet", "fable", "term", "wrong_model", fresh, fresh, 0},
		{"exact", "fable", "fable", "term", "verified", fresh, fresh, proc.Start},
		{"family ID", "claude-fable-5-2", "fable", "term", "verified", fresh, fresh, 0},
		{"date-suffixed Fable ID", "claude-fable-5-2-20261123", "fable", "term", "verified", fresh, fresh, 0},
		{"other family ID", "claude-sonnet-4-6-20261123", "fable", "term", "wrong_model", fresh, fresh, 0},
		{"configured other family", "claude-sonnet-4-6-20261123", "claude-sonnet-4-6-20261123", "term", "verified", fresh, fresh, 0},
		{"configured ID is exact", "claude-fable-5-2-20261123", "claude-fable-5-2", "term", "wrong_model", fresh, fresh, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agent := herdr.Agent{PaneID: "p", TerminalID: tc.terminal, Name: "next", Agent: "claude"}
			pane := herdr.PaneInfo{PaneID: "p", TerminalID: tc.terminal, Agent: "claude", Tokens: map[string]string{
				"model": tc.model, "model_at": strconv.FormatInt(tc.stamp, 10),
			}}
			info := herdr.ProcessInfo{PaneID: "p", ForegroundProcesses: []herdr.Process{
				{PID: uint32(os.Getpid()), Argv: []string{"claude", "--model", "fable"}}, // #nosec G115 -- OS PID fits in a uint32
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
				fmt.Sprintf("--launched-at=%d", tc.launch),
			}
			if tc.pin != 0 {
				args = append(args, fmt.Sprintf("--pid=%d", os.Getpid()), fmt.Sprintf("--start=%d", tc.pin))
			}
			out, _, err := runOut(t, lookup, args...)
			verdict := oneJSONObject(t, out)
			require.Equal(t, tc.reason, verdict["reason"])
			require.Equal(t, tc.requested, verdict["requested_model"])
			if tc.reason != "candidate_changed" {
				require.Equal(t, tc.model, verdict["observed_model"])
				require.Equal(t, float64(os.Getpid()), verdict["pid"], "record the process even when the model is missing")
			}
			if tc.reason == "verified" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// NERD-5262: exercise the real adapter on the frozen preimage with isolated
// stand-ins. The plugin's actual CLI is tested separately above.
func TestShiftScriptCallsVerifierAtOpenAndGo(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	require.Contains(t, s.read("calls"), shiftCheck)
	require.NoError(t, s.run("old", "verified", "brief"))
	require.Contains(t, s.read("cues"), "INCOMING.md")
	require.Error(t, s.run("old", "verified", "go"), "the outgoing boss still needs an account")
	s.account()
	require.Error(t, s.run("old", "wrong_model", "go"))
	require.NoFileExists(t, s.path("current/GO"))
	require.Contains(t, s.read("calls"), "--pid 100 --start 200")
	require.Equal(t, 2, strings.Count(s.read("calls"), shiftCheck))
	require.Error(t, s.run("old", "missing_model", "go"))
	require.NoFileExists(t, s.path("current/GO"))
	require.NoError(t, s.run("old", "verified", "go"))
	require.FileExists(t, s.path("current/GO"))
	require.NoFileExists(t, s.path("current/GO.tmp"))
	require.Error(t, s.run("old", "verified", "go"), "a recorded GO cannot be repeated")
	beforeStatus := s.read("calls")
	require.NoError(t, s.run("old", "wrong_model", "status"))
	require.Equal(t, 1, strings.Count(strings.TrimPrefix(s.read("calls"), beforeStatus), shiftCheck))
	require.NoError(t, os.Remove(s.path("current")))
	require.Error(t, s.run("old", "wrong_model", "open"))
	require.Contains(t, s.read("current/state"), "model_pid=100")
}

func TestShiftScriptRetriesMissingModelWithPinnedProcess(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "missing_once", "open"))
	launch := s.state("launched_at")
	require.Regexp(t, `^[0-9]{13}$`, launch)
	require.Equal(t,
		shiftCheck+launch+" --pid 0 --start 0\n"+shiftCheck+launch+" --pid 100 --start 200\n",
		s.read("calls"))
}

func TestShiftScriptWithholdsCloseAfterGoOnModelRefusal(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	s.account()
	require.NoError(t, s.run("old", "verified", "go"))
	err := s.run("new", "wrong_model", "takeover")
	require.NoFileExists(t, s.path("close"), "a refused takeover must never try to close the old pane")
	require.NoFileExists(t, s.path("rename"))
	require.Error(t, err)
	require.FileExists(t, s.path("current/GO"))
	require.NoFileExists(t, s.path("current/DONE"))
	require.True(t, strings.HasSuffix(s.read("calls"), "--pid 100 --start 200\n"))

	require.NoError(t, s.run("new", "verified", "takeover"))
	require.Equal(t, "old\n", s.read("close"))
	require.Equal(t, "ha-boss-next ha-boss\n", s.read("rename"))
}

const shiftCheck = "boss shift check-model --pane new --terminal split-terminal --name ha-boss-next --model fable --launched-at "

type shiftScript struct {
	t   *testing.T
	dir string
}

func setupShiftScript(t *testing.T) shiftScript {
	t.Helper()
	s := shiftScript{t: t, dir: t.TempDir()}
	patch, err := os.ReadFile(filepath.Join("..", "..", "docs", "boss-shift-v1.patch"))
	require.NoError(t, err)
	preimage, err := os.ReadFile(filepath.Join("testdata", "shift-v0.sh"))
	require.NoError(t, err)
	script := s.path("shift.sh")
	require.NoError(t, os.WriteFile(script, preimage, 0o600))
	apply := exec.CommandContext(t.Context(), "patch", "--batch", "--fuzz=0", script) // #nosec G204 -- scratch script only
	apply.Stdin = bytes.NewReader(patch)
	out, err := apply.CombinedOutput()
	require.NoError(t, err, "%s", out)

	for name, body := range map[string]string{
		"sleep": "#!/bin/bash\nexit 0\n",
		"herdr": `#!/bin/bash
case "$1 $2" in
  "agent get") [ ! -e "$SHIFT_ROOT/close" ] || exit 1
               echo '{"result":{"agent":{"pane_id":"old"}}}' ;;
  "pane split") echo '{"result":{"pane":{"pane_id":"new","terminal_id":"split-terminal"}}}' ;;
  "agent start") ;;
  "agent prompt") echo "$*" >> "$SHIFT_ROOT/cues" ;;
  "pane close") echo "$3" >> "$SHIFT_ROOT/close" ;;
  "agent rename") echo "$3 $4" >> "$SHIFT_ROOT/rename" ;;
  *) exit 9 ;;
esac
`,
		"herdr-agentisan": `#!/bin/bash
verdict=$MODEL_VERDICT
if [ "$verdict" = missing_once ]; then
  if [ -e "$SHIFT_ROOT/calls" ]; then verdict=verified; else verdict=missing_model; fi
fi
printf '%s\n' "$*" >> "$SHIFT_ROOT/calls"
[ "$1 $2 $3" = "boss shift check-model" ] || exit 9
printf '{"reason":"%s","pid":100,"start":200}\n' "$verdict"
[ "$verdict" = verified ]
`,
	} {
		require.NoError(t, os.WriteFile(s.path(name), []byte(body), 0o700)) // #nosec G306 -- executable stand-ins in the scratch directory
	}
	return s
}

func (s shiftScript) path(name string) string {
	return filepath.Join(s.dir, name)
}

func (s shiftScript) run(pane, verdict, verb string) error {
	s.t.Helper()
	cmd := exec.CommandContext(s.t.Context(), "bash", s.path("shift.sh"), verb) // #nosec G204 -- scratch script only
	cmd.Env = []string{
		"HERDR_SOCKET_PATH=" + s.path("herdr.sock"), "SHIFT_REPO=" + s.dir,
		"PATH=" + s.dir + ":/usr/bin:/bin", "HERDR_PANE_ID=" + pane, "SHIFT_ROOT=" + s.dir,
		"SHIFT_PLUGIN=" + s.path("herdr-agentisan"), "MODEL_VERDICT=" + verdict, "SHIFT_MODEL_SEEN=1",
	}
	out, err := cmd.CombinedOutput()
	s.t.Logf("%s %s: %s", verb, verdict, out)
	return err
}

func (s shiftScript) read(name string) string {
	s.t.Helper()
	contents, err := os.ReadFile(s.path(name))
	require.NoError(s.t, err)
	return string(contents)
}

func (s shiftScript) state(key string) string {
	s.t.Helper()
	for line := range strings.SplitSeq(s.read("current/state"), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return value
		}
	}
	s.t.Fatalf("missing %s in state", key)
	return ""
}

func (s shiftScript) account() {
	s.t.Helper()
	require.NoError(s.t, os.WriteFile(s.path("current/account-1.md"), []byte("Account.\n"), 0o600))
}

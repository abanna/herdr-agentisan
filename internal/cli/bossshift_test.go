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
	launch := started.UnixMilli()
	time.Sleep(time.Until(started.Add(7 * time.Second))) // The test process models the pinned Claude.
	fresh := time.Now().UnixMilli()
	tests := []struct {
		name, model, requested, terminal, reason string
		stamp, launch                            int64
		pin                                      uint64
	}{
		{"argv only", "", "fable", "term", "missing_model", 0, launch, 0},
		{"before launch", "fable", "fable", "term", "stale_report", launch, launch + 1, 0},
		{"before Claude", "fable", "fable", "term", "stale_report", launch - 1, launch - 2, 0},
		{"future", "fable", "fable", "term", "invalid_report_time", fresh + 60000, launch, 0},
		{"first observation", "fable", "fable", "term", "awaiting_pin", fresh, launch, 0},
		{"wrong terminal", "fable", "fable", "different", "candidate_changed", fresh, launch, 0},
		{"reused PID", "fable", "fable", "term", "candidate_changed", fresh, launch, proc.Start + 1},
		{"substring", "claude-unfable-5-2", "fable", "term", "wrong_model", fresh, launch, 0},
		{"suffix", "claude-fable-5-2-sonnet", "fable", "term", "wrong_model", fresh, launch, 0},
		{"exact", "fable", "fable", "term", "verified", fresh, launch, proc.Start},
		{"family ID", "claude-fable-5-2", "fable", "term", "verified", fresh, launch, 0},
		{"one-million-context Fable ID", "claude-fable-5-2[1m]", "fable", "term", "verified", fresh, launch, 0},
		{"uppercase Fable suffix", "claude-fable-5-2[1M]", "fable", "term", "wrong_model", fresh, launch, 0},
		{"other Fable context window", "claude-fable-5-2[200k]", "fable", "term", "wrong_model", fresh, launch, 0},
		{"date-suffixed Fable ID", "claude-fable-5-2-20261123", "fable", "term", "verified", fresh, launch, 0},
		{"other family ID", "claude-sonnet-4-6-20261123", "fable", "term", "wrong_model", fresh, launch, 0},
		{"configured other family", "claude-sonnet-4-6-20261123", "claude-sonnet-4-6-20261123", "term", "verified", fresh, launch, 0},
		{"configured ID is exact", "claude-fable-5-2-20261123", "claude-fable-5-2", "term", "wrong_model", fresh, launch, 0},
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
			if tc.reason == "verified" || tc.reason == "wrong_model" || tc.pin != 0 {
				pin := tc.pin
				if pin == 0 {
					pin = proc.Start
				}
				args = append(args, fmt.Sprintf("--pid=%d", os.Getpid()), fmt.Sprintf("--start=%d", pin),
					fmt.Sprintf("--pinned-at=%d", tc.launch+250))
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
				if tc.reason == "wrong_model" {
					require.ErrorContains(t, err, "select the requested model with /model")
				}
				if tc.reason == "candidate_changed" || tc.reason == "awaiting_pin" {
					require.ErrorContains(t, err, reopenShiftHint)
				}
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
	require.Equal(t, 3, strings.Count(s.read("calls"), shiftCheck))
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
	require.Equal(t, "1700000000001", s.state("model_pinned_at"), "take the pin after the first observation")
	require.Equal(t,
		shiftCheck+launch+" --pid 0 --start 0 --pinned-at 0\n"+shiftCheck+launch+" --pid 100 --start 200 --pinned-at "+s.state("model_pinned_at")+"\n",
		s.read("calls"))
}

func TestShiftScriptWaitsForPostPinRefresh(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	out, err := s.invoke("old", "slow_refresh", "open")
	require.NoError(t, err)
	require.Equal(t, 52, strings.Count(s.read("calls"), shiftCheck))
	require.Equal(t, strings.Repeat("2\n", 51), s.read("sleeps"))
	require.Equal(t, 1, strings.Count(out, "waiting up to about a minute"))
	require.NotContains(t, out, "probe refused")
}

func TestShiftScriptExplainsRetryableExhaustion(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	out, err := s.invoke("old", "report_before_pin", "open")
	require.Error(t, err)
	require.Equal(t, 50, strings.Count(s.read("calls"), shiftCheck))
	require.Contains(t, out, "wait up to 60 s, run shift.sh status; abort only on wrong_model or candidate_changed")
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
	require.True(t, strings.HasSuffix(s.read("calls"), "--pid 100 --start 200 --pinned-at "+s.state("model_pinned_at")+"\n"))

	require.NoError(t, s.run("new", "verified", "takeover"))
	require.Equal(t, "old\n", s.read("close"))
	require.Equal(t, "ha-boss-next ha-boss\n", s.read("rename"))
}

func TestShiftScriptRefusesGoWithoutPin(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	s.account()
	s.write("current/state", s.read("current/state")+"model_pinned_at=0\n")
	out, err := s.invoke("old", "verified", "go")
	require.Error(t, err)
	require.Contains(t, out, reopenShiftHint)
	require.NoFileExists(t, s.path("current/GO"))
	require.Equal(t, 2, strings.Count(s.read("calls"), shiftCheck))
}

func TestShiftScriptStatusLeavesInterruptedOpenUnpinned(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	state, _, _ := strings.Cut(s.read("current/state"), "model_pid=")
	state += "model_pid=0\nmodel_start=0\nmodel_pinned_at=0\n"
	s.write("current/state", state)
	s.account()
	out, err := s.invoke("old", "verified", "status")
	require.NoError(t, err)
	require.Contains(t, out, "awaiting_pin")
	require.Equal(t, state, s.read("current/state"))
	out, err = s.invoke("old", "verified", "go")
	require.Error(t, err)
	require.Contains(t, out, reopenShiftHint)
	require.NoFileExists(t, s.path("current/GO"))
}

func TestShiftScriptExplainsStateWithNoPin(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	s.account()
	state, _, _ := strings.Cut(s.read("current/state"), "model_pid=")
	s.write("current/state", state)
	out, err := s.invoke("old", "verified", "go")
	require.Error(t, err)
	require.Contains(t, out, "no candidate process pin")
	require.Contains(t, out, "abort")
}

func TestShiftScriptStopsAtWrongModel(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.Error(t, s.run("old", "wrong_model", "open"))
	require.Equal(t, 2, strings.Count(s.read("calls"), shiftCheck))
}

func TestShiftScriptRechecksRecordedModelAtGo(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	s.account()
	_, err := s.invoke("old", "verified", "go", "SHIFT_MODEL=sonnet")
	require.NoError(t, err)
	require.Equal(t, 3, strings.Count(s.read("calls"), shiftCheck))
}

func TestShiftScriptChecksPluginBeforeSplit(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"missing", "old"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := setupShiftScript(t)
			binary := s.path(name)
			if name == "old" {
				require.NoError(t, os.WriteFile(binary, []byte("#!/bin/bash\necho legacy-help\n"), 0o700)) // #nosec G306 -- scratch stand-in
			}
			out, err := s.invoke("old", "verified", "open", "SHIFT_PLUGIN="+binary)
			require.Error(t, err)
			require.NoFileExists(t, s.path("split"))
			require.Contains(t, out, "install a herdr-agentisan build with boss shift check-model")
		})
	}
}

func TestShiftScriptClosesSplitWithoutTerminal(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	_, err := s.invoke("old", "verified", "open", "SPLIT_WITHOUT_TERMINAL=1")
	require.Error(t, err)
	require.Equal(t, "new\n", s.read("close"))
	require.NoFileExists(t, s.path("current"))
}

func TestShiftScriptRetriesFailedGoCue(t *testing.T) {
	t.Parallel()
	s := setupShiftScript(t)
	require.NoError(t, s.run("old", "verified", "open"))
	s.account()
	_, err := s.invoke("old", "verified", "go", "FAIL_PROMPT=1")
	require.Error(t, err)
	require.FileExists(t, s.path("current/GO"))
	require.NoFileExists(t, s.path("cues"))
	s.write("current/GO", "saved GO\n")
	require.Error(t, s.run("old", "wrong_model", "go"))
	require.NoFileExists(t, s.path("cues"))
	require.NoError(t, s.run("old", "verified", "go"))
	require.Equal(t, "saved GO\n", s.read("current/GO"))
	require.Contains(t, s.read("cues"), "ha-boss-next")
	require.Error(t, s.run("old", "verified", "go"), "a delivered cue cannot be repeated")
	require.Equal(t, 1, strings.Count(s.read("cues"), "takeover"))
}

const (
	shiftCheck      = "boss shift check-model --pane new --terminal split-terminal --name ha-boss-next --model fable --launched-at "
	reopenShiftHint = "abort and open again"
)

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
		"sleep": "#!/bin/bash\nprintf '%s\\n' \"$1\" >> \"$SHIFT_ROOT/sleeps\"\n",
		"date": `#!/bin/bash
if [ "$1" != +%s%3N ]; then exec /bin/date "$@"; fi
if [ -e "$SHIFT_ROOT/calls" ]; then echo 1700000000001; else echo 1700000000000; fi
`,
		"herdr": `#!/bin/bash
case "$1 $2" in
  "agent get") [ ! -e "$SHIFT_ROOT/close" ] || exit 1
               echo '{"result":{"agent":{"pane_id":"old"}}}' ;;
  "pane split") touch "$SHIFT_ROOT/split"
                if [ "${SPLIT_WITHOUT_TERMINAL:-0}" = 1 ]; then
                  echo '{"result":{"pane":{"pane_id":"new"}}}'
                else
                  echo '{"result":{"pane":{"pane_id":"new","terminal_id":"split-terminal"}}}'
                fi ;;
  "agent start") ;;
  "agent prompt") [ "${FAIL_PROMPT:-0}" = 0 ] || exit 9
                  echo "$*" >> "$SHIFT_ROOT/cues" ;;
  "pane close") echo "$3" >> "$SHIFT_ROOT/close" ;;
  "agent rename") echo "$3 $4" >> "$SHIFT_ROOT/rename" ;;
  *) exit 9 ;;
esac
`,
		"herdr-agentisan": `#!/bin/bash
[ "$4" != --help ] || { printf "%s\n" "--pinned-at"; exit 0; }
verdict=$MODEL_VERDICT
pid=100 start=200
if [ "$verdict" = missing_once ]; then
  if [ -e "$SHIFT_ROOT/calls" ]; then verdict=verified; else verdict=missing_model; fi
fi
if [ "$verdict" = slow_refresh ]; then
  probes=0
  if [ -e "$SHIFT_ROOT/calls" ]; then probes=$(wc -l < "$SHIFT_ROOT/calls"); fi
  verdict=report_before_pin
  if (( probes < 16 )); then verdict=claude_unavailable; pid=0 start=0
  elif (( probes == 16 )); then verdict=awaiting_pin
  elif (( probes == 51 )); then verdict=verified
  fi
fi
printf '%s\n' "$*" >> "$SHIFT_ROOT/calls"
[ "$1 $2 $3" = "boss shift check-model" ] || exit 9
if [[ "$verdict" = verified || "$verdict" = wrong_model ]] && [[ " $* " = *" --pid 0 "* ]]; then verdict=awaiting_pin; fi
printf '{"reason":"%s","pid":%s,"start":%s}\n' "$verdict" "$pid" "$start"
[ "$verdict" = verified ] || { echo "probe refused" >&2; exit 1; }
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
	_, err := s.invoke(pane, verdict, verb)
	return err
}

func (s shiftScript) invoke(pane, verdict, verb string, overrides ...string) (string, error) {
	s.t.Helper()
	cmd := exec.CommandContext(s.t.Context(), "bash", s.path("shift.sh"), verb) // #nosec G204 -- scratch script only
	cmd.Env = append([]string{
		"HERDR_SOCKET_PATH=" + s.path("herdr.sock"), "SHIFT_REPO=" + s.dir,
		"PATH=" + s.dir + ":/usr/bin:/bin", "HERDR_PANE_ID=" + pane, "SHIFT_ROOT=" + s.dir,
		"SHIFT_PLUGIN=" + s.path("herdr-agentisan"), "MODEL_VERDICT=" + verdict, "SHIFT_MODEL_SEEN=1",
	}, overrides...)
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

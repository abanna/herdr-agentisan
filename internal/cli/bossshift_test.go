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
		name, model, terminal, reason string
		stamp, launch                 int64
		pin                           uint64
	}{
		{"argv only", "", "term", "missing_model", 0, fresh, 0},
		{"before launch", "fable", "term", "stale_report", fresh - 1, fresh, 0},
		{"before Claude", "fable", "term", "stale_report", started.UnixMilli() - 1, started.UnixMilli() - 2, 0},
		{"expired", "fable", "term", "stale_report", fresh - 181000, fresh - 182000, 0},
		{"future", "fable", "term", "invalid_report_time", fresh + 60000, fresh, 0},
		{"wrong terminal", "fable", "different", "candidate_changed", fresh, fresh, 0},
		{"reused PID", "fable", "term", "candidate_changed", fresh, fresh, proc.Start + 1},
		{"substring", "claude-unfable-5-2", "term", "wrong_model", fresh, fresh, 0},
		{"suffix", "claude-fable-5-2-sonnet", "term", "wrong_model", fresh, fresh, 0},
		{"exact", "fable", "term", "verified", fresh, fresh, proc.Start},
		{"family ID", "claude-fable-5-2", "term", "verified", fresh, fresh, 0},
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
				"boss", "shift", "check-model", "--pane=p", "--terminal=term", "--name=next", "--model=fable",
				fmt.Sprintf("--launched-at=%d", tc.launch),
			}
			if tc.pin != 0 {
				args = append(args, fmt.Sprintf("--pid=%d", os.Getpid()), fmt.Sprintf("--start=%d", tc.pin))
			}
			out, _, err := runOut(t, lookup, args...)
			verdict := oneJSONObject(t, out)
			require.Equal(t, tc.reason, verdict["reason"])
			require.Equal(t, "fable", verdict["requested_model"])
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

func TestShiftWaitIgnoresPartialOrWrongRound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	wait := func(want string, flags ...string) map[string]any {
		t.Helper()
		args := []string{"boss", "shift", "wait", "--record", dir, "--attempt=one", "--timeout=0"}
		out, _, err := runOut(t, func(string) (string, bool) { return "", false }, append(args, flags...)...)
		got := oneJSONObject(t, out)
		require.Equal(t, want, got["status"])
		if want == "timeout" {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		return got
	}
	round := func(attempt string, n int, decision string) string {
		return fmt.Sprintf("attempt: %s\nround: %d\n%s\nMy independent account.\n<!-- end shift round -->\n", attempt, n, decision)
	}
	write("account-1.md.tmp", round("one", 1, ""))
	write("account-1.md", "attempt: one\nround: 1\n\nPartial body")
	wait("timeout")
	write("account-1.md", round("one", 2, ""))
	wait("timeout")
	write("account-1.md", round("old", 1, ""))
	wait("timeout")
	require.NoError(t, os.Rename(filepath.Join(dir, "account-1.md.tmp"), filepath.Join(dir, "account-1.md")))
	require.Equal(t, filepath.Join(dir, "account-1.md"), wait("published")["path"])
	write("reply-1.md", round("one", 1, "decision: revise\n"))
	require.Equal(t, "revise", wait("published", "--reply", "--round=1")["decision"])
	require.Equal(t, float64(2), wait("timeout")["round"])
	wait("timeout", "--ready")
	write("reply-1.md", round("one", 1, "decision: ready\n"))
	wait("published", "--ready")
	write("account-2.md", round("one", 2, ""))
	wait("timeout", "--ready")
	write("reply-2.md", round("one", 2, "decision: ready\n"))
	write("MODEL-BLOCKED", "")
	wait("timeout", "--ready")
	write("ABORTED", "")
	wait("aborted")
}

func TestShiftBriefReturnsPointers(t *testing.T) {
	t.Parallel()
	record := t.TempDir()
	role := filepath.Join(record, "role.md")
	require.NoError(t, os.WriteFile(role, []byte("role"), 0o600))
	out, _, err := runOut(t, func(string) (string, bool) { return "", false }, "boss", "shift", "brief", "--record", record, "--attempt=one", "--role", role)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"record": record, "attempt": "one", "role": role}, oneJSONObject(t, out))
}

// Run only the patched scratch copy of the reviewed script, with stand-ins
// for the plugin and herdr.
func TestShiftScriptCallsVerifierAtOpenAndGo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	patch, err := os.ReadFile(filepath.Join("..", "..", "docs", "boss-shift-v1.patch"))
	require.NoError(t, err)
	preimage, err := os.ReadFile(filepath.Join("testdata", "shift-v0.sh"))
	require.NoError(t, err)
	script := filepath.Join(dir, "shift.sh")
	require.NoError(t, os.WriteFile(script, preimage, 0o600))
	apply := exec.CommandContext(t.Context(), "patch", "--batch", "--fuzz=0", script) // #nosec G204 -- scratch script only
	apply.Stdin = bytes.NewReader(patch)
	out, err := apply.CombinedOutput()
	require.NoError(t, err, "%s", out)

	for name, body := range map[string]string{
		"herdr": `#!/bin/bash
case "$1 $2" in
  "agent get") echo '{"result":{"agent":{"pane_id":"old"}}}' ;;
  "pane split") echo '{"result":{"pane":{"pane_id":"new","terminal_id":"split-terminal"}}}' ;;
  "agent start") ;;
  "agent prompt") echo "$*" >> "$SHIFT_ROOT/cues" ;;
  *) exit 9 ;;
esac
`,
		"herdr-agentisan": `#!/bin/bash
printf '%s\n' "$*" >> "$SHIFT_ROOT/calls"
case "$3" in
  check-model) echo "{\"reason\":\"${MODEL_VERDICT:-verified}\",\"pid\":100,\"start\":200}"
               [ "${MODEL_VERDICT:-verified}" = verified ] ;;
  brief) echo '{"attempt":"dummy","role":"the role","record":"the record"}' ;;
  wait) echo '{"status":"published","round":1,"decision":"ready"}' ;;
  *) exit 9 ;;
esac
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700)) // #nosec G306 -- executable stand-ins in the scratch directory
	}
	runShift := func(verdict, verb string, args ...string) error {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "bash", append([]string{script, verb}, args...)...) // #nosec G204 -- scratch script only
		cmd.Env = []string{
			"PATH=" + dir + ":/usr/bin:/bin", "HERDR_PANE_ID=old", "SHIFT_ROOT=" + dir,
			"SHIFT_PLUGIN=" + filepath.Join(dir, "herdr-agentisan"), "MODEL_VERDICT=" + verdict, "SHIFT_MODEL_SEEN=1",
		}
		out, err := cmd.CombinedOutput()
		t.Logf("%s %s: %s", verb, verdict, out)
		return err
	}
	read := func(name string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(contents)
	}
	require.NoError(t, runShift("verified", "open"))
	prefix := "boss shift check-model --pane new --terminal split-terminal --name ha-boss-next --model fable --launched-at "
	require.Contains(t, read("calls"), prefix)
	require.NoError(t, runShift("verified", "brief"))
	require.Contains(t, read("cues"), "the role")
	require.NoError(t, runShift("verified", "wait", "1"))
	require.Contains(t, read("calls"), "--round 1")
	require.Error(t, runShift("wrong_model", "go"))
	require.NoFileExists(t, filepath.Join(dir, "current", "GO"))
	require.Contains(t, read("calls"), "--pid 100 --start 200")
	require.Equal(t, 2, strings.Count(read("calls"), prefix))
	require.NoError(t, runShift("verified", "go"))
	require.FileExists(t, filepath.Join(dir, "current", "GO"))
	require.Contains(t, read("calls"), "--ready --timeout 0")
	require.NoError(t, os.Remove(filepath.Join(dir, "current")))
	require.Error(t, runShift("wrong_model", "open"))
	require.Contains(t, read("current/state"), "model_pid=100")
}

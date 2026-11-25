package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/bossshift"
	"github.com/abanna/herdr-agentisan/internal/herdr"
)

func newBossCmd() *cobra.Command {
	boss := &cobra.Command{Use: "boss", Short: "Boss shift handoff helpers"}
	shift := &cobra.Command{Use: "shift", Short: "Verify the shift candidate"}
	boss.AddCommand(shift)
	shift.AddCommand(newShiftModelCmd(), newShiftBriefCmd(), newShiftWaitCmd())
	return boss
}

var shiftModelHints = map[string]string{
	"invalid_candidate":     "check the launch time and session id, and give --pid and --start together",
	"candidate_unavailable": "check the herdr connection and confirm the candidate pane is still open",
	"candidate_changed":     "abort and open again with the intended candidate",
	"claude_unavailable":    "wait for Claude to start and run it again, or inspect the pane for an exited session",
	"missing_model":         "the candidate has reported no model: wait for a statusline report, fix its hook, or abort",
	"invalid_report_time":   "check the host clock and wait for a fresh statusline report",
	"stale_report":          "the report is old: wait for a fresh one, fix the statusline hook, or abort",
	"wrong_session":         "the model report is not from this candidate's session: fix its statusline hook (this build's report statusline), or abort",
	"wrong_model":           "select the requested model with /model in the candidate pane",
}

func shiftResult(cmd *cobra.Command, result bossshift.ModelVerdict, err error) error {
	if writeErr := render(cmd.OutOrStdout(), true, result, nil); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return fmt.Errorf("%s: %w. %s", cmd.CommandPath(), err, shiftModelHints[result.Reason])
	}
	return nil
}

func newShiftModelCmd() *cobra.Command {
	var target bossshift.Candidate
	c := &cobra.Command{
		Use:   "check-model",
		Short: "Read the candidate Claude's current model report",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			socket, _ := lookupEnvFrom(ctx)("HERDR_SOCKET_PATH")
			v, err := bossshift.CheckModel(ctx, herdr.Client{SocketPath: socket}, target)
			return shiftResult(cmd, v, err)
		},
	}
	f := c.Flags()
	f.StringVar(&target.Pane, "pane", "", "candidate pane ID")
	f.StringVar(&target.Terminal, "terminal", "", "recorded terminal ID")
	f.StringVar(&target.Name, "name", "", "temporary agent name")
	f.StringVar(&target.Model, "model", "", "requested Claude model")
	f.Int64Var(&target.LaunchedAt, "launched-at", 0, "recorded launch in Unix milliseconds")
	f.Uint32Var(&target.PID, "pid", 0, "recorded Claude PID, with --start; omit both to name the launched Claude")
	f.StringVar(&target.SessionID, "session-id", "", "the --session-id the candidate was launched with")
	f.Uint64Var(&target.Start, "start", 0, "recorded raw process start, with --pid")
	return c
}

// roundResult prints a verb's JSON, whatever its outcome, and fails with its error.
func roundResult(cmd *cobra.Command, result any, err error) error {
	if result != nil {
		if writeErr := render(cmd.OutOrStdout(), true, result, nil); writeErr != nil {
			return writeErr
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
	}
	return nil
}

func newShiftBriefCmd() *cobra.Command {
	var record bossshift.Record
	var role string
	c := &cobra.Command{
		Use:   "brief",
		Short: "Print the candidate's role and attempt pointers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := bossshift.Brief(record, role)
			if err != nil {
				return roundResult(cmd, nil, err)
			}
			return roundResult(cmd, v, nil)
		},
	}
	f := c.Flags()
	f.StringVar(&record.Dir, "record", "", "absolute attempt directory")
	f.StringVar(&record.Attempt, "attempt", "", "attempt ID, the directory's name")
	f.StringVar(&role, "role", "", "absolute path to roles/boss-shift.md")
	return c
}

func newShiftWaitCmd() *cobra.Command {
	var req bossshift.WaitRequest
	c := &cobra.Command{
		Use:   "wait",
		Short: "Wait for a published account or reply in one attempt",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := bossshift.Wait(cmd.Context(), req)
			if v.Status == "" {
				return roundResult(cmd, nil, err)
			}
			return roundResult(cmd, v, err)
		},
	}
	f := c.Flags()
	f.StringVar(&req.Dir, "record", "", "absolute attempt directory")
	f.StringVar(&req.Attempt, "attempt", "", "attempt ID, the directory's name")
	f.IntVar(&req.Round, "round", 0, "round number (default: the account that awaits a reply)")
	f.BoolVar(&req.Reply, "reply", false, "wait for the reply to --round")
	f.DurationVar(&req.Timeout, "timeout", 2*time.Minute, "wait limit (0 probes once, maximum 5m)")
	return c
}

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/bossshift"
	"github.com/abanna/herdr-agentisan/internal/herdr"
)

func newBossCmd() *cobra.Command {
	boss := &cobra.Command{Use: "boss", Short: "Boss shift handoff helpers"}
	shift := &cobra.Command{Use: "shift", Short: "Verify the shift candidate"}
	boss.AddCommand(shift)
	shift.AddCommand(newShiftModelCmd())
	return boss
}

var shiftModelHints = map[string]string{
	"invalid_candidate":     "check the recorded launch and process pin before retrying",
	"candidate_unavailable": "check the herdr connection and confirm the candidate pane is still open",
	"candidate_changed":     "inspect shift status and confirm which candidate owns the recorded pane",
	"claude_unavailable":    "wait for Claude to start or inspect the pane for an exited session",
	"missing_model":         "check the candidate's statusline hook and wait for a report",
	"invalid_report_time":   "check the host clock and wait for a fresh statusline report",
	"stale_report":          "check the candidate's statusline hook and wait for it to refresh",
	"awaiting_pin":          "record the candidate process and wait for its next report",
	"report_before_pin":     "wait for the candidate's next statusline report",
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
	f.Uint32Var(&target.PID, "pid", 0, "recorded Claude PID, with --start")
	f.Int64Var(&target.PinnedAt, "pinned-at", 0, "recorded process pin in Unix milliseconds")
	f.Uint64Var(&target.Start, "start", 0, "recorded raw process start, with --pid")
	return c
}

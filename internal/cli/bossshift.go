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
	shift := &cobra.Command{Use: "shift", Short: "Verify the candidate and read written shift rounds"}
	boss.AddCommand(shift)
	shift.AddCommand(newShiftModelCmd(), newShiftBriefCmd(), newShiftWaitCmd())
	return boss
}

func shiftResult(cmd *cobra.Command, result any, err error) error {
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
	f.Uint64Var(&target.Start, "start", 0, "recorded raw process start, with --pid")
	return c
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
				return shiftResult(cmd, nil, err)
			}
			return shiftResult(cmd, v, nil)
		},
	}
	c.Flags().StringVar(&record.Dir, "record", "", "absolute attempt directory")
	c.Flags().StringVar(&record.Attempt, "attempt", "", "attempt ID")
	c.Flags().StringVar(&role, "role", "", "absolute path to roles/boss-shift.md")
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
				return shiftResult(cmd, nil, err)
			}
			return shiftResult(cmd, v, err)
		},
	}
	f := c.Flags()
	f.StringVar(&req.Dir, "record", "", "absolute attempt directory")
	f.StringVar(&req.Attempt, "attempt", "", "attempt ID")
	f.IntVar(&req.Round, "round", 0, "round number (default: current account)")
	f.BoolVar(&req.Reply, "reply", false, "wait for the reply to --round")
	f.BoolVar(&req.Ready, "ready", false, "wait for the latest account's ready reply")
	f.DurationVar(&req.Timeout, "timeout", 2*time.Minute, "wait limit (0 probes once, maximum 5m)")
	return c
}

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

func shiftResult(cmd *cobra.Command, result bossshift.ModelVerdict, err error) error {
	if writeErr := render(cmd.OutOrStdout(), true, result, nil); writeErr != nil {
		return writeErr
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

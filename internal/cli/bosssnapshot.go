package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// newBossCmd registers the caller-scoped shift snapshot (NERD-5748).
func newBossCmd() *cobra.Command {
	var asJSON bool
	var flags opFlags
	snapshotCmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Show your team's workers and board counts for shift change",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			lookup := lookupEnvFrom(ctx)
			socket, _ := lookup("HERDR_SOCKET_PATH")
			dir, _ := plugin.StateDir(flags.stateDir, lookup)
			paths, err := daemon.PathsFor(dir, socket)
			if err != nil {
				return fmt.Errorf("%w: check herdr and plugin state paths", daemon.ErrUnavailable)
			}
			s, err := daemon.BossSnapshotOf(ctx, paths.Socket)
			if err != nil {
				return fmt.Errorf("boss snapshot: %w", err)
			}
			return render(cmd.OutOrStdout(), asJSON, s, s.WriteText)
		},
	}
	flags.add(snapshotCmd, false)
	addJSONFlag(snapshotCmd, &asJSON)
	c := &cobra.Command{Use: "boss", Short: "Commands for the team's boss"}
	c.AddCommand(snapshotCmd)
	return c
}

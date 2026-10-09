package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

func newDashboardCmd() *cobra.Command {
	var fixture string

	c := &cobra.Command{
		Use:   "dashboard",
		Short: "Show the team dashboard: a card per group; Enter or a click focuses an agent",
		Long: "Draws one card per group with each agent's status, model, ctx, item and\n" +
			"stage, refreshed every second. Arrow keys move, Enter or a left click\n" +
			"focuses and zooms the agent through HERDR_SOCKET_PATH, ? shows help and\n" +
			"q quits.\n\n" +
			"The daemon does not serve snapshots yet, so --fixture is required: a\n" +
			"snapshot JSON file, re-read on every refresh. Try:\n\n" +
			"  herdr-agentisan dashboard --fixture docs/dashboard/sample-snapshot.json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if fixture == "" {
				return fmt.Errorf("%w: the daemon does not serve snapshots yet; run with --fixture FILE", dashboard.ErrNoSource)
			}
			env, err := plugin.EnvFrom(lookupEnvFrom(ctx))
			if err != nil {
				return fmt.Errorf("read plugin environment: %w", err)
			}
			cfg, err := dashboard.FixtureConfig(ctx, fixture, env.SocketPath)
			if err != nil {
				return fmt.Errorf("dashboard: %w", err)
			}
			if err := dashboard.Run(ctx, cfg, cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("dashboard: %w", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&fixture, "fixture", "", "read snapshots from this JSON file instead of the daemon")
	return c
}

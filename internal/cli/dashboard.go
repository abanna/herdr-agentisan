package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

func newDashboardCmd() *cobra.Command {
	var fixture, stateDir, project string

	c := &cobra.Command{
		Use:   "dashboard",
		Short: "Show the team dashboard: a header box, then a box per group; Enter or a click focuses an agent",
		Long: "Draws a header box (the project's PRs, issues and test slots; the boss),\n" +
			"then one box per group, stacked, with one line per agent: status, model,\n" +
			"ctx, item, stage and time in the stage. Refreshed every second; the groups\n" +
			"scroll when they do not fit. Up and down move, Enter or a left click\n" +
			"focuses and zooms the agent through HERDR_SOCKET_PATH, a click on\n" +
			"[ btop ] opens btop in a herdr popup, ? shows help and q quits.\n\n" +
			"Snapshots come from the daemon of the herdr server in HERDR_SOCKET_PATH,\n" +
			"under the plugin state directory (HERDR_PLUGIN_STATE_DIR, or --state-dir),\n" +
			"where `daemon health` looks; herdr's dashboard pane sets both. With no\n" +
			"daemon running the footer says so and the dashboard keeps asking.\n" +
			"The team shown is the one whose workspace the dashboard's own pane is in,\n" +
			"found through herdr by this process's id every 10 seconds; outside every\n" +
			"team the footer says so. --project shows that project's team instead.\n" +
			"--fixture reads a snapshot JSON file instead, again on every refresh:\n\n" +
			"  herdr-agentisan dashboard --fixture docs/dashboard/sample-snapshot.json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			env, err := plugin.EnvFrom(lookupEnvFrom(ctx))
			if err != nil {
				return fmt.Errorf("read plugin environment: %w", err)
			}
			var cfg dashboard.Config
			if fixture != "" {
				cfg, err = dashboard.FixtureConfig(ctx, fixture, env.SocketPath)
			} else {
				dir := stateDir
				if dir == "" {
					dir = env.StateDir
				}
				lookup := lookupEnvFrom(ctx)
				paneID, _ := lookup("HERDR_PANE_ID")
				workspaceID, _ := lookup("HERDR_WORKSPACE_ID")
				cfg, err = dashboard.DaemonConfig(dir, env.SocketPath, dashboard.Where{
					Project: project, PID: os.Getpid(), PaneID: paneID, WorkspaceID: workspaceID,
				})
				if err != nil {
					err = fmt.Errorf("%w (run this from herdr, pass --state-dir, or read a snapshot file with --fixture)", err)
				}
			}
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
	c.Flags().StringVar(&stateDir, "state-dir", "", "plugin state directory the daemon runs under (default: HERDR_PLUGIN_STATE_DIR)")
	c.Flags().StringVar(&project, "project", "", "show this project's team (default: the team of the workspace the dashboard runs in)")
	c.MarkFlagsMutuallyExclusive("fixture", "project")
	return c
}

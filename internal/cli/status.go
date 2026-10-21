package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// errBadBudget is a --timeout that is not a positive duration.
var errBadBudget = errors.New("--timeout must be positive")

func newStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status",
		Short: "One-line team status, for herdr's tab bar",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath())
		},
	}
	c.AddCommand(newStatusLineCmd())
	return c
}

func newStatusLineCmd() *cobra.Command {
	var stateDir, project, workspace string
	var budget time.Duration

	c := &cobra.Command{
		Use:   "line",
		Short: "Print a team's PRs, test slots and open issues on one line, for herdr's tab_bar_right",
		Long: "Prints herdr-status.sh's line for a team, \"PRs #878⏳ #886✓ · slots 2/2 ·\n" +
			"issues 14\", from the daemon's snapshot, never calling gh or herdr (ADR-001\n" +
			"A41). It fits herdr's 80 characters: the PRs give way to a count first.\n" +
			"What the daemon does not report is left out.\n\n" +
			"The team is --project's, or the one --workspace's workspace is in. With\n" +
			"neither, it is the team of HERDR_ACTIVE_WORKSPACE_ID, which herdr sets\n" +
			"for a tab-bar command to the focused workspace, so the line follows\n" +
			"focus; without that, the ◆ spaces' team. Outside every team it prints\n" +
			"an empty line, which hides the entry.\n\n" +
			"The daemon is the one of the herdr server in HERDR_SOCKET_PATH, under the\n" +
			"plugin state directory: herdr does not set HERDR_PLUGIN_STATE_DIR for a\n" +
			"tab-bar command, so pass --state-dir. A daemon that is down, starting or\n" +
			"slower than --timeout is one short line and exit 0, since herdr blanks an\n" +
			"entry that fails. See docs/config/herdr-tab-bar.example.toml.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if budget <= 0 {
				return fmt.Errorf("status line: %w, got %s", errBadBudget, budget)
			}
			ctx := cmd.Context()
			lookup := lookupEnvFrom(ctx)
			env, err := plugin.EnvFrom(lookup)
			if err != nil {
				return fmt.Errorf("read plugin environment: %w", err)
			}
			dir := stateDir
			if dir == "" {
				dir = env.StateDir
			}
			paths, err := daemon.PathsFor(dir, env.SocketPath)
			if err != nil {
				return fmt.Errorf("status line: %w (pass --state-dir; herdr does not set HERDR_PLUGIN_STATE_DIR for a tab-bar command)", noHerdr(err))
			}
			sel := daemon.Selector{Project: project, WorkspaceID: workspace}
			if project == "" && workspace == "" {
				sel.WorkspaceID, _ = lookup("HERDR_ACTIVE_WORKSPACE_ID")
			}
			ctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			return printf(cmd.OutOrStdout(), "%s\n", dashboard.TabBar(ctx, paths.Socket, sel))
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "plugin state directory the daemon runs under (default: HERDR_PLUGIN_STATE_DIR, which herdr does not set for a tab-bar command)")
	c.Flags().StringVar(&project, "project", "", "show this project's team (default: the team of HERDR_ACTIVE_WORKSPACE_ID)")
	c.Flags().StringVar(&workspace, "workspace", "", "show the team of this workspace id (default: HERDR_ACTIVE_WORKSPACE_ID)")
	c.Flags().DurationVar(&budget, "timeout", dashboard.TabBarBudget, "how long to wait for the daemon; keep it under herdr's timeout_seconds")
	c.MarkFlagsMutuallyExclusive("project", "workspace")
	return c
}

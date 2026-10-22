package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

func newBackCmd() *cobra.Command {
	var stateDir string
	var asJSON bool
	c := &cobra.Command{
		Use:   "back",
		Short: "Return to the previous pane, un-zooming the one you leave (the back action)",
		Long: "Asks the daemon to walk back through its focus history; any other focus\n" +
			"change ends the walk. Going nowhere fails, with a toast as the herdr action.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			lookup := lookupEnvFrom(ctx)
			env, err := plugin.EnvFrom(lookup)
			if err != nil {
				return fmt.Errorf("read plugin environment: %w", err)
			}
			dir, _ := plugin.StateDir(stateDir, lookup)
			paths, err := daemon.PathsFor(dir, env.SocketPath)
			if err != nil {
				return fmt.Errorf("back: %w", noHerdr(err))
			}
			goBack := func(ctx context.Context) (daemon.BackResult, error) { return daemon.Back(ctx, paths.Socket) }
			res, err := plugin.Back(ctx, env, goBack, herdr.Client{SocketPath: env.SocketPath})
			if err != nil {
				return err //nolint:wrapcheck // plugin.Back wraps it, sentinel kept
			}
			return render(cmd.OutOrStdout(), asJSON, res, func(w io.Writer) error {
				if res.From == "" {
					return printf(w, "back to %s\n", res.To)
				}
				return printf(w, "back to %s from %s\n", res.To, res.From)
			})
		},
	}
	c.Flags().StringVar(&stateDir, "state-dir", "", "plugin state directory (default: HERDR_PLUGIN_STATE_DIR, else herdr's state directory for this plugin)")
	addJSONFlag(c, &asJSON)
	return c
}

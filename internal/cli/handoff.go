package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// newHandoffCmd is the handoff commands (ADR-001 A33): the daemon writes the
// handoff token a worker ready to be replaced carries. Like the workforce
// commands, stdout is exactly one JSON object on every path, and it exits 1
// when refused.
func newHandoffCmd() *cobra.Command {
	return workforceGroup("handoff", "Mark a worker's handoff (the herdr-workforce skill)", newHandoffReadyCmd())
}

func newHandoffReadyCmd() *cobra.Command {
	var (
		f      opFlags
		unmark bool
	)
	c := &cobra.Command{
		Use:   "ready [<id|pane>]",
		Short: "Mark a worker ready to be replaced, once its handoff is written",
		Long: "Sets the handoff token on the worker's agent pane, which worker retire\n" +
			"--after-handoff waits for: by logical id, tab label or pane id, or, with\n" +
			"none, the pane this command runs in. --clear takes the mark off.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return badRequest(cmd, fmt.Errorf("%s takes at most one logical id or pane id, got %d arguments", cmd.CommandPath(), len(args)))
			}
			if len(args) == 1 && args[0] == "" {
				return badRequest(cmd, errors.New("an empty target names nothing; leave it out to mark this pane"))
			}
			return f.runOp(cmd, nil, daemon.OpHandoffReady, func() (any, error) {
				if len(args) == 1 {
					return workforce.HandoffReadyRequest{Target: args[0], Clear: unmark}, nil
				}
				ctx := cmd.Context()
				lookup := lookupEnvFrom(ctx)
				pane := report.PaneFrom(lookup)
				pane.Lineage = lineageFrom(ctx)()
				caller, err := workforce.FindCaller(ctx, herdr.Client{SocketPath: pane.SocketPath}, pane, lookup)
				return workforce.HandoffReadyRequest{Clear: unmark, CallerPane: caller.Pane, Worker: caller.Worker}, err //nolint:wrapcheck // workforce wraps it
			})
		},
	}
	f.add(c, false)
	c.Flags().BoolVar(&unmark, "clear", false, "take the mark off instead")
	return c
}

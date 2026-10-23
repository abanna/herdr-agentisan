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
// handoff token a worker ready to be replaced carries, and completes a
// rotation its replacement acknowledges. Like the workforce commands, stdout
// is exactly one JSON object on every path, and it exits 1 when refused.
func newHandoffCmd() *cobra.Command {
	return workforceGroup("handoff", "Mark and acknowledge a worker's handoff (the herdr-workforce skill)", newHandoffReadyCmd(), newHandoffAckCmd())
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
				caller, err := callerPane(cmd)
				worker, _ := lookupEnvFrom(cmd.Context())(workforce.WorkerEnv)
				return workforce.HandoffReadyRequest{Clear: unmark, CallerPane: caller, Worker: worker}, err
			})
		},
	}
	f.add(c, false)
	c.Flags().BoolVar(&unmark, "clear", false, "take the mark off instead")
	return c
}

func newHandoffAckCmd() *cobra.Command {
	var f opFlags
	c := &cobra.Command{
		Use:   "ack [<pane>]",
		Short: "Acknowledge a worker's handoff from its replacement, completing the rotation",
		Long: "Completes the pending handoff whose replacement is the pane: by pane id,\n" +
			"or, with none, the pane this command runs in. The replacement must first\n" +
			"report the old pane's item and stage; the worker's herdr name and its\n" +
			"inbox then move to it. Never HERDR_AGENTISAN_WORKER: the old pane has it too.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return badRequest(cmd, fmt.Errorf("%s takes at most one pane id, got %d arguments", cmd.CommandPath(), len(args)))
			}
			if len(args) == 1 && args[0] == "" {
				return badRequest(cmd, errors.New("an empty pane names nothing; leave it out to acknowledge from this pane"))
			}
			return f.runOp(cmd, nil, daemon.OpHandoffAck, func() (any, error) {
				if len(args) == 1 {
					return workforce.HandoffAckRequest{Target: args[0]}, nil
				}
				caller, err := callerPane(cmd)
				return workforce.HandoffAckRequest{CallerPane: caller}, err
			})
		},
	}
	f.add(c, false)
	return c
}

// callerPane is the current id of the pane cmd runs in, as
// workforce.FindCallerPane confirms it, or "".
func callerPane(cmd *cobra.Command) (string, error) {
	ctx := cmd.Context()
	pane := report.PaneFrom(lookupEnvFrom(ctx))
	pane.Lineage = lineageFrom(ctx)()
	return workforce.FindCallerPane(ctx, herdr.Client{SocketPath: pane.SocketPath}, pane) //nolint:wrapcheck // workforce wraps it
}

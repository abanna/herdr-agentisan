package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// newSendCmd is send (ADR-001 D8, A30): it asks the daemon to queue a
// message, which the daemon types into the recipient's agent exactly once.
// Like the workforce commands, stdout is exactly one JSON object on every
// path, and it exits 1 when refused.
func newSendCmd() *cobra.Command {
	var f opFlags
	c := &cobra.Command{
		Use:   "send <to> <body|->",
		Short: "Queue a message for a worker or a boss (boss: your own team's)",
		Long: "Queues the body for the recipient, which the daemon types into its agent\n" +
			"once: a worker by logical id, a team's boss by name (<short>-boss), or boss\n" +
			"for your own team's. A body of - is read from stdin. See docs/messaging.md.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return badRequest(cmd, fmt.Errorf("%s takes a recipient and a body, got %d arguments: quote the body, or pass - and write it to stdin", cmd.CommandPath(), len(args)))
			}
			body := args[1]
			if body == "-" {
				read, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), store.MaxBody+1))
				if err != nil {
					return badRequest(cmd, fmt.Errorf("read the body from stdin: %w", err))
				}
				if len(read) > store.MaxBody {
					return refuseWith(cmd, "too_large", fmt.Sprintf("the body on stdin is over %d bytes", store.MaxBody))
				}
				body = string(read)
			}
			// Before the request's encoding would replace each byte that is
			// not UTF-8, and grow the body by two bytes for it.
			if err := store.CheckBody(body); err != nil {
				if errors.Is(err, store.ErrTooLarge) {
					return refuseWith(cmd, "too_large", err.Error())
				}
				return badRequest(cmd, err)
			}
			return f.runOp(cmd, nil, daemon.OpSend, func() (any, error) {
				ctx := cmd.Context()
				lookup := lookupEnvFrom(ctx)
				pane := report.PaneFrom(lookup)
				pane.Lineage = lineageFrom(ctx)()
				caller, err := workforce.FindCallerPane(ctx, herdr.Client{SocketPath: pane.SocketPath}, pane)
				worker, _ := lookup(workforce.WorkerEnv)
				return workforce.SendRequest{To: args[0], Body: body, CallerPane: caller, Worker: worker}, err //nolint:wrapcheck // workforce wraps it
			})
		},
	}
	f.add(c, false)
	c.SetFlagErrorFunc(badRequest)
	return c
}

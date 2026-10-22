package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/daemon"
)

// RenderStart is renderStart, for the outcomes a test of the commands could
// reach only by waiting out daemon.DefaultStartWait.
var RenderStart = renderStart

// DaemonOptions are the options `daemon run` builds from ctx's environment.
func DaemonOptions(ctx context.Context) (daemon.Options, error) {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	return daemonOptions(cmd, "", false)
}

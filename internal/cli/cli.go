// Package cli builds the product CLI: the `herdr-agentisan` command tree.
//
// Commands are thin. They parse flags, call a domain package, and render — no
// business logic lives here, so every caller of the domain sees the same rules.
package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/config"
)

// Root builds the `herdr-agentisan` command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "herdr-agentisan",
		Short: "Agentisan plugin for herdr",
		Long: "herdr-agentisan is the herdr plugin binary. herdr invokes its\n" +
			"subcommands from the plugin manifest; they can also be run by hand.",
		SilenceUsage: true,
		// Cobra prints the error itself; returning it as well double-prints.
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (%s)", config.Version, config.Commit),
	}
	root.AddCommand(newVersionCmd())
	return root
}

// addJSONFlag is shared by every subcommand so `--json` behaves identically.
func addJSONFlag(c *cobra.Command, target *bool) {
	c.Flags().BoolVar(target, "json", false, "emit JSON instead of text")
}

func render(w io.Writer, asJSON bool, v any, text func(io.Writer) error) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
		return nil
	}
	return text(w)
}

// buildInfo is the provenance `version` reports.
type buildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func newVersionCmd() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "version",
		Short: "Print the build version and commit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildInfo{Version: config.Version, Commit: config.Commit}
			return render(cmd.OutOrStdout(), asJSON, info, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "herdr-agentisan %s (%s)\n", info.Version, info.Commit)
				return err //nolint:wrapcheck // trivial write, message adds nothing
			})
		},
	}
	addJSONFlag(c, &asJSON)
	return c
}

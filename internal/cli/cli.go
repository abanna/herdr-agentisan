// Package cli builds the product CLI: the `herdr-agentisan` command tree.
//
// Commands are thin. They parse flags, call a domain package, and render — no
// business logic lives here, so every caller of the domain sees the same rules.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// lookupEnvKey addresses the environment lookup stashed in the command
// context. Tests inject one so a command never reads the developer's real
// HERDR_* variables (a shell inside herdr has them set).
type lookupEnvKey struct{}

// WithLookupEnv returns a context whose commands read the environment
// through lookup instead of os.LookupEnv.
func WithLookupEnv(ctx context.Context, lookup func(string) (string, bool)) context.Context {
	return context.WithValue(ctx, lookupEnvKey{}, lookup)
}

func lookupEnvFrom(ctx context.Context) func(string) (string, bool) {
	if f, ok := ctx.Value(lookupEnvKey{}).(func(string) (string, bool)); ok && f != nil {
		return f
	}
	return os.LookupEnv
}

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
	root.AddCommand(newVersionCmd(), newActionCmd())
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

func newActionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "action",
		Short: "Commands herdr runs for the [[actions]] in herdr-plugin.toml",
		// Without Args and RunE, cobra answers an unknown verb under a
		// non-root group by printing help and exiting 0 — so a typo'd
		// manifest action would report success to herdr. NoArgs turns the
		// unknown verb into an error; RunE makes a bare `action` fail too.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath())
		},
	}
	c.AddCommand(newActionPingCmd())
	return c
}

func newActionPingCmd() *cobra.Command {
	var asJSON bool

	c := &cobra.Command{
		Use:   "ping",
		Short: "Show a toast in herdr to prove the plugin is wired up",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			env, err := plugin.EnvFrom(lookupEnvFrom(ctx))
			if err != nil {
				return fmt.Errorf("read plugin environment: %w", err)
			}
			res, err := plugin.Ping(ctx, herdr.Client{SocketPath: env.SocketPath}, config.Version)
			if errors.Is(err, herdr.ErrNoSocket) {
				return fmt.Errorf("HERDR_SOCKET_PATH is not set; run this from inside herdr: %w", err)
			}
			if err != nil {
				return fmt.Errorf("action ping: %w", err)
			}
			logger := logging.From(ctx)
			logger.Debug().Bool("shown", res.Shown).Str("reason", res.Reason).Msg("ping")
			return render(cmd.OutOrStdout(), asJSON, res, func(w io.Writer) error {
				if res.Shown {
					_, err := fmt.Fprintln(w, "notification shown")
					return err //nolint:wrapcheck // trivial write
				}
				_, err := fmt.Fprintf(w, "notification not shown: %s\n", res.Reason)
				return err //nolint:wrapcheck // trivial write
			})
		},
	}
	addJSONFlag(c, &asJSON)
	return c
}

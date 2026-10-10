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
	"github.com/abanna/herdr-agentisan/internal/report"
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

// lineageKey addresses the lineage reader stashed in the command context.
// Tests inject one so a report never matches panes against the real process
// table.
type lineageKey struct{}

// WithLineage returns a context whose commands read the reporting process's
// lineage through read instead of report.SelfLineage.
func WithLineage(ctx context.Context, read func() *report.Lineage) context.Context {
	return context.WithValue(ctx, lineageKey{}, read)
}

// lineageFrom returns the injected lineage reader, or one that reads the real
// process table anchored on the command's environment.
func lineageFrom(ctx context.Context) func() *report.Lineage {
	if f, ok := ctx.Value(lineageKey{}).(func() *report.Lineage); ok && f != nil {
		return f
	}
	lookup := lookupEnvFrom(ctx)
	return func() *report.Lineage { return report.SelfLineage(lookup) }
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
	root.AddCommand(newVersionCmd(), newActionCmd(), newReportCmd(), newDaemonCmd(), newConfigCmd(), newDashboardCmd(), newBackCmd())
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

func newReportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "report",
		Short: "Push the calling agent's own state to its herdr pane",
		// Same guard as `action`: an unknown verb must fail, not print help
		// and exit 0.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath())
		},
	}
	c.AddCommand(newReportStatuslineCmd(), newReportStageCmd(), newReportCodexCmd())
	return c
}

func newReportStatuslineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "statusline",
		Short: "Push ctx from Claude's statusline JSON on stdin",
		Long: "Reads Claude Code's statusline JSON on stdin and sets this pane's ctx\n" +
			"token. Add one line to the statusline script:\n\n" +
			"  herdr-agentisan report statusline <<<\"$input\" >/dev/null 2>&1 &\n\n" +
			"It prints nothing and always exits 0; why it reported nothing goes to\n" +
			"the debug log (HERDR_AGENTISAN_LOG_LEVEL=debug).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Read the lineage first: the statusline backgrounds this process
			// and may exit, and once it has, only CLAUDE_PID still reaches the
			// pane.
			lineage := lineageFrom(ctx)()
			logger := logging.From(ctx)
			pane := report.PaneFrom(lookupEnvFrom(ctx))
			pane.Lineage = lineage
			res, err := report.Statusline(ctx, herdr.Client{SocketPath: pane.SocketPath}, pane, cmd.InOrStdin())
			// The statusline runs this in the background on every refresh, so
			// a failure is never the user's to see: it would either vanish
			// into the redirect or, without one, garble the statusline.
			if err != nil {
				logger.Debug().Err(err).Str("pane", pane.PaneID).Ints("lineage", lineage.PIDs()).Msg("report statusline: nothing reported")
				return nil
			}
			logger.Debug().Str("pane", pane.PaneID).Str("reported_to", res.PaneID).Int(report.CtxKey, res.Ctx).Msg("report statusline")
			return nil
		},
	}
}

func newReportCodexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "codex",
		Short: "Push ctx from a Codex hook's payload on stdin",
		Long: "Reads the JSON a Codex hook passes on stdin and sets this pane's ctx\n" +
			"token to what Codex shows as \"Context N% used\", from the latest token\n" +
			"count in the session's transcript. Run it as an async PostToolUse and\n" +
			"Stop hook (docs/config/codex-hooks.example.toml), and start Codex with\n" +
			"--no-daemon: a hook run by the shared app-server daemon cannot tell which\n" +
			"pane it belongs to, and reports nothing.\n\n" +
			"It prints nothing and always exits 0; why it reported nothing goes to\n" +
			"the debug log (HERDR_AGENTISAN_LOG_LEVEL=debug).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Read the lineage first, as statusline does: the pane is found
			// through the processes running when the hook fired.
			lineage := lineageFrom(ctx)()
			logger := logging.From(ctx)
			pane := report.PaneFrom(lookupEnvFrom(ctx))
			pane.Lineage = lineage
			res, err := report.Codex(ctx, herdr.Client{SocketPath: pane.SocketPath}, pane, cmd.InOrStdin())
			// A hook's output is Codex's to read: anything on stdout could
			// fail a synchronous hook, so a failure goes to the debug log.
			if err != nil {
				logger.Debug().Err(err).Str("pane", pane.PaneID).Ints("lineage", lineage.PIDs()).Msg("report codex: nothing reported")
				return nil
			}
			logger.Debug().Str("pane", pane.PaneID).Str("reported_to", res.PaneID).Int(report.CtxKey, res.Ctx).Msg("report codex")
			return nil
		},
	}
}

func newReportStageCmd() *cobra.Command {
	var item, stage string
	c := &cobra.Command{
		Use:   "stage --item <id> --stage <step>",
		Short: "Push item and stage from an agentisan step transition",
		Long: "Sets this pane's item and stage tokens: the ticket the agent works on\n" +
			"and the agentisan pipeline step it is in. Agentisan runs it on every\n" +
			"step transition, passing each value as --flag=value so that one\n" +
			"starting with a dash stays a value.\n\n" +
			"A value is at most 80 printable characters; anything else is refused,\n" +
			"never cut short. The tokens expire 24 h after the last report.\n\n" +
			"It prints nothing and exits 0 whether or not it reported; only a\n" +
			"malformed command line (an unknown flag, an extra argument) fails. Why\n" +
			"nothing was reported goes to the debug log (HERDR_AGENTISAN_LOG_LEVEL=debug).",
		Example: "herdr-agentisan report stage --item=NERD-5253 --stage=build_test",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Read the lineage first, as statusline does: the pane is found
			// through the processes running when the report was made.
			lineage := lineageFrom(ctx)()
			logger := logging.From(ctx)
			pane := report.PaneFrom(lookupEnvFrom(ctx))
			pane.Lineage = lineage
			res, err := report.Stage(ctx, herdr.Client{SocketPath: pane.SocketPath}, pane, item, stage)
			// A step transition runs this, and a sidebar token never fails
			// a step: why nothing was reported is for the debug log only.
			if err != nil {
				logger.Debug().Err(err).Str("pane", pane.PaneID).Ints("lineage", lineage.PIDs()).Msg("report stage: nothing reported")
				return nil
			}
			logger.Debug().Str("pane", pane.PaneID).Str("reported_to", res.PaneID).
				Str(report.ItemKey, res.Item).Str(report.StageKey, res.Stage).Msg("report stage")
			return nil
		},
	}
	c.Flags().StringVar(&item, "item", "", "ticket key, such as NERD-5253")
	c.Flags().StringVar(&stage, "stage", "", "agentisan pipeline step, such as build_test")
	return c
}

package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/codexhooks"
)

func newCodexCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "codex",
		Short: "Set Codex up to report to herdr",
		// Same guard as `action`: an unknown verb must fail, not print help
		// and exit 0.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath())
		},
	}
	c.AddCommand(newCodexHooksCmd())
	return c
}

// trustSteps is what is left to the user after an install (ADR-001 A43).
const trustSteps = "Next, once: start `codex --no-daemon` in a herdr pane and choose\n" +
	"\"Trust all and continue\", or trust the hooks in /hooks. Codex asks again\n" +
	"whenever a hook changes. Codex also trusts a folder by its exact path:\n" +
	"trust each repo a Codex worker runs in when Codex first asks.\n" +
	"Start Codex with --no-daemon, `codex resume` and `codex fork` included:\n" +
	"under the shared background server the hooks report nothing.\n"

func newCodexHooksCmd() *cobra.Command {
	var check, turn, asJSON bool

	c := &cobra.Command{
		Use:   "hooks",
		Short: "Install the Codex hooks that report each Codex agent's ctx",
		Long: "Adds the PostToolUse, Stop and PostCompact hooks that run `herdr-agentisan\n" +
			"report codex` to Codex's hooks file, $CODEX_HOME/hooks.json (default\n" +
			"~/.codex/hooks.json), after any hooks already there, and writes the file\n" +
			"only when that changes it. It never edits config.toml and never trusts a\n" +
			"hook: Codex runs a hook only once you trust it, at startup or in /hooks.\n" +
			"It refuses, changing nothing, when config.toml already runs `report codex`.\n\n" +
			"--turn also adds the Stop and UserPromptSubmit hooks that run\n" +
			"`herdr-agentisan report turn`, which report each turn's item and stage,\n" +
			"each in a group of its own after any hooks already there; it refuses,\n" +
			"changing nothing, when config.toml already runs either report.\n\n" +
			"--check only reports where each hook is, and exits 1 unless all three are\n" +
			"installed as this command writes them, or with --turn all five.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := codexhooks.Home(lookupEnvFrom(cmd.Context()))
			if err != nil {
				return fmt.Errorf("codex hooks: %w", err)
			}
			run, again := codexhooks.Install, "herdr-agentisan codex hooks"
			switch {
			case check && turn:
				run = codexhooks.CheckWithTurn
			case check:
				run = codexhooks.Check
			case turn:
				run = codexhooks.InstallWithTurn
			}
			if turn {
				again += " --turn"
			}
			res, err := run(home)
			if err != nil {
				return fmt.Errorf("codex hooks: %w", err)
			}
			if err := render(cmd.OutOrStdout(), asJSON, res, func(w io.Writer) error {
				return writeCodexHooks(w, res, check, turn)
			}); err != nil {
				return err
			}
			if check && (!res.Installed() || turn && !res.TurnInstalled()) {
				return fmt.Errorf("codex hooks: %w; run %s", codexhooks.ErrNotInstalled, again)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only report whether the hooks are installed, and exit 1 unless they are")
	c.Flags().BoolVar(&turn, "turn", false, "also the two hooks that report each turn's item and stage (report turn)")
	addJSONFlag(c, &asJSON)
	return c
}

func writeCodexHooks(w io.Writer, res codexhooks.Result, check, turn bool) error {
	var b []byte
	if !check {
		done := "unchanged"
		if res.Changed {
			done = "written"
		}
		b = fmt.Appendf(b, "%s: %s\n", res.Path, done)
	}
	for _, h := range res.Hooks {
		name := fmt.Sprintf("%-12s", h.Event)
		if turn {
			// Stop runs both reports, so each line names its own.
			name = fmt.Sprintf("%-16s %-5s", h.Event, h.Report)
		}
		line := fmt.Sprintf("  %s %s", name, h.State)
		if check && h.Source != "" {
			line = fmt.Sprintf("  %s %-8s %s", name, h.State, h.Source)
		}
		b = fmt.Appendf(b, "%s\n", line)
	}
	if res.HooksOff {
		b = fmt.Append(b, "warning: config.toml sets features.hooks = false, so Codex runs no hook until it is true\n")
	}
	if !check {
		b = fmt.Append(b, "\n", trustSteps)
	}
	_, err := w.Write(b)
	return err //nolint:wrapcheck // trivial write, message adds nothing
}

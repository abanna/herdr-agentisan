package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/debugger"
	"github.com/abanna/herdr-agentisan/internal/trail"
)

func newDebuggerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "debugger",
		Short: "The debugger agent that ships with the plugin (ADR-001 A32)",
	}
	c.AddCommand(newDebuggerArgsCmd())
	return c
}

// launchLine is `debugger args`' JSON: the environment to set and the argv
// to run, claude first.
type launchLine struct {
	Env  map[string]string `json:"env"`
	Argv []string          `json:"argv"`
}

func newDebuggerArgsCmd() *cobra.Command {
	var root, repo, serverDir, configDir string
	var asJSON bool
	c := &cobra.Command{
		Use:   "args",
		Short: "Print the command that starts the debugger, for a launch by hand",
		Long: "args prints the claude command line, with the environment it needs, that starts\n" +
			"the debugger read-only in --repo. `worker spawn --group debugger` builds the same.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lookup := lookupEnvFrom(cmd.Context())
			bin, err := os.Executable()
			if err != nil {
				return fmt.Errorf("debugger args: the plugin binary: %w", err)
			}
			if root == "" {
				root = pluginRoot(lookup, bin)
			}
			if _, err := os.Stat(filepath.Join(root, debugger.RoleFile)); err != nil {
				return fmt.Errorf("debugger args: no %s under %q; pass --root, the plugin's checkout: %w", debugger.RoleFile, root, err)
			}
			if configDir == "" {
				configDir, _ = lookup(trail.ConfigDirEnv)
			}
			if configDir == "" {
				return errors.New("debugger args: pass --config-dir, the plugin's config dir, or set " + trail.ConfigDirEnv)
			}
			if repo, err = filepath.Abs(repo); err != nil {
				return fmt.Errorf("debugger args: the repo: %w", err)
			}
			launch, err := debugger.Launch{Root: root, Bin: bin, Repo: repo, ServerDir: serverDir, ConfigDir: configDir}.Command()
			if err != nil {
				return fmt.Errorf("debugger args: %w", err)
			}
			line := launchLine{Env: launch.Env, Argv: append([]string{"claude"}, launch.Args...)}
			return render(cmd.OutOrStdout(), asJSON, line, func(w io.Writer) error {
				words := make([]string, 0, len(line.Env)+len(line.Argv))
				for _, k := range slices.Sorted(maps.Keys(line.Env)) {
					words = append(words, k+"="+shellQuote(line.Env[k]))
				}
				for _, a := range line.Argv {
					words = append(words, shellQuote(a))
				}
				_, err := fmt.Fprintln(w, strings.Join(words, " "))
				return err //nolint:wrapcheck // the command's own output stream
			})
		},
	}
	c.Flags().StringVar(&root, "root", "", "the plugin's checkout (default: $HERDR_PLUGIN_ROOT, else the binary's)")
	c.Flags().StringVar(&repo, "repo", ".", "the project repo the debugger runs in")
	c.Flags().StringVar(&serverDir, "server-dir", "", "the daemon's directory for this herdr server, holding daemon.log")
	c.Flags().StringVar(&configDir, "config-dir", "", "the plugin's config dir (default: $"+trail.ConfigDirEnv+")")
	addJSONFlag(c, &asJSON)
	return c
}

// pluginRoot is HERDR_PLUGIN_ROOT when herdr set it, else the checkout the
// binary was built into (<root>/bin/herdr-agentisan).
func pluginRoot(lookup func(string) (string, bool), bin string) string {
	if r, _ := lookup("HERDR_PLUGIN_ROOT"); r != "" {
		return r
	}
	return filepath.Dir(filepath.Dir(bin))
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+-]+$`)

// shellQuote quotes s for a POSIX shell, leaving plain words bare.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

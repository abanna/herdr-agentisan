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

// launchLine is `debugger args`' JSON: the directory to start in, the
// environment to set and the argv to run, claude first.
type launchLine struct {
	Dir  string            `json:"dir"`
	Env  map[string]string `json:"env"`
	Argv []string          `json:"argv"`
}

func newDebuggerArgsCmd() *cobra.Command {
	var root, repo, serverDir, configDir string
	var asJSON bool
	c := &cobra.Command{
		Use:   "args",
		Short: "Print the command that starts the debugger, for a launch by hand",
		Long: "args prints the shell line that starts the debugger in --repo: a cd into the repo,\n" +
			"then claude with the environment it needs, its reads fenced to the repo and --server-dir.\n" +
			"`worker spawn --group debugger` builds the same.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lookup := lookupEnvFrom(cmd.Context())
			bin, err := os.Executable()
			if err != nil {
				return fmt.Errorf("debugger args: the plugin binary: %w", err)
			}
			if root == "" {
				root = debugger.Root(lookup, bin)
			}
			if err := debugger.CheckRoot(root); err != nil {
				return fmt.Errorf("debugger args: %w; pass --root", err)
			}
			// claude's --add-dir refuses a server dir that is not a
			// directory; say so before printing a launch that cannot start.
			if serverDir != "" {
				if fi, err := os.Stat(serverDir); err != nil || !fi.IsDir() {
					return fmt.Errorf("debugger args: --server-dir %q is not a directory; pass the daemon's srv-… directory", serverDir)
				}
			}
			if configDir == "" {
				configDir, _ = lookup(trail.ConfigDirEnv)
			}
			if configDir == "" {
				return errors.New("debugger args: pass --config-dir, the plugin's config dir, or set " + trail.ConfigDirEnv)
			}
			keyFiles, err := debugger.KeyFiles(configDir, lookup)
			if err != nil {
				return fmt.Errorf("debugger args: %w", err)
			}
			if repo, err = filepath.Abs(repo); err != nil {
				return fmt.Errorf("debugger args: the repo: %w", err)
			}
			launch, err := debugger.Launch{Root: root, Bin: bin, Repo: repo, ServerDir: serverDir, ConfigDir: configDir, KeyFiles: keyFiles}.Command()
			if err != nil {
				return fmt.Errorf("debugger args: %w", err)
			}
			line := launchLine{Dir: launch.Dir, Env: launch.Env, Argv: append([]string{"claude"}, launch.Args...)}
			return render(cmd.OutOrStdout(), asJSON, line, func(w io.Writer) error {
				// The line starts in the repo: the directory claude starts in
				// is one its read fence admits.
				words := make([]string, 0, 3+len(line.Env)+len(line.Argv))
				words = append(words, "cd", shellQuote(line.Dir), "&&")
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

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+-]+$`)

// shellQuote quotes s for a POSIX shell, leaving plain words bare.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

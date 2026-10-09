package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/plugin"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// opReadKey addresses the 1Password reader stashed in the command context.
// Tests inject one so --check-secrets never runs the real op CLI.
type opReadKey struct{}

// WithOpRead returns a context whose commands read op:// references through
// read instead of settings.OpRead.
func WithOpRead(ctx context.Context, read func(context.Context, string) (string, error)) context.Context {
	return context.WithValue(ctx, opReadKey{}, read)
}

func opReadFrom(ctx context.Context) func(context.Context, string) (string, error) {
	if f, ok := ctx.Value(opReadKey{}).(func(context.Context, string) (string, error)); ok && f != nil {
		return f
	}
	return settings.OpRead
}

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Inspect the herdr-agentisan.toml configuration",
		// Same guard as `action`: an unknown verb must fail, not print help
		// and exit 0.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath())
		},
	}
	c.AddCommand(newConfigResolveCmd())
	return c
}

// resolveOutput is the --json shape of `config resolve`.
type resolveOutput struct {
	// Project is null when no project was selected.
	Project  *string           `json:"project"`
	Values   map[string]any    `json:"values"`
	Sources  map[string]string `json:"sources"`
	Projects []string          `json:"projects"`
	Secrets  []secretOutput    `json:"secrets,omitempty"`
}

type secretOutput struct {
	Path   string `json:"path"`
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

func newConfigResolveCmd() *cobra.Command {
	var (
		project, configDir   string
		sets                 []string
		asJSON, checkSecrets bool
	)

	c := &cobra.Command{
		Use:   "resolve",
		Short: "Print the resolved configuration and the layer each value came from",
		Long: "Resolves herdr-agentisan.toml in the plugin config directory, the\n" +
			"project's .herdr-agentisan.toml and any --set overrides, and prints one\n" +
			"line per value with the layer it came from: default, shared, project,\n" +
			"repo or flag. Secrets print as their reference, never their value.\n\n" +
			"With no --project it resolves the shared values and lists the projects.\n" +
			"--check-secrets also resolves each secret and fails if any is missing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			lookup := lookupEnvFrom(ctx)
			dir := configDir
			if dir == "" {
				env, err := plugin.EnvFrom(lookup)
				if err != nil {
					return fmt.Errorf("read plugin environment: %w", err)
				}
				dir = env.ConfigDir
			}
			res, err := settings.Load(settings.LoadOptions{
				ConfigDir: dir, Project: project, Set: sets, LookupEnv: lookup,
			})
			if errors.Is(err, settings.ErrNoConfigDir) {
				return fmt.Errorf("pass --config-dir or set HERDR_PLUGIN_CONFIG_DIR: %w", err)
			}
			if err != nil {
				return fmt.Errorf("config resolve: %w", err)
			}
			var statuses []settings.SecretStatus
			var secretsErr error
			if checkSecrets {
				statuses, secretsErr = settings.CheckSecrets(ctx, res, lookup, opReadFrom(ctx))
			}
			if err := render(cmd.OutOrStdout(), asJSON, resolveJSON(res, statuses, checkSecrets), func(w io.Writer) error {
				return writeResolved(w, res, statuses)
			}); err != nil {
				return err
			}
			if secretsErr != nil {
				return fmt.Errorf("config resolve: %w", secretsErr)
			}
			return nil
		},
	}
	c.Flags().StringVar(&project, "project", "", "resolve this project's profile")
	c.Flags().StringVar(&configDir, "config-dir", "", "directory holding herdr-agentisan.toml (default $HERDR_PLUGIN_CONFIG_DIR)")
	// StringArray, not StringSlice: a slice splits on commas and would cut
	// --set rotation.thresholds=[50,60,70] into three.
	c.Flags().StringArrayVar(&sets, "set", nil, "override a value as key.path=<toml value>; repeatable")
	c.Flags().BoolVar(&checkSecrets, "check-secrets", false, "resolve every secret and fail if any is missing")
	addJSONFlag(c, &asJSON)
	return c
}

func resolveJSON(res settings.Resolved, statuses []settings.SecretStatus, checked bool) resolveOutput {
	out := resolveOutput{Values: res.Values, Sources: res.Sources, Projects: res.Projects}
	if res.Project != "" {
		out.Project = &res.Project
	}
	if checked {
		out.Secrets = make([]secretOutput, 0, len(statuses))
	}
	for _, s := range statuses {
		so := secretOutput{Path: s.Path, Ref: s.Ref.String(), Status: "set"}
		if s.Err != nil {
			so.Status, so.Reason = "missing", s.Err.Error()
		}
		out.Secrets = append(out.Secrets, so)
	}
	return out
}

func writeResolved(w io.Writer, res settings.Resolved, statuses []settings.SecretStatus) error {
	var b strings.Builder
	for _, l := range res.Leaves() {
		fmt.Fprintf(&b, "%s = %s  # %s\n", l.Path, settings.FormatValue(l.Value), l.Layer)
	}
	if res.Project == "" {
		names := "(none)"
		if len(res.Projects) > 0 {
			names = strings.Join(res.Projects, ", ")
		}
		fmt.Fprintf(&b, "projects: %s\n", names)
	}
	for _, s := range statuses {
		if s.Err != nil {
			fmt.Fprintf(&b, "%s: missing (%v)\n", s.Path, s.Err)
			continue
		}
		fmt.Fprintf(&b, "%s: set\n", s.Path)
	}
	_, err := io.WriteString(w, b.String())
	return err //nolint:wrapcheck // trivial write, message adds nothing
}

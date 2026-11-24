package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/bundle"
	"github.com/abanna/herdr-agentisan/internal/config"
)

// ErrNeedsFlags is stage read's refusal when --repo or --work-dir is missing.
var ErrNeedsFlags = errors.New("--repo and --work-dir are required")

type bundleLocatorKey struct{}

// WithBundleLocator returns a context whose commands find the bundle's
// server through locate instead of bundle.Locate, so a test never starts
// uv or the real bundle.
func WithBundleLocator(ctx context.Context, locate func(repo, clientVersion string) (bundle.Launch, error)) context.Context {
	return context.WithValue(ctx, bundleLocatorKey{}, locate)
}

func bundleLocatorFrom(ctx context.Context) func(repo, clientVersion string) (bundle.Launch, error) {
	if f, ok := ctx.Value(bundleLocatorKey{}).(func(string, string) (bundle.Launch, error)); ok && f != nil {
		return f
	}
	return bundle.Locate
}

func newStageCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stage",
		Short: "A worktree's workflow stage, read from the Agentisan bundle",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s needs a subcommand; see --help", cmd.CommandPath())
		},
	}
	c.AddCommand(newStageReadCmd())
	return c
}

func newStageReadCmd() *cobra.Command {
	var repo, workDir string
	var budget time.Duration

	c := &cobra.Command{
		Use:   "read",
		Short: "Print a worktree's workflow stage, ticket and update time, read through the bundle's MCP server",
		Long: "Starts the Agentisan bundle installed in --repo (the project's main checkout)\n" +
			"as an MCP server over stdio, asks it for the workflow state of --work-dir\n" +
			"(convergence.read_state) and stops it (NERD-5790). One --timeout bounds the\n" +
			"start-up, the handshake and the read; when it ends the server's process group\n" +
			"is killed. Any failure of the bundle is an error and exit 1.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if repo == "" || workDir == "" {
				return fmt.Errorf("stage read: %w", ErrNeedsFlags)
			}
			if budget <= 0 {
				return fmt.Errorf("stage read: %w, got %s", errBadBudget, budget)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), budget)
			defer cancel()
			launch, err := bundleLocatorFrom(ctx)(repo, config.Version)
			if err != nil {
				return fmt.Errorf("stage read: %w", err)
			}
			s, err := bundle.Open(ctx, launch)
			if err != nil {
				return fmt.Errorf("stage read: %w", err)
			}
			defer func() {
				if cerr := s.Close(); cerr != nil && err == nil {
					err = fmt.Errorf("stage read: %w", cerr)
				}
			}()
			st, err := bundle.ReadState(s, workDir)
			if err != nil {
				return fmt.Errorf("stage read: %w", err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "stage %s · %s · updated %s\n", orNone(st.Stage, "(none yet)"), orNone(st.Ticket, "(no ticket)"), st.UpdatedAt.UTC().Format(time.RFC3339))
			return err //nolint:wrapcheck // trivial write
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "the project's main checkout, where the Agentisan bundle is installed (absolute)")
	c.Flags().StringVar(&workDir, "work-dir", "", "the worktree to read, at or under --repo")
	c.Flags().DurationVar(&budget, "timeout", time.Minute, "bound on starting the bundle, the handshake and the read")
	return c
}

func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

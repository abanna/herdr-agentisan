// Package devcli builds the `devctl` command tree: repository operations and
// helpers that keep the gates honest.
//
// These are development-time verbs, deliberately separate from the product
// CLI in internal/cli. Nothing here is part of the shipped service.
package devcli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nerds-run/go-agents/internal/api"
	"github.com/nerds-run/go-agents/internal/config"
)

// DefaultCoverageFloor is the repository's minimum statement coverage.
// AGENTS.md documents it; changing one without the other is drift.
const DefaultCoverageFloor = 75.0

// Root builds the `devctl` command tree.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "devctl",
		Short: "Development operations and helpers for this repository",
		Long: "devctl holds the repository's own tooling: the gates that have no\n" +
			"Go builtin (coverage floor, docs parity) and the generators whose\n" +
			"output is committed and drift-checked.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (%s)", config.Version, config.Commit),
	}
	root.AddCommand(
		newParityCmd(), newCoverageCmd(), newOpenAPICmd(),
		newRoutesCmd(), newLargeFilesCmd(), newManifestsCmd(),
	)
	return root
}

// repoRoot walks up from the working directory to the directory holding go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod found in any parent directory")
		}
		dir = parent
	}
}

func newParityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "docs-parity",
		Short: "Fail if AGENTS.md, Taskfile.yml and ci.yml describe different gates",
		Long: "Checks every direction: a gate CI runs must be documented and\n" +
			"wrapped in a task, and a documented command must be one CI runs\n" +
			"unless it is declared local-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			rep, err := CheckParity(root)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), rep); err != nil {
				return fmt.Errorf("write report: %w", err)
			}
			if !rep.OK() {
				return errors.New("docs parity check failed")
			}
			return nil
		},
	}
}

func newCoverageCmd() *cobra.Command {
	var (
		profile string
		minPct  float64
	)
	c := &cobra.Command{
		Use:   "coverage",
		Short: "Enforce the statement-coverage floor go test cannot enforce itself",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			path := profile
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("coverage profile %s not found: run `task test:unit` first: %w", profile, err)
			}

			// #nosec G204 -- path is derived from a repo-relative flag, not user input at runtime.
			out, err := exec.CommandContext(cmd.Context(), "go", "tool", "cover", "-func", path).Output()
			if err != nil {
				return fmt.Errorf("run go tool cover: %w", err)
			}
			pct, err := ParseCoverageTotal(strings.NewReader(string(out)))
			if err != nil {
				return err
			}

			res := CoverageResult{Percent: pct, Threshold: minPct}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), res); err != nil {
				return fmt.Errorf("write result: %w", err)
			}
			if !res.Meets() {
				return fmt.Errorf("coverage %.1f%% is below the %.1f%% floor", res.Percent, res.Threshold)
			}
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "coverage.out", "coverage profile written by `go test -coverprofile`")
	c.Flags().Float64Var(&minPct, "min", DefaultCoverageFloor, "minimum total statement coverage")
	return c
}

func newOpenAPICmd() *cobra.Command {
	var write bool
	c := &cobra.Command{
		Use:   "openapi",
		Short: "Generate the OpenAPI document from the bound routes",
		Long: "Writes docs/openapi.json with --write, otherwise prints it. CI\n" +
			"regenerates and diffs, so a route added without regenerating fails.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A route the server binds but does not document would ship an
			// incomplete spec that still passes the diff check.
			if missingDocs, missingRoutes := api.SpecDrift(); len(missingDocs)+len(missingRoutes) > 0 {
				var b strings.Builder
				b.WriteString("route table and OpenAPI operations disagree")
				if len(missingDocs) > 0 {
					fmt.Fprintf(&b, "\n  bound but undocumented: %s", strings.Join(missingDocs, ", "))
				}
				if len(missingRoutes) > 0 {
					fmt.Fprintf(&b, "\n  documented but not bound: %s", strings.Join(missingRoutes, ", "))
				}
				return errors.New(b.String())
			}

			doc, err := api.Spec()
			if err != nil {
				return fmt.Errorf("render openapi document: %w", err)
			}
			if !write {
				_, err := cmd.OutOrStdout().Write(doc)
				return err //nolint:wrapcheck // trivial write
			}
			root, err := repoRoot()
			if err != nil {
				return err
			}
			dst := filepath.Join(root, api.SpecPath())
			if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
				return fmt.Errorf("create docs directory: %w", err)
			}
			if err := os.WriteFile(dst, doc, 0o600); err != nil {
				return fmt.Errorf("write %s: %w", api.SpecPath(), err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", api.SpecPath())
			return err //nolint:wrapcheck // trivial write
		},
	}
	c.Flags().BoolVar(&write, "write", false, "write docs/openapi.json instead of printing")
	return c
}

func newLargeFilesCmd() *cobra.Command {
	var maxKB int64
	c := &cobra.Command{
		Use:   "large-files",
		Short: "Fail if any tracked file exceeds the size ceiling git does not enforce",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			found, err := FindLargeFiles(cmd.Context(), root, maxKB)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(found) == 0 {
				_, err := fmt.Fprintf(w, "no tracked file exceeds %dKB\n", maxKB)
				return err //nolint:wrapcheck // trivial write
			}
			for _, f := range found {
				if _, err := fmt.Fprintf(w, "%s is %d bytes (%dKB, ceiling %dKB)\n", f.Path, f.Bytes, f.KB(), maxKB); err != nil {
					return err //nolint:wrapcheck // trivial write
				}
			}
			return fmt.Errorf("%d tracked file(s) exceed the %dKB ceiling", len(found), maxKB)
		},
	}
	c.Flags().Int64Var(&maxKB, "max-kb", DefaultMaxFileKB, "maximum size of a tracked file, in KB")
	return c
}

func newManifestsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "manifests",
		Short: "Fail if infrastructure/local/k8s contradicts the code it claims to deploy",
		Long: "Checks the invariants YAML alone cannot: probe paths and ports\n" +
			"against internal/config, the pod uid against the Dockerfile, the\n" +
			"rolling-update strategy, and that no Service publishes the admin\n" +
			"listener carrying /metrics and /debug/pprof.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			findings, err := CheckManifests(root)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(findings) == 0 {
				_, err := fmt.Fprintf(w, "%s agrees with the code it deploys\n", ManifestDir())
				return err //nolint:wrapcheck // trivial write
			}
			for _, f := range findings {
				if _, err := fmt.Fprintln(w, f); err != nil {
					return err //nolint:wrapcheck // trivial write
				}
			}
			return fmt.Errorf("%d manifest invariant(s) broken", len(findings))
		},
	}
}

func newRoutesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "routes",
		Short: "List every route the API binds, and flag undocumented ones",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			missingDocs, missingRoutes := api.SpecDrift()
			w := cmd.OutOrStdout()
			if len(missingDocs) == 0 && len(missingRoutes) == 0 {
				_, err := fmt.Fprintln(w, "every bound route is documented")
				return err //nolint:wrapcheck // trivial write
			}
			for _, r := range missingDocs {
				if _, err := fmt.Fprintf(w, "bound but undocumented: %s\n", r); err != nil {
					return err //nolint:wrapcheck // trivial write
				}
			}
			for _, r := range missingRoutes {
				if _, err := fmt.Fprintf(w, "documented but not bound: %s\n", r); err != nil {
					return err //nolint:wrapcheck // trivial write
				}
			}
			return errors.New("route documentation is out of date")
		},
	}
}

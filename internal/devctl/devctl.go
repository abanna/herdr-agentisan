// Package devctl builds the `devctl` command tree: repository operations and
// helpers that keep the gates honest.
//
// These are development-time verbs, deliberately separate from the product
// CLI in internal/cli. Nothing here is part of the shipped service.
package devctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abanna/herdr-agentisan/internal/config"
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
			"Go builtin (coverage floor, docs parity, large files), and CI's test\n" +
			"jobs' packages and coverage merge.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (%s)", config.Version, config.Commit),
	}
	root.AddCommand(newParityCmd(), newCoverageCmd(), newLargeFilesCmd(),
		newTestPackagesCmd(), newCoverageMergeCmd())
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
			stale, err := StaleExemptions(root)
			if err != nil {
				return err
			}
			if len(stale) > 0 {
				return fmt.Errorf("docs parity: stale escape-hatch entries in internal/devctl/parity.go:\n  - %s",
					strings.Join(stale, "\n  - "))
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

func newTestPackagesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test-packages <shard>",
		Short: "Print the packages one of CI's test jobs tests: " + strings.Join(Shards(), ", "),
		// Every argument is a shard name, an option included, so every
		// refusal prints FailedShardPath.
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			pkgs, err := testPackages(cmd.Context(), args)
			if err != nil {
				if _, werr := fmt.Fprintln(cmd.OutOrStdout(), FailedShardPath); werr != nil {
					return errors.Join(err, fmt.Errorf("write %s: %w", FailedShardPath, werr))
				}
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), strings.Join(pkgs, " ")); err != nil {
				return fmt.Errorf("write packages: %w", err)
			}
			return nil
		},
	}
}

// goListBudget bounds test-packages' go list runs, so a hung one cannot hold a CI job.
const goListBudget = 2 * time.Minute

// testPackages is the one shard args names, read against this module.
func testPackages(ctx context.Context, args []string) ([]string, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("want one test shard (%s), got %d arguments", strings.Join(Shards(), ", "), len(args))
	}
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, goListBudget)
	defer cancel()
	module, all, err := ModulePackages(ctx, root)
	if err != nil {
		return nil, err
	}
	return ShardPackages(module, all, args[0])
}

func newCoverageMergeCmd() *cobra.Command {
	var dir, out string
	c := &cobra.Command{
		Use:   "coverage-merge",
		Short: "Merge every CI test job's coverage-<shard>.out into one profile for the floor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := repoRoot()
			if err != nil {
				return err
			}
			inputs, err := ShardProfiles(underRoot(root, dir))
			if err != nil {
				return err
			}
			rep, err := MergeCoverageProfiles(underRoot(root, out), inputs)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), rep); err != nil {
				return fmt.Errorf("write report: %w", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "directory holding each test job's coverage-<shard>.out")
	c.Flags().StringVar(&out, "out", "coverage.out", "merged profile to write, for `devctl coverage`")
	return c
}

// underRoot resolves a relative flag path against the repository root.
func underRoot(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

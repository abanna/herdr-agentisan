package devctl

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CI and Task run the unit suite in five shards (NERD-5676): one for each
// package below, whose tests take longest, and RestShard for every other
// package, each job's packages from `devctl test-packages <shard>`, so a
// package added later needs no edit.

// heavyShards each test one package, ./internal/<name>, and nothing below it.
var heavyShards = []string{"dashboard", "store", "daemon", "workforce"}

// RestShard is the job that tests every package no other shard names.
const RestShard = "rest"

// FailedShardPath is what a refused `test-packages` prints: its caller is to
// read it through $( ), which drops the exit code, and go test fails on it.
const FailedShardPath = "./devctl-test-packages-failed"

// Shards lists every test shard, in CI's job order.
func Shards() []string { return append(slices.Clone(heavyShards), RestShard) }

// ShardPackages picks shard's packages out of all, the import paths `go list
// ./...` printed for module. It refuses an unknown shard, a heavy package all
// lacks, whichever shard is asked for, and a shard left with no package.
func ShardPackages(module string, all []string, shard string) ([]string, error) {
	heavy := make(map[string]bool, len(heavyShards))
	for _, name := range heavyShards {
		path := module + "/internal/" + name
		if !slices.Contains(all, path) {
			return nil, fmt.Errorf("test shard %s names %s, which is not a package of this module", name, path)
		}
		heavy[path] = true
	}

	var pkgs []string
	switch {
	case shard == RestShard:
		for _, p := range all {
			if !heavy[p] {
				pkgs = append(pkgs, p)
			}
		}
	case slices.Contains(heavyShards, shard):
		pkgs = []string{module + "/internal/" + shard}
	default:
		return nil, fmt.Errorf("unknown test shard %q: want one of %s", shard, strings.Join(Shards(), ", "))
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("test shard %s has no packages", shard)
	}
	return pkgs, nil
}

// ModulePackages returns the module path and packages selected in the
// requested race configuration.
func ModulePackages(ctx context.Context, root string, race bool) (string, []string, error) {
	module, err := goList(ctx, root, "-m")
	if err != nil {
		return "", nil, err
	}
	modules := strings.Fields(string(module))
	if len(modules) != 1 {
		return "", nil, fmt.Errorf("go list -m printed %d modules, want 1", len(modules))
	}
	pkgs, err := goListPackages(ctx, root, race, "./...")
	if err != nil {
		return "", nil, err
	}
	return modules[0], strings.Fields(string(pkgs)), nil
}

// goListPackages uses the same race setting for shard and coverage discovery.
func goListPackages(ctx context.Context, root string, race bool, args ...string) ([]byte, error) {
	return goList(ctx, root, append([]string{"-race=" + strconv.FormatBool(race)}, args...)...)
}

// goListWaitDelay bounds the wait for go list's output after ctx kills it,
// which a child holding the pipe would otherwise stretch.
const goListWaitDelay = time.Second

// goListBudget bounds package discovery so a hung go list cannot hold a CI job.
const goListBudget = 2 * time.Minute

// goList runs `go list <args>` in root and returns its raw output. Its error
// carries what go list printed on stderr.
func goList(ctx context.Context, root string, args ...string) ([]byte, error) {
	// #nosec G204 -- callers supply fixed go list arguments, never CLI operands.
	cmd := exec.CommandContext(ctx, "go", append([]string{"list"}, args...)...)
	cmd.Dir = root
	cmd.WaitDelay = goListWaitDelay
	out, err := cmd.Output()
	if err != nil {
		arg := strings.Join(args, " ")
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("go list %s: %w: %s", arg, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("go list %s: %w", arg, err)
	}
	return out, nil
}

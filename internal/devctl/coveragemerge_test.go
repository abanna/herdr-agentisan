package devctl_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

func TestMergedCoverageRetainsZeroBlocksForSubprocessOnlyPackage(t *testing.T) {
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	t.Setenv("CGO_ENABLED", "1")
	dir := t.TempDir()
	t.Chdir(dir)
	for file, source := range map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"cmd/helper/main.go": `package main
import ("example.com/m/internal/subprocess"; "example.com/m/internal/runner_test")
func main() { println(subprocess.Value()+shadow.Value()) }
`,
		"internal/subprocess/value.go":   "package subprocess\nfunc Value() int { return 40 }\n",
		"internal/runner_test/shadow.go": "package shadow\nfunc Value() int { return 2 }\n",
		"internal/driver/driver.go": `package driver
func One() int { return 1 }
func Two() int { return 2 }
func Three() int { return 3 }
func Four() int { return 4 }
`,
		"internal/driver/driver_test.go": `package driver
import ("os/exec"; "strings"; "testing")
func TestRun(t *testing.T) {
	if One()+Two()+Three()+Four() != 10 { t.Fatal("wrong sum") }
	out, err := exec.CommandContext(t.Context(), "go", "run", "../../cmd/helper").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "42" { t.Fatalf("helper: %q, %v", out, err) }
}
`,
		"internal/linked/value.go":  "package linked\nfunc Value() int { return 1 }\n",
		"internal/runner/runner.go": "package runner\nfunc Value() int { return 2 }\n",
		"internal/runner/runner_test.go": `package runner_test
import ("testing"; "example.com/m/internal/linked"; "example.com/m/internal/runner")
func TestRun(t *testing.T) {
	if runner.Value()+linked.Value() != 3 { t.Fatal("wrong sum") }
}
`,
	} {
		path := filepath.Join(dir, filepath.FromSlash(file))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	}

	goCmd := func(t *testing.T, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = dir
		cmd.WaitDelay = time.Second
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "go %s\n%s", strings.Join(args, " "), out)
	}
	testFlags := []string{"test", "-race", "-p=1", "-covermode=atomic", "-coverpkg=./internal/..."}
	goCmd(t, append(testFlags, "-coverprofile=single.out", "./...")...)
	for _, shard := range []string{"first", "second"} {
		require.NoError(t, os.Mkdir(shard, 0o700))
	}
	goCmd(t, append(testFlags, "-cover", "./cmd/helper", "./internal/driver", "./internal/subprocess", "./internal/runner_test",
		"-args", "-test.gocoverdir="+filepath.Join(dir, "first"))...)
	goCmd(t, append(testFlags, "-cover", "./internal/runner", "./internal/linked",
		"-args", "-test.gocoverdir="+filepath.Join(dir, "second"))...)

	unlinked := []string{"example.com/m/internal/subprocess/value.go:", "example.com/m/internal/runner_test/shadow.go:"}
	const linked = "example.com/m/internal/linked"
	single, err := os.ReadFile("single.out")
	require.NoError(t, err)
	for _, pkg := range unlinked {
		blocks := coverageBlocksFor(string(single), pkg)
		require.NotEmpty(t, blocks, "Go's single-job profile includes %s", pkg)
		for _, block := range blocks {
			require.True(t, strings.HasSuffix(block, " 0"), "subprocess block must have a zero count: %s", block)
		}
	}

	singleResult, err := runDev(t, "coverage", "--profile", "single.out", "--min", "75")
	require.NoError(t, err)
	require.Contains(t, singleResult, "coverage 75.0% (floor 75.0%) ok")

	t.Run("complete shards", func(t *testing.T) {
		goCmd(t, "tool", "covdata", "textfmt", "-i=first,second", "-o", "merged.out")
		before, err := os.ReadFile("merged.out")
		require.NoError(t, err)
		for _, pkg := range unlinked {
			require.NotContains(t, string(before), pkg, "no test binary linked %s", pkg)
		}
		require.Contains(t, string(before), linked+"/value.go:", "an external test imported this package")

		out, err := runDev(t, "coverage", "--profile", "merged.out", "--min", "75", "--complete-unlinked")
		require.NoError(t, err, out)
		assert.Equal(t, singleResult, out)
		after, err := os.ReadFile("merged.out")
		require.NoError(t, err)
		for _, pkg := range unlinked {
			assert.Equal(t, coverageBlocksFor(string(single), pkg), coverageBlocksFor(string(after), pkg))
		}
		_, err = runDev(t, "coverage", "--profile", "merged.out", "--min", "76", "--complete-unlinked")
		assert.ErrorContains(t, err, "coverage 75.0% is below the 76.0% floor")
	})

	t.Run("missing test shard", func(t *testing.T) {
		goCmd(t, "tool", "covdata", "textfmt", "-i=first", "-o", "missing.out")
		before, err := os.ReadFile("missing.out")
		require.NoError(t, err)
		_, err = runDev(t, "coverage", "--profile", "missing.out", "--min", "0", "--complete-unlinked")
		require.ErrorIs(t, err, devctl.ErrMissingCoveragePackages)
		assert.ErrorContains(t, err, linked)
		assert.ErrorContains(t, err, "example.com/m/internal/runner")
		assert.NotContains(t, err.Error(), "example.com/m/internal/runner_test")
		assert.NotContains(t, err.Error(), "example.com/m/internal/subprocess")
		after, err := os.ReadFile("missing.out")
		require.NoError(t, err)
		assert.Equal(t, before, after, "a genuinely missing test contribution must leave the profile untouched")
	})
}

func coverageBlocksFor(profile, prefix string) []string {
	var blocks []string
	for line := range strings.SplitSeq(profile, "\n") {
		if strings.HasPrefix(line, prefix) {
			blocks = append(blocks, line)
		}
	}
	slices.Sort(blocks)
	return slices.Compact(blocks)
}

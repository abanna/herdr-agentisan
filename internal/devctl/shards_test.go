package devctl_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

// module is a made-up module whose packages the split is checked against.
const module = "example.com/m"

// modulePackages is a module with the four heavy packages, a subpackage of
// one of them and two others, in `go list` order.
var modulePackages = []string{
	module + "/cmd/tool",
	module + "/internal/attention",
	module + "/internal/daemon",
	module + "/internal/dashboard",
	module + "/internal/na\u00efve",
	module + "/internal/Store",
	module + "/internal/store",
	module + "/internal/store/sub",
	module + "/internal/workforce",
}

func TestShardPackagesSplitsTheModule(t *testing.T) {
	t.Parallel()

	tests := map[string][]string{
		"dashboard": {module + "/internal/dashboard"},
		"store":     {module + "/internal/store"},
		"daemon":    {module + "/internal/daemon"},
		"workforce": {module + "/internal/workforce"},
		// Every other package, a heavy package's subpackage and its case
		// variant included, as go list printed them: a package added later
		// is tested with no list to edit.
		"rest": {
			module + "/cmd/tool", module + "/internal/attention", module + "/internal/na\u00efve",
			module + "/internal/Store", module + "/internal/store/sub",
		},
	}
	for shard, want := range tests {
		t.Run(shard, func(t *testing.T) {
			t.Parallel()

			got, err := devctl.ShardPackages(module, modulePackages, shard)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestShardPackagesRefusesWhatWouldLeaveAPackageUntested(t *testing.T) {
	t.Parallel()

	withoutDaemon := slices.DeleteFunc(slices.Clone(modulePackages), func(p string) bool {
		return p == module+"/internal/daemon"
	})
	onlyHeavy := []string{
		module + "/internal/daemon", module + "/internal/dashboard",
		module + "/internal/store", module + "/internal/workforce",
	}
	tests := map[string]struct {
		all   []string
		shard string
		want  string
	}{
		"an unknown shard":                      {modulePackages, "dashbaord", "unknown test shard"},
		"an empty shard name":                   {modulePackages, "", "unknown test shard"},
		"a case variant":                        {modulePackages, "Store", "unknown test shard"},
		"a non-ASCII look-alike":                {modulePackages, "st\u00f6re", "unknown test shard"},
		"a shard with spaces around it":         {modulePackages, " store ", "unknown test shard"},
		"a shard with a line break":             {modulePackages, "store\n", "unknown test shard"},
		"a shard with a CR":                     {modulePackages, "store\r", "unknown test shard"},
		"a byte not UTF-8":                      {modulePackages, "store\xff", "unknown test shard"},
		"a cut multibyte sequence":              {modulePackages, "stor\xc3", "unknown test shard"},
		"a surrogate's bytes":                   {modulePackages, "store\xed\xa0\x80", "unknown test shard"},
		"a byte-order mark":                     {modulePackages, "\ufeffstore", "unknown test shard"},
		"a NUL":                                 {modulePackages, "store\x00", "unknown test shard"},
		"mixed encodings":                       {modulePackages, "st\u00f6re\xff", "unknown test shard"},
		"a heavy package gone, asked for it":    {withoutDaemon, "daemon", "internal/daemon"},
		"a heavy package gone, asked for rest":  {withoutDaemon, "rest", "internal/daemon"},
		"a heavy package gone, asked for other": {withoutDaemon, "store", "internal/daemon"},
		"nothing left for rest":                 {onlyHeavy, "rest", "no packages"},
		"no packages at all":                    {nil, "rest", "internal/"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := devctl.ShardPackages(module, tc.all, tc.shard)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
			assert.Empty(t, got)
		})
	}
}

// Compare the command's shards with Go's package list for each test mode.
func TestShardsTestEveryPackageOfThisModuleOnce(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"dashboard", "store", "daemon", "workforce", "rest"}, devctl.Shards(), "CI's five test jobs")
	for _, mode := range []struct {
		goFlag   string
		devFlags []string
	}{
		{goFlag: "-race=true"},
		{goFlag: "-race=false", devFlags: []string{"--race=false"}},
	} {
		t.Run(mode.goFlag, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "go", "list", mode.goFlag, "./...")
			cmd.Dir = "../.."
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			all := strings.Fields(string(out))
			require.Contains(t, all, "github.com/abanna/herdr-agentisan/internal/devctl")
			assert.ElementsMatch(t, all, packagesFromAllShards(t, mode.devFlags...))
		})
	}
}

func TestTestPackagesCommandPrintsAShardsPackages(t *testing.T) {
	out, stderr, err := runDevWithSeparateStreams(t, "test-packages", "store")
	require.NoError(t, err)
	assert.Equal(t, "github.com/abanna/herdr-agentisan/internal/store\n", out)
	assert.Empty(t, stderr)

	out, stderr, err = runDevWithSeparateStreams(t, "test-packages", "rest")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	rest := strings.Fields(out)
	assert.Contains(t, rest, "github.com/abanna/herdr-agentisan/internal/devctl")
	assert.NotContains(t, rest, "github.com/abanna/herdr-agentisan/internal/store")
}

func TestShardsIncludeRaceOnlyTestPackageOnlyWithRace(t *testing.T) {
	pkg := writeTaggedShardFixture(t, "race")
	t.Run("race", func(t *testing.T) {
		assert.Equal(t, 1, shardPackageCount(t, pkg))
	})
	t.Run("non-race", func(t *testing.T) {
		assert.Zero(t, shardPackageCount(t, pkg, "--race=false"))
	})
}

func TestShardsIncludeNonRaceOnlyTestPackageOnlyWithoutRace(t *testing.T) {
	pkg := writeTaggedShardFixture(t, "!race")
	t.Run("race", func(t *testing.T) {
		assert.Zero(t, shardPackageCount(t, pkg))
	})
	t.Run("non-race", func(t *testing.T) {
		assert.Equal(t, 1, shardPackageCount(t, pkg, "--race=false"))
	})
}

func writeTaggedShardFixture(t *testing.T, tag string) string {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("go.mod", []byte("module "+module+"\n\ngo 1.26\n"), 0o600))
	for _, name := range devctl.Shards() {
		dir := filepath.Join("internal", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg.go"), []byte("package "+name+"\n"), 0o600))
	}
	const dir = "internal/conditional"
	require.NoError(t, os.MkdirAll(dir, 0o755))
	src := "//go:build " + tag + "\n\npackage conditional\nimport \"testing\"\nfunc TestConditional(t *testing.T) {}\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "conditional_test.go"), []byte(src), 0o600))
	return module + "/" + dir
}

func packagesFromAllShards(t *testing.T, flags ...string) []string {
	t.Helper()
	var packages []string
	for _, shard := range devctl.Shards() {
		args := append([]string{"test-packages", shard}, flags...)
		out, stderr, err := runDevWithSeparateStreams(t, args...)
		require.NoError(t, err, shard)
		require.Empty(t, stderr)
		got := strings.Fields(out)
		require.NotEmpty(t, got, shard)
		packages = append(packages, got...)
	}
	return packages
}

func shardPackageCount(t *testing.T, want string, flags ...string) int {
	t.Helper()
	count := 0
	for _, pkg := range packagesFromAllShards(t, flags...) {
		if pkg == want {
			count++
		}
	}
	return count
}

// TestTestPackagesCommandFailsSoGoTestFails: a test job is to read the
// command through $( ), which drops its exit code, so a refusal also prints a
// path no package has, and go test fails on it whatever the module root holds.
func TestTestPackagesCommandFailsSoGoTestFails(t *testing.T) {
	for name, args := range map[string][]string{
		"an unknown shard":      {"test-packages", "nope"},
		"no shard":              {"test-packages"},
		"two shards":            {"test-packages", "store", "daemon"},
		"an option":             {"test-packages", "--nope"},
		"invalid race argument": {"test-packages", "rest", "--race=sometimes"},
		"race flag, no shard":   {"test-packages", "--race=false"},
		"help":                  {"test-packages", "--help"},
		"help and a shard":      {"test-packages", "rest", "-h"},
	} {
		t.Run(name, func(t *testing.T) {
			out, stderr, err := runDevWithSeparateStreams(t, args...)
			require.Error(t, err)
			assert.Equal(t, devctl.FailedShardPath+"\n", out)
			assert.Empty(t, stderr, "the root returns the diagnostic to main")
		})
	}
	assert.True(t, strings.HasPrefix(devctl.FailedShardPath, "./"), "a relative path go test reads as a directory")
}

// TestModulePackagesRefusesWhatGoListCannotList: go list's own failure, with
// its words, and a workspace that would make the module ambiguous.
func TestModulePackagesRefusesWhatGoListCannotList(t *testing.T) {
	t.Run("a directory in no module", func(t *testing.T) {
		t.Setenv("GOWORK", "off")
		_, _, err := devctl.ModulePackages(t.Context(), t.TempDir(), false)
		require.Error(t, err)
		// Outside a module go list -m answers command-line-arguments, and
		// go list ./... then fails.
		assert.ErrorContains(t, err, "go list -race=false ./...")
		assert.ErrorContains(t, err, "does not contain main module", "go list's own stderr is in the error")
	})
	t.Run("a root that does not exist", func(t *testing.T) {
		_, _, err := devctl.ModulePackages(t.Context(), filepath.Join(t.TempDir(), "gone"), false)
		require.Error(t, err)
	})
	t.Run("go is not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, _, err := devctl.ModulePackages(t.Context(), "../..", false)
		require.Error(t, err)
		assert.ErrorContains(t, err, "go list -m")
	})
	t.Run("a workspace of two modules", func(t *testing.T) {
		dir := t.TempDir()
		for _, m := range []string{"a", "b"} {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, m), 0o755))
			mod := fmt.Sprintf("module example.com/%s\n\ngo 1.26\n", m)
			require.NoError(t, os.WriteFile(filepath.Join(dir, m, "go.mod"), []byte(mod), 0o600))
		}
		work := filepath.Join(dir, "go.work")
		require.NoError(t, os.WriteFile(work, []byte("go 1.26\n\nuse (\n\t./a\n\t./b\n)\n"), 0o600))
		t.Setenv("GOWORK", work)
		_, _, err := devctl.ModulePackages(t.Context(), filepath.Join(dir, "a"), false)
		require.Error(t, err)
		assert.ErrorContains(t, err, "2 modules, want 1")
	})
}

package devctl_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

func TestCoverageAllowsUninstrumentedBlankFunctions(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	for name, source := range map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.26\n",
		"internal/blank/blank.go": `package blank
const A = 1
func _() {
	var assertion [1]struct{}
	_ = assertion[A-1]
	_ = func() int { return A }
}
`,
		"internal/live/live.go": "package live\nvar F = func() int { return 1 }\n",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	}

	const profile = "coverage.out"
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-p=1", "-coverprofile="+profile, "-covermode=atomic", "-coverpkg=./internal/...", "./...")
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	data, err := os.ReadFile(filepath.Join(dir, profile))
	require.NoError(t, err)
	require.Contains(t, string(data), "example.com/m/internal/live/live.go:")
	require.NotContains(t, string(data), "example.com/m/internal/blank/")

	pkgs, err := devctl.FloorPackages(t.Context(), dir)
	require.NoError(t, err)
	missing, err := devctl.MissingPackages(strings.NewReader(string(data)), pkgs)
	require.NoError(t, err)
	assert.Empty(t, missing, "Go emitted blocks for every package with instrumentable statements")
}

func TestFloorPackagesReadsBeyondTheFirstSourceFile(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "internal", "multi")
	require.NoError(t, os.MkdirAll(pkgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "a_types.go"), []byte("package multi\ntype T int\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(pkgDir, "b_code.go"), []byte("package multi\nfunc F() T { return 1 }\n"), 0o600))

	pkgs, err := devctl.FloorPackages(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	assert.True(t, pkgs[0].HasStatements, "the second file contains statements")
}

func TestFloorPackagesPropagatesGoListFailure(t *testing.T) {
	t.Setenv("GOWORK", "off")
	_, err := devctl.FloorPackages(t.Context(), t.TempDir())
	require.Error(t, err)
	assert.ErrorContains(t, err, "go list -json=ImportPath,Dir,GoFiles,CgoFiles ./internal/...")
}

func TestMissingPackagesRequiresModeHeader(t *testing.T) {
	t.Parallel()
	_, err := devctl.MissingPackages(strings.NewReader("example.com/m/internal/a/x.go:3.14,5.2 1 0\n"), nil)
	require.ErrorContains(t, err, "coverage profile has no mode header")
}

// TestCoverageRefusesAProfileMissingAPackage: the floor counts each package
// it is given that holds a statement, so one with no block in the profile is
// named, in the order given; a package with no statement needs none, and a
// block counts only for the package of its own file. On a module, the
// packages are go list's, each told by its own files whether it holds a
// statement.
func TestCoverageRefusesAProfileMissingAPackage(t *testing.T) {
	const m = "example.com/m/internal"
	pkgs := []devctl.Package{
		{ImportPath: m + "/a", HasStatements: true},
		{ImportPath: m + "/b", HasStatements: true},
		{ImportPath: m + "/types"},
		{ImportPath: m + "/store", HasStatements: true},
	}
	block := func(pkg string) string { return pkg + "/x.go:3.14,5.2 1 0\n" }
	const mode = "mode: atomic\n"
	complete := mode + block(m+"/a") + block(m+"/b") + block(m+"/store")
	for name, tc := range map[string]struct {
		profile string
		want    []string
	}{
		"every package with statements has a block": {complete, nil},
		"CRLF profile": {strings.ReplaceAll(complete, "\n", "\r\n"), nil},
		"one has none": {mode + block(m+"/a") + block(m+"/store"), []string{m + "/b"}},
		"two have none, named in the order given":  {mode + block(m+"/b"), []string{m + "/a", m + "/store"}},
		"a subpackage's block is not its parent's": {mode + block(m+"/a") + block(m+"/b") + block(m+"/store/sub"), []string{m + "/store"}},
		"a path that only begins the package's":    {mode + block(m+"/a") + block(m+"/b") + block(m+"/storex"), []string{m + "/store"}},
		"a block outside the packages":             {mode + block(m+"/a") + block(m+"/b") + block("example.com/m/cmd/tool"), []string{m + "/store"}},
		"no block at all":                          {mode, []string{m + "/a", m + "/b", m + "/store"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := devctl.MissingPackages(strings.NewReader(tc.profile), pkgs)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	t.Run("a line that is not a block", func(t *testing.T) {
		t.Parallel()
		_, err := devctl.MissingPackages(strings.NewReader(mode+"not a block\n"), pkgs)
		require.Error(t, err)
	})
	t.Run("a module's packages", func(t *testing.T) {
		t.Setenv("CGO_ENABLED", "1")
		dir := t.TempDir()
		for file, src := range map[string]string{
			"go.mod":                           "module example.com/m\n\ngo 1.26\n",
			"internal/cgo/cgo.go":              "package cgo\nimport \"C\"\nfunc F() int { return 1 }\n",
			"internal/testonly/helper_test.go": "package testonly\nfunc helper() int { return 1 }\n",
			"internal/types/excluded.go":       "//go:build ignore\n\npackage types\nfunc F() int { return 1 }\n",
			"internal/code/code.go":            "package code\n\nfunc F() int { return 1 }\n",
			"internal/lit/lit.go":              "package lit\n\n// F is a statement in a function literal.\nvar F = func() int { return 1 }\n",
			"internal/types/types.go":          "package types\n\ntype T struct{}\n\nconst C = 1\n\nfunc (T) M() {}\n",
			"internal/types/types_test.go":     "package types\n\nfunc helper() int { return 1 }\n",
			"cmd/tool/main.go":                 "package main\n\nfunc main() { println() }\n",
			"internal/code/nested/nested.go":   "package nested\n\nfunc G() { _ = 1 }\n",
		} {
			path := filepath.Join(dir, filepath.FromSlash(file))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(src), 0o600))
		}
		got, err := devctl.FloorPackages(t.Context(), dir)
		require.NoError(t, err)
		assert.Equal(t, []devctl.Package{
			{ImportPath: "example.com/m/internal/cgo", GoFiles: []string{"cgo.go"}, HasStatements: true},
			{ImportPath: "example.com/m/internal/code", GoFiles: []string{"code.go"}, HasStatements: true},
			{ImportPath: "example.com/m/internal/code/nested", GoFiles: []string{"nested.go"}, HasStatements: true},
			{ImportPath: "example.com/m/internal/lit", GoFiles: []string{"lit.go"}, HasStatements: true},
			{ImportPath: "example.com/m/internal/testonly"},
			// Its one function is empty, and its test file's statements are
			// not the floor's.
			{ImportPath: "example.com/m/internal/types", GoFiles: []string{"types.go"}},
		}, got, "./internal/... only, cmd/ outside it")

		missing, err := devctl.MissingPackages(strings.NewReader(mode), got)
		require.NoError(t, err)
		assert.Equal(t, []string{"example.com/m/internal/cgo", "example.com/m/internal/code", "example.com/m/internal/code/nested", "example.com/m/internal/lit"}, missing)
	})
}

// TestCoverageCommandNamesAPackageMissingFromTheProfile: devctl coverage,
// the floor step, fails on a profile of this module with one package's
// blocks cut, naming it, though its total meets the floor; the same profile
// whole passes. With binary coverage data (go tool covdata), a package no
// test binary links writes no data, and this is what still counts it.
func TestCoverageCommandNamesAPackageMissingFromTheProfile(t *testing.T) {
	const cut = "github.com/abanna/herdr-agentisan/internal/attention"
	out, err := runDev(t, "coverage", "--profile", floorProfile(t, ""), "--min", "0")
	require.NoError(t, err, out)
	assert.Contains(t, out, "ok")

	out, err = runDev(t, "coverage", "--profile", floorProfile(t, cut), "--min", "0")
	require.ErrorIs(t, err, devctl.ErrMissingCoveragePackages)
	assert.ErrorContains(t, err, cut)
	assert.ErrorContains(t, err, "1 package")
	assert.Contains(t, out, "coverage 0.0% (floor 0.0%) ok", "the total is still reported")

	_, err = runDev(t, "coverage", "--profile", floorProfile(t, cut), "--min", "75")
	require.ErrorIs(t, err, devctl.ErrMissingCoveragePackages)
	assert.ErrorContains(t, err, "below the")
	assert.ErrorContains(t, err, cut)
}

// floorProfile writes a coverage profile with a block in a real file of
// every package the floor counts in this module, but skip, and the extra
// lines after them, and returns its path.
func floorProfile(t *testing.T, skip string, extra ...string) string {
	t.Helper()
	pkgs, err := devctl.FloorPackages(t.Context(), "../..")
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)
	var b strings.Builder
	b.WriteString("mode: atomic\n")
	for _, p := range pkgs {
		if p.ImportPath == skip || !p.HasStatements {
			continue
		}
		b.WriteString(p.ImportPath + "/" + p.GoFiles[0] + ":1.1,1.2 1 0\n")
	}
	for _, line := range extra {
		b.WriteString(line + "\n")
	}
	path := filepath.Join(t.TempDir(), "coverage.out")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
	return path
}

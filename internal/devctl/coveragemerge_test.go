package devctl_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

// writeProfiles writes each body as a profile in a new temp dir and returns
// the paths, in order, and the path a merge should write.
func writeProfiles(t *testing.T, bodies ...string) ([]string, string) {
	t.Helper()

	dir := t.TempDir()
	paths := make([]string, len(bodies))
	for i, body := range bodies {
		paths[i] = filepath.Join(dir, fmt.Sprintf("in%d.out", i))
		require.NoError(t, os.WriteFile(paths[i], []byte(body), 0o600))
	}
	return paths, filepath.Join(dir, "merged.out")
}

func TestMergeCoverageProfilesKeepsOneModeLine(t *testing.T) {
	t.Parallel()

	paths, out := writeProfiles(t,
		"mode: atomic\na.go:1.1,2.2 2 0\nb.go:1.1,3.2 3 1\n",
		"mode: atomic\na.go:1.1,2.2 2 4\nc.go:5.1,6.2 1 0\n",
	)
	rep, err := devctl.MergeCoverageProfiles(out, paths)
	require.NoError(t, err)

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "mode: atomic\na.go:1.1,2.2 2 0\nb.go:1.1,3.2 3 1\na.go:1.1,2.2 2 4\nc.go:5.1,6.2 1 0\n", string(raw),
		"one mode line, then every block of every profile, for go tool cover to add up")
	// a.go's block is in both profiles: counted once.
	assert.Equal(t, devctl.MergeReport{Path: out, Profiles: 2, Blocks: 3, Statements: 6}, rep)
	assert.Contains(t, rep.String(), "6 statements")
}

func TestMergeCoverageProfilesRefusesWhatGoToolCoverWouldMisread(t *testing.T) {
	t.Parallel()

	ok := "mode: atomic\na.go:1.1,2.2 2 0\n"
	tests := map[string]struct {
		bodies []string
		want   string
	}{
		"an empty profile":         {[]string{ok, ""}, "no mode line"},
		"no mode line":             {[]string{"a.go:1.1,2.2 2 0\n"}, "no mode line"},
		"a mode line with no mode": {[]string{"mode: \na.go:1.1,2.2 2 0\n"}, "no mode line"},
		"two modes":                {[]string{ok, "mode: set\na.go:1.1,2.2 2 1\n"}, "mode set"},
		"a second mode line":       {[]string{ok + "mode: atomic\n"}, "malformed"},
		"a malformed block":        {[]string{"mode: atomic\na.go:1.1,2.2 x 0\n"}, "malformed"},
		"a block with no count":    {[]string{"mode: atomic\na.go:1.1,2.2 2\n"}, "malformed"},
		"no blocks":                {[]string{ok, "mode: atomic\n"}, "no blocks"},
		"a block's statements differ between profiles": {
			[]string{ok, "mode: atomic\na.go:1.1,2.2 3 1\n"}, "statements",
		},
		"a mode go does not write":    {[]string{"mode: often\na.go:1.1,2.2 2 0\n"}, "mode often"},
		"a CR inside a block line":    {[]string{"mode: atomic\na\r.go:1.1,2.2 2 0\n"}, "malformed"},
		"a NUL in a block line":       {[]string{"mode: atomic\na\x00.go:1.1,2.2 2 0\n"}, "malformed"},
		"a byte-order mark first":     {[]string{"\ufeffmode: atomic\na.go:1.1,2.2 2 0\n"}, "no mode line"},
		"negative statements":         {[]string{"mode: atomic\na.go:1.1,2.2 -1 0\n"}, "malformed"},
		"a count past 64 bits":        {[]string{"mode: atomic\na.go:1.1,2.2 1 18446744073709551616\n"}, "malformed"},
		"statements past 64 bits":     {[]string{"mode: atomic\na.go:1.1,2.2 9223372036854775808 1\n"}, "malformed"},
		"no position":                 {[]string{"mode: atomic\na.go 2 0\n"}, "malformed"},
		"a line past the 1 MiB limit": {[]string{"mode: atomic\n" + strings.Repeat("a", 1<<20) + ".go:1.1,2.2 1 0\n"}, "too long"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths, out := writeProfiles(t, tc.bodies...)
			_, err := devctl.MergeCoverageProfiles(out, paths)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
			assert.NoFileExists(t, out, "a refused merge writes nothing")
			assertOnly(t, filepath.Dir(out), len(paths), "a refused merge leaves no temporary file")
		})
	}

	t.Run("no output", func(t *testing.T) {
		t.Parallel()

		paths, _ := writeProfiles(t, ok)
		_, err := devctl.MergeCoverageProfiles("", paths)
		require.Error(t, err)
	})
	t.Run("no profiles", func(t *testing.T) {
		t.Parallel()

		out := filepath.Join(t.TempDir(), "merged.out")
		_, err := devctl.MergeCoverageProfiles(out, nil)
		require.Error(t, err)
		assert.NoFileExists(t, out)
	})
	t.Run("a directory for a profile", func(t *testing.T) {
		t.Parallel()

		paths, out := writeProfiles(t, ok)
		_, err := devctl.MergeCoverageProfiles(out, append(paths, t.TempDir()))
		require.Error(t, err)
		assert.ErrorContains(t, err, "not a regular file")
		assert.NoFileExists(t, out)
	})

	t.Run("a missing profile", func(t *testing.T) {
		t.Parallel()

		paths, out := writeProfiles(t, ok)
		_, err := devctl.MergeCoverageProfiles(out, append(paths, filepath.Join(t.TempDir(), "gone.out")))
		require.Error(t, err)
		assert.ErrorContains(t, err, "gone.out")
		assert.NoFileExists(t, out)
	})
}

// coverageBodies are where coverage.go's CoverageResult.Meets and .String
// bodies sit now, as profile positions, so the profiles below name real code.
func coverageBodies(t *testing.T) (meets, str string) {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "coverage.go", nil, 0)
	require.NoError(t, err)
	at := map[string]string{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil {
			from, to := fset.Position(fn.Body.Lbrace), fset.Position(fn.Body.Rbrace)
			at[fn.Name.Name] = fmt.Sprintf("%d.%d,%d.%d", from.Line, from.Column+1, to.Line, to.Column)
		}
	}
	require.Contains(t, at, "Meets")
	require.Contains(t, at, "String")
	return at["Meets"], at["String"]
}

// TestCoverageCountsAMergedBlockOnce pins what `devctl coverage` does with a
// merged profile: a block in two jobs' profiles counts its statements once,
// covered when either job ran it. Meets has 1 statement, run in the second
// profile only; String has 3, run in neither. Summed counts, counted once,
// give 1/4 = 25.0%. Counted twice, it would be 1/8; the first profile's
// count alone, 0/4.
func TestCoverageCountsAMergedBlockOnce(t *testing.T) {
	meets, str := coverageBodies(t)
	file := "github.com/abanna/herdr-agentisan/internal/devctl/coverage.go"
	paths, out := writeProfiles(t,
		fmt.Sprintf("mode: atomic\n%s:%s 1 0\n%s:%s 3 0\n", file, meets, file, str),
		fmt.Sprintf("mode: atomic\n%s:%s 1 2\n%s:%s 3 0\n", file, meets, file, str),
	)
	_, err := devctl.MergeCoverageProfiles(out, paths)
	require.NoError(t, err)

	got, err := runDev(t, "coverage", "--profile", out, "--min", "25")
	require.NoError(t, err)
	assert.Contains(t, got, "coverage 25.0% ")

	_, err = runDev(t, "coverage", "--profile", out, "--min", "25.1")
	require.Error(t, err)
	assert.ErrorContains(t, err, "below the")

	t.Run("two profiles pasted together are refused, not misread", func(t *testing.T) {
		raw0, err := os.ReadFile(paths[0])
		require.NoError(t, err)
		raw1, err := os.ReadFile(paths[1])
		require.NoError(t, err)
		pasted := filepath.Join(t.TempDir(), "pasted.out")
		require.NoError(t, os.WriteFile(pasted, append(raw0, raw1...), 0o600))

		_, err = runDev(t, "coverage", "--profile", pasted, "--min", "0")
		require.Error(t, err, "go tool cover fails on a second mode line")
	})
}

// TestCoverageMergeCommandRefusesAMissingShard: a test job dropped from
// ci.yml, or a shard with no job, leaves its profile missing.
func TestCoverageMergeCommandRefusesAMissingShard(t *testing.T) {
	dir := t.TempDir()
	for _, shard := range devctl.Shards()[1:] {
		body := "mode: atomic\na.go:1.1,2.2 2 1\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, devctl.ShardProfile(shard)), []byte(body), 0o600))
	}
	out := filepath.Join(dir, "coverage.out")

	_, err := runDev(t, "coverage-merge", "--dir", dir, "--out", out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "no coverage profile for test shard dashboard")
	assert.ErrorContains(t, err, "coverage-dashboard.out")
	assert.NoFileExists(t, out)
}

// assertOnly checks dir holds exactly n entries: the inputs, and nothing a
// merge left behind.
func assertOnly(t *testing.T, dir string, n int, msg string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Len(t, names, n, "%s: %v", msg, names)
}

// TestMergeCoverageProfilesReplacesItsOutputWhole: an output already there
// is replaced, never written through or into.
func TestMergeCoverageProfilesReplacesItsOutputWhole(t *testing.T) {
	t.Parallel()

	body := "mode: atomic\na.go:1.1,2.2 2 1\n"
	t.Run("an old profile", func(t *testing.T) {
		t.Parallel()

		paths, out := writeProfiles(t, body)
		require.NoError(t, os.WriteFile(out, []byte("mode: atomic\nold.go:1.1,2.2 9 9\n"), 0o600))
		_, err := devctl.MergeCoverageProfiles(out, paths)
		require.NoError(t, err)
		raw, err := os.ReadFile(out)
		require.NoError(t, err)
		assert.Equal(t, body, string(raw))
	})
	t.Run("a symlink, and a symlink where a temporary file might go", func(t *testing.T) {
		t.Parallel()

		paths, out := writeProfiles(t, body)
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(t, os.WriteFile(target, []byte("keep"), 0o600))
		require.NoError(t, os.Symlink(target, out))
		require.NoError(t, os.Symlink(target, out+".tmp"))
		_, err := devctl.MergeCoverageProfiles(out, paths)
		require.NoError(t, err)
		raw, err := os.ReadFile(target)
		require.NoError(t, err)
		assert.Equal(t, "keep", string(raw), "nothing is written through a link")
		info, err := os.Lstat(out)
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular(), "the link itself is replaced")
	})
	t.Run("a directory", func(t *testing.T) {
		t.Parallel()

		paths, out := writeProfiles(t, body)
		require.NoError(t, os.Mkdir(out, 0o755))
		_, err := devctl.MergeCoverageProfiles(out, paths)
		require.Error(t, err)
		assertOnly(t, filepath.Dir(out), len(paths)+1, "the inputs and the directory, no temporary file")
	})
	t.Run("a parent that does not exist", func(t *testing.T) {
		t.Parallel()

		paths, _ := writeProfiles(t, body)
		_, err := devctl.MergeCoverageProfiles(filepath.Join(t.TempDir(), "gone", "merged.out"), paths)
		require.Error(t, err)
	})
}

// TestMergeCoverageProfilesTwiceAtOnce: two merges to one output at once
// each write a whole profile; neither sees the other's half.
func TestMergeCoverageProfilesTwiceAtOnce(t *testing.T) {
	t.Parallel()

	body := "mode: atomic\n" + strings.Repeat("a.go:1.1,2.2 2 1\n", 5000)
	paths, out := writeProfiles(t, body)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := devctl.MergeCoverageProfiles(out, paths)
			errs <- err
		}()
	}
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, body, string(raw))
	assertOnly(t, filepath.Dir(out), 2, "the input and the output, no temporary file")
}

// TestMergeCoverageProfilesTakesALineUpToItsLimit: a line one byte under the
// 1 MiB limit merges; one past it is refused (above).
func TestMergeCoverageProfilesTakesALineUpToItsLimit(t *testing.T) {
	t.Parallel()

	suffix := ".go:1.1,2.2 1 0"
	line := strings.Repeat("a", 1<<20-1-len(suffix)) + suffix
	paths, out := writeProfiles(t, "mode: atomic\n"+line+"\n")
	rep, err := devctl.MergeCoverageProfiles(out, paths)
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Blocks)
}

// TestCoverageMergeCommandResolvesRelativePathsAtTheModuleRoot: --dir and
// --out, given relative, are read from the module root, not the working
// directory (this test runs in internal/devctl).
func TestCoverageMergeCommandResolvesRelativePathsAtTheModuleRoot(t *testing.T) {
	dir := t.TempDir()
	for _, shard := range devctl.Shards() {
		body := "mode: atomic\na.go:1.1,2.2 2 1\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, devctl.ShardProfile(shard)), []byte(body), 0o600))
	}
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	rel, err := filepath.Rel(root, dir)
	require.NoError(t, err)

	got, err := runDev(t, "coverage-merge", "--dir", rel, "--out", filepath.Join(rel, "coverage.out"))
	require.NoError(t, err)
	assert.Contains(t, got, "merged 5 profiles")
	raw, err := os.ReadFile(filepath.Join(dir, "coverage.out"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(raw), "mode: "))
	assert.Equal(t, len(devctl.Shards()), strings.Count(string(raw), "a.go:"))
}

// TestMergeCoverageProfilesWritesWhatGoToolCoverReads: each profile is read
// through a link to it, and its blocks written as cover reads them.
func TestMergeCoverageProfilesWritesWhatGoToolCoverReads(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ in, want string }{
		// bufio.ScanLines drops the CR of a CRLF, for cover as for the merge.
		"CRLF, read as LF": {"mode: atomic\r\na.go:1.1,2.2 2 0\r\n", "mode: atomic\na.go:1.1,2.2 2 0\n"},
		// File names as bytes: non-ASCII, not UTF-8, a cut sequence, a
		// surrogate's bytes, and a space, which cover reads from the line's end.
		"names as bytes": {
			"mode: set\nx\xff\xe2\x82.go:1.1,2.2 1 1\n\u00e9\xed\xa0\x80.go:3.1,4.2 2 0\nmy file.go:5.1,6.2 1 1\n", "",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths, out := writeProfiles(t, tc.in)
			link := filepath.Join(t.TempDir(), "linked.out")
			require.NoError(t, os.Symlink(paths[0], link))
			_, err := devctl.MergeCoverageProfiles(out, []string{link})
			require.NoError(t, err)
			raw, err := os.ReadFile(out)
			require.NoError(t, err)
			want := tc.want
			if want == "" {
				want = tc.in
			}
			assert.Equal(t, want, string(raw))
		})
	}
}

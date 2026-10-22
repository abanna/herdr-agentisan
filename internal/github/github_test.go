package github_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/github/ghtest"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// slow bounds what a loaded machine may take to start and reap a shell.
const slow = 10 * time.Second

const repo = "abanna/herdr-agentisan"

// issuesQuery is the GraphQL the issue count asks, written out here so the
// test cannot share a typo with the code.
const issuesQuery = `query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { issues(states: OPEN) { totalCount } } }`

func recorded(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(raw)
}

func runner(f *ghtest.Fake) github.Runner {
	return github.Runner{Bin: f.Bin, Env: f.Env()}
}

// TestRunnerPRs runs `gh pr list` for one repo, with exactly the arguments
// herdr-status.sh's call has, each a flag=value so no value can read as a
// flag, and maps what it prints.
func TestRunnerPRs(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Answer(t, recorded(t, "pr-list.json"), recorded(t, "issues.json"))
	prs, err := runner(f).PRs(t.Context(), repo)
	require.NoError(t, err)
	assert.Len(t, prs, 5)
	assert.Equal(t, []string{
		"pr", "list", "--repo=abanna/herdr-agentisan", "--state=open", "--limit=100",
		"--json=number,headRefName,headRefOid,mergeStateStatus,reactionGroups,statusCheckRollup",
	}, f.Args(t, "pr"))
	assert.Equal(t, []string{"pr"}, f.Calls(t), "one call, and no issue count")
}

// TestRunnerPRView reads the state of a PR that left the open list, as
// `gh pr view <n> --repo=<repo> --json=state,mergeCommit`: merged with its
// merge commit, closed, or still open. Only that call's answer fails it: the
// list may answer while a lookup fails.
func TestRunnerPRView(t *testing.T) {
	t.Parallel()
	const oid = "e5f63b82f751b5bc2d44f874f045ae90a2789688"
	f := ghtest.New(t)
	f.View(t, 50, `{"mergeCommit":{"oid":"`+oid+`"},"state":"MERGED"}`)
	f.View(t, 51, `{"mergeCommit":null,"state":"CLOSED"}`)
	f.View(t, 59, `{"mergeCommit":null,"state":"OPEN"}`)
	r := runner(f)
	for _, tc := range []struct {
		n    int
		want github.PRState
	}{
		{50, github.PRState{State: github.StateMerged, MergeCommit: oid}},
		{51, github.PRState{State: github.StateClosed}},
		{59, github.PRState{State: github.StateOpen}},
	} {
		got, err := r.PRView(t.Context(), repo, tc.n)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "PR %d", tc.n)
	}
	assert.Equal(t, []string{"pr", "view", "59", "--repo=abanna/herdr-agentisan", "--json=state,mergeCommit"}, f.Args(t, "view"))

	_, err := r.PRView(t.Context(), repo, 60)
	require.ErrorIs(t, err, github.ErrGh, "a PR the stand-in has no answer for")
	assert.Contains(t, err.Error(), "Could not resolve")

	f.CallMode(t, "view", ghtest.ModeFail)
	_, err = r.PRView(t.Context(), repo, 50)
	require.ErrorIs(t, err, github.ErrGh)
	_, err = r.OpenPRs(t.Context(), repo)
	require.NoError(t, err, "the list still answers")

	calls := len(f.Calls(t))
	for _, n := range []int{0, -3} {
		_, err = r.PRView(t.Context(), repo, n)
		require.ErrorIs(t, err, github.ErrNumber)
	}
	_, err = r.PRView(t.Context(), "-x/y", 50)
	require.ErrorIs(t, err, github.ErrRepo)
	assert.Len(t, f.Calls(t), calls, "a bad number or repo never runs gh")
}

// TestRunnerOpenIssues asks GitHub's GraphQL for the exact count, the owner
// and name passed as raw string fields: -f, never -F, which would read a
// value starting with @ as a file.
func TestRunnerOpenIssues(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Answer(t, "[]\n", `{"data":{"repository":{"issues":{"totalCount":14}}}}`)
	n, err := runner(f).OpenIssues(t.Context(), repo)
	require.NoError(t, err)
	assert.Equal(t, 14, n)
	assert.Equal(t, []string{"api", "graphql", "-f", "query=" + issuesQuery, "-f", "owner=abanna", "-f", "name=herdr-agentisan"},
		f.Args(t, "api"))
}

// TestRunnerEnvironment: gh gets the environment it is given, plus the
// settings that keep it from prompting, colouring or checking for updates,
// and its stdin is empty, its output pipes.
func TestRunnerEnvironment(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	r := github.Runner{Bin: f.Bin, Env: f.Env("PROBE=given", "NO_COLOR=")}
	_, err := r.PRs(t.Context(), repo)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"GH_PROMPT_DISABLED": "1", "NO_COLOR": "1", "GH_NO_UPDATE_NOTIFIER": "1", "PROBE": "given", "TTY": "none",
	}, f.Seen(t), "and no terminal on any of its stdio: it has nobody to prompt")
}

// TestRunnerInheritsTheEnvironment: with no environment given, gh gets the
// daemon's own, which carries what its keyring login needs. The test strips
// every GH_* variable and GITHUB_TOKEN first, so the stand-in never sees a
// real token.
func TestRunnerInheritsTheEnvironment(t *testing.T) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GH_") || strings.HasPrefix(k, "GITHUB_") {
			t.Setenv(k, "")
			require.NoError(t, os.Unsetenv(k))
		}
	}
	f := ghtest.New(t)
	t.Setenv("FAKE_GH_DIR", f.Dir)
	t.Setenv("PROBE", "inherited")
	_, err := github.Runner{Bin: f.Bin}.PRs(t.Context(), repo)
	require.NoError(t, err)
	assert.Equal(t, "inherited", f.Seen(t)["PROBE"])
	assert.Equal(t, "1", f.Seen(t)["GH_PROMPT_DISABLED"])
}

// TestRunnerFailures: every way gh can fail is one of the package's
// sentinels, with gh's first stderr line as the reason; a gh that cannot
// start never runs.
func TestRunnerFailures(t *testing.T) {
	t.Parallel()
	notExec := filepath.Join(t.TempDir(), "gh")
	require.NoError(t, os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600))
	fifo := filepath.Join(t.TempDir(), "gh")
	require.NoError(t, syscall.Mkfifo(fifo, 0o700))
	shut := t.TempDir()
	behind := filepath.Join(shut, "bin", "gh")
	require.NoError(t, os.MkdirAll(filepath.Dir(behind), 0o700))
	require.NoError(t, os.WriteFile(behind, []byte("#!/bin/sh\n"), 0o700)) // #nosec G306 -- executable on purpose; never reached
	require.NoError(t, os.Chmod(shut, 0o000))
	t.Cleanup(func() { _ = os.Chmod(shut, 0o700) })
	tests := map[string]struct {
		bin     func(f *ghtest.Fake) string
		arrange func(t *testing.T, f *ghtest.Fake)
		want    error
		reason  string
		noCall  bool
	}{
		"no gh":                      {bin: func(*ghtest.Fake) string { return "" }, want: github.ErrNoGh, noCall: true},
		"a relative path":            {bin: func(*ghtest.Fake) string { return "gh" }, want: github.ErrNoGh, noCall: true},
		"gh is not there":            {bin: func(f *ghtest.Fake) string { return filepath.Join(f.Dir, "missing", "gh") }, want: github.ErrNoGh, noCall: true},
		"gh is not executable":       {bin: func(*ghtest.Fake) string { return notExec }, want: github.ErrNoGh, noCall: true},
		"gh is a directory":          {bin: func(f *ghtest.Fake) string { return f.Dir }, want: github.ErrNoGh, noCall: true},
		"gh is a dangling symlink":   {bin: func(f *ghtest.Fake) string { return link(t, filepath.Join(f.Dir, "gone")) }, want: github.ErrNoGh, noCall: true},
		"gh is a FIFO":               {bin: func(*ghtest.Fake) string { return fifo }, want: github.ErrNoGh, noCall: true},
		"gh behind a shut directory": {bin: func(*ghtest.Fake) string { return behind }, want: github.ErrNoGh, noCall: true},
		"not logged in": {arrange: func(t *testing.T, f *ghtest.Fake) {
			f.Fail(t, "HTTP 401: Bad credentials (https://api.github.com/graphql)\nTry authenticating with:  gh auth login\n")
		}, want: github.ErrGh, reason: "HTTP 401: Bad credentials (https://api.github.com/graphql)"},
		"no such repo": {arrange: func(t *testing.T, f *ghtest.Fake) {
			f.Fail(t, "GraphQL: Could not resolve to a Repository with the name 'abanna/gone'. (repository)\n")
		}, want: github.ErrGh, reason: "Could not resolve to a Repository"},
		"rate limited": {arrange: func(t *testing.T, f *ghtest.Fake) {
			f.Fail(t, "GraphQL: API rate limit exceeded for user ID 1.\n")
		}, want: github.ErrGh, reason: "API rate limit exceeded"},
		"a silent failure":   {arrange: func(t *testing.T, f *ghtest.Fake) { f.Fail(t, "") }, want: github.ErrGh},
		"killed by a signal": {arrange: func(t *testing.T, f *ghtest.Fake) { f.Mode(t, ghtest.ModeKill) }, want: github.ErrGh},
		"bad output": {arrange: func(t *testing.T, f *ghtest.Fake) {
			f.Answer(t, "Welcome to GitHub CLI!\n", "{")
		}, want: github.ErrOutput},
	}
	if os.Geteuid() == 0 {
		delete(tests, "gh behind a shut directory") // root searches it anyway
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := ghtest.New(t)
			if tc.arrange != nil {
				tc.arrange(t, f)
			}
			r := runner(f)
			if tc.bin != nil {
				r.Bin = tc.bin(f)
			}
			prs, err := r.PRs(t.Context(), repo)
			require.ErrorIs(t, err, tc.want)
			assert.Nil(t, prs)
			_, err = r.OpenIssues(t.Context(), repo)
			require.ErrorIs(t, err, tc.want)
			if tc.reason != "" {
				assert.Contains(t, err.Error(), tc.reason)
				assert.NotContains(t, err.Error(), "gh auth login", "only the first line of stderr")
			}
			if tc.noCall {
				assert.Empty(t, f.Calls(t), "gh never ran")
			}
		})
	}
}

// link is a symlink in a temp dir to target.
func link(t *testing.T, target string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	require.NoError(t, os.Symlink(target, p))
	return p
}

// TestRunnerFollowsASymlink: gh installed as a symlink, as version managers
// install it, runs.
func TestRunnerFollowsASymlink(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	r := runner(f)
	r.Bin = link(t, f.Bin)
	n, err := r.OpenIssues(t.Context(), repo)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, []string{"api"}, f.Calls(t))
}

// TestRunnerRefusesABadRepo: a repo must be owner/name as GitHub allows
// them (an owner of at most 39 characters, a name of at most 100, neither
// "." nor ".."), or gh never runs: nothing else can reach its argv.
func TestRunnerRefusesABadRepo(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		repo string
		ok   bool
	}{
		"owner/name":                {repo: "abanna/herdr-agentisan", ok: true},
		"dots and underscores":      {repo: "a-1/x.y_z", ok: true},
		"the longest owner":         {repo: strings.Repeat("a", 39) + "/x", ok: true},
		"an owner one too long":     {repo: strings.Repeat("a", 40) + "/x"},
		"the longest name":          {repo: "a/" + strings.Repeat("x", 100), ok: true},
		"a name one too long":       {repo: "a/" + strings.Repeat("x", 101)},
		"empty":                     {repo: ""},
		"no slash":                  {repo: "herdr-agentisan"},
		"no owner":                  {repo: "/herdr-agentisan"},
		"no name":                   {repo: "abanna/"},
		"a path":                    {repo: "a/b/c"},
		"a name of dots":            {repo: "abanna/.."},
		"a name of one dot":         {repo: "abanna/."},
		"an owner like a flag":      {repo: "-R/x"},
		"a name like a flag":        {repo: "a/--json", ok: true}, // one argv element, after --repo=
		"an owner starting with _":  {repo: "_a/x"},
		"a space":                   {repo: "abanna/herdr agentisan"},
		"a shell metacharacter":     {repo: "abanna/x;rm"},
		"a newline":                 {repo: "abanna/x\n"},
		"a NUL":                     {repo: "abanna/x\x00"},
		"non-ASCII":                 {repo: "äbanna/x"},
		"not UTF-8":                 {repo: "abanna/x\xff"},
		"a URL":                     {repo: "https://github.com/abanna/x"},
		"a host":                    {repo: "github.com/abanna/x"},
		"an @ for a file":           {repo: "@a/x"},
		"a fullwidth slash":         {repo: "abanna／x"},
		"a decomposed accent (NFD)": {repo: "abanna/e\u0301"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := ghtest.New(t)
			_, prsErr := runner(f).PRs(t.Context(), tc.repo)
			_, issuesErr := runner(f).OpenIssues(t.Context(), tc.repo)
			if tc.ok {
				require.NoError(t, prsErr)
				require.NoError(t, issuesErr)
				assert.Contains(t, f.Args(t, "pr"), "--repo="+tc.repo)
				owner, name, _ := strings.Cut(tc.repo, "/")
				assert.Equal(t, []string{"-f", "owner=" + owner, "-f", "name=" + name}, f.Args(t, "api")[4:])
				return
			}
			require.ErrorIs(t, prsErr, github.ErrRepo)
			require.ErrorIs(t, issuesErr, github.ErrRepo)
			assert.Empty(t, f.Calls(t), "gh never ran")
		})
	}
}

// TestRunnerCutsALongReason: an error carries at most 200 bytes of gh's
// stderr, still valid UTF-8 where the cut splits a character.
func TestRunnerCutsALongReason(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Fail(t, strings.Repeat("x", 199)+"é and more\n")
	_, err := runner(f).PRs(t.Context(), repo)
	require.ErrorIs(t, err, github.ErrGh)
	assert.Contains(t, err.Error(), ": "+strings.Repeat("x", 199))
	assert.NotContains(t, err.Error(), "and more")
	assert.True(t, utf8.ValidString(err.Error()))
}

// TestRunnerStderrOnSuccess: gh's warnings on a call that succeeds are not
// a failure.
func TestRunnerStderrOnSuccess(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Mode(t, ghtest.ModeWarn)
	prs, err := runner(f).PRs(t.Context(), repo)
	require.NoError(t, err)
	assert.Equal(t, []snapshot.PR{}, prs)
	n, err := runner(f).OpenIssues(t.Context(), repo)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestRunnerTimeoutKillsTheGroup: a gh that hangs is killed at the timeout,
// and so is every process it started: the whole group goes.
func TestRunnerTimeoutKillsTheGroup(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Mode(t, ghtest.ModeHang)
	r := runner(f)
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := r.PRs(t.Context(), repo)
	require.ErrorIs(t, err, github.ErrTimeout)
	assert.Less(t, time.Since(start), slow)

	raw, err := os.ReadFile(f.Child())
	require.NoError(t, err, "the hanging gh started its child")
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
	}, slow, 10*time.Millisecond, "gh's child outlived the timeout")
}

// TestRunnerOutputCap: a gh that prints more than the cap is stopped as soon
// as it passes it, well before the timeout, and its output is ErrOutput.
func TestRunnerOutputCap(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Mode(t, ghtest.ModeFlood)
	r := runner(f)
	r.MaxOutput = 1024
	r.Timeout = time.Minute
	start := time.Now()
	_, err := r.PRs(t.Context(), repo)
	require.ErrorIs(t, err, github.ErrOutput)
	assert.Contains(t, err.Error(), "1024 bytes")
	assert.Less(t, time.Since(start), slow)
}

// TestRunnerOutputCapBoundary: output of exactly the cap is read; one byte
// more is ErrOutput.
func TestRunnerOutputCapBoundary(t *testing.T) {
	t.Parallel()
	body := recorded(t, "pr-list.json")
	for name, tc := range map[string]struct {
		max  int
		want error
	}{
		"exactly the cap": {max: len(body)},
		"one byte past":   {max: len(body) - 1, want: github.ErrOutput},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := ghtest.New(t)
			f.Answer(t, body, "{}")
			r := runner(f)
			r.MaxOutput = tc.max
			prs, err := r.PRs(t.Context(), repo)
			require.ErrorIs(t, err, tc.want)
			if tc.want == nil {
				assert.Len(t, prs, 5)
			}
		})
	}
}

// TestRunnerHonoursTheContext: the caller's cancel ends a call with the
// context's error, not a timeout, and kills gh.
func TestRunnerHonoursTheContext(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Mode(t, ghtest.ModeHang)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := runner(f).OpenIssues(ctx, repo)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, github.ErrTimeout)
	assert.Less(t, time.Since(start), slow)
}

// TestFind looks gh up in a PATH value, as the daemon's environment has it,
// never the test's: the first absolute directory holding an executable
// file named gh, through a symlink too. Anything else named gh, and any
// directory that cannot be read, is passed over.
func TestFind(t *testing.T) {
	t.Parallel()
	exe := func(dir string) string {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		p := filepath.Join(dir, "gh")
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700)) // #nosec G306 -- executable on purpose; never run
		return p
	}
	dirWith := func(name string, make func(p string) error) string {
		d := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.MkdirAll(d, 0o700))
		require.NoError(t, make(filepath.Join(d, "gh")))
		return d
	}
	root := t.TempDir()
	first, second := exe(filepath.Join(root, "a")), exe(filepath.Join(root, "b"))
	a, b := filepath.Dir(first), filepath.Dir(second)
	plain := dirWith("plain", func(p string) error { return os.WriteFile(p, []byte("x"), 0o600) })
	asDir := dirWith("dir", func(p string) error { return os.Mkdir(p, 0o700) })
	fifo := dirWith("fifo", func(p string) error { return syscall.Mkfifo(p, 0o700) })
	link := dirWith("link", func(p string) error { return os.Symlink(first, p) })
	dangling := dirWith("dangling", func(p string) error { return os.Symlink(filepath.Join(root, "gone", "gh"), p) })
	locked := exe(filepath.Join(root, "locked", "bin"))
	require.NoError(t, os.Chmod(filepath.Dir(filepath.Dir(locked)), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(filepath.Dir(locked)), 0o700) })
	odd := map[string]string{}
	for _, name := range []string{"bin é", "bin\xff", "-bin", "bin:x"} {
		odd[name] = filepath.Dir(exe(filepath.Join(t.TempDir(), name)))
	}
	empty := filepath.Join(root, "empty")
	require.NoError(t, os.MkdirAll(empty, 0o700))
	join := func(dirs ...string) string { return strings.Join(dirs, string(os.PathListSeparator)) }

	tests := map[string]struct {
		path, want string
	}{
		"unset":                         {path: "", want: ""},
		"the first that has it":         {path: join(empty, a, b), want: first},
		"not executable is skipped":     {path: join(plain, b), want: second},
		"a directory named gh":          {path: join(asDir, b), want: second},
		"a FIFO named gh":               {path: join(fifo, b), want: second},
		"a symlink to gh":               {path: join(link, b), want: filepath.Join(link, "gh")},
		"a dangling symlink":            {path: join(dangling, b), want: second},
		"a directory it cannot search":  {path: join(filepath.Dir(locked), b), want: second},
		"a relative entry is skipped":   {path: join(".", "a", "../b", "ghtest", b), want: second},
		"an empty entry is skipped":     {path: join("", b), want: second},
		"nowhere":                       {path: join(empty, plain, asDir), want: ""},
		"a missing directory":           {path: join(filepath.Join(root, "gone")), want: ""},
		"a NUL in an entry":             {path: join(a+"\x00", b), want: second},
		"a non-ASCII directory":         {path: odd["bin é"], want: filepath.Join(odd["bin é"], "gh")},
		"a non-UTF-8 directory":         {path: odd["bin\xff"], want: filepath.Join(odd["bin\xff"], "gh")},
		"a directory like an option":    {path: odd["-bin"], want: filepath.Join(odd["-bin"], "gh")},
		"a separator splits a name":     {path: odd["bin:x"], want: ""},
		"many entries, the last has it": {path: join(append(slices.Repeat([]string{empty}, 200), b)...), want: second},
	}
	if os.Geteuid() == 0 {
		delete(tests, "a directory it cannot search") // root searches it anyway
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, github.Find(tc.path))
		})
	}
}

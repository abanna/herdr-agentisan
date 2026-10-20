package daemon_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/github/ghtest"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// ghConfig writes a config dir with project agentisan, coloured, on
// abanna/herdr-agentisan, the ◆ spaces' project, and project other, a tabs
// team, on acme/other.
func ghConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "schema_version = 1\n[spaces]\nproject = \"agentisan\"\n" +
		"[projects.agentisan]\nrepo = \"~/Development/agentisan\"\ncolor = \"#7c3aed\"\ngithub = \"abanna/herdr-agentisan\"\n" +
		"[projects.other]\nrepo = \"/elsewhere\"\nshort = \"o\"\ngithub = \"acme/other\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(body), 0o600))
	return dir
}

// recordedPRs is the PR list recorded from abanna/herdr-agentisan, as
// internal/github maps it.
func recordedPRs() []snapshot.PR {
	pr := func(n int, m snapshot.MergeState) snapshot.PR {
		return snapshot.PR{Number: n, CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: m}
	}
	return []snapshot.PR{pr(21, snapshot.MergeClean), pr(22, snapshot.MergeClean), pr(23, snapshot.MergeDirty), pr(24, snapshot.MergeClean), pr(26, snapshot.MergeDirty)}
}

// recordedGh is a stand-in gh that answers the recorded PR list and 14 open
// issues.
func recordedGh(t *testing.T) *ghtest.Fake {
	t.Helper()
	f := ghtest.New(t)
	raw, err := os.ReadFile("../github/testdata/pr-list.json")
	require.NoError(t, err)
	f.Answer(t, string(raw), `{"data":{"repository":{"issues":{"totalCount":14}}}}`)
	return f
}

// ghOpts are teamOpts reading configDir, collecting with f on ticks.
func ghOpts(t *testing.T, stateDir string, th *teamHerdr, configDir string, f *ghtest.Fake, ticks <-chan time.Time) daemon.Options {
	t.Helper()
	o := teamOpts(t, stateDir, th, configDir)
	o.Team.GitHub = daemon.WithGitHubTicks(daemon.GitHubOptions{Gh: f.Bin, Env: f.Env()}, ticks)
	return o
}

// hasHeader is a snapshot whose project has both its PRs and its count.
func hasHeader(s snapshot.Snapshot) bool {
	return s.Project != nil && s.Project.PRs != nil && s.Project.Issues != nil
}

func noPRs(s snapshot.Snapshot) bool {
	return s.Project == nil || (s.Project.PRs == nil && s.Project.Issues == nil)
}

// calls waits until f has had n calls, and then n exactly.
func calls(t *testing.T, f *ghtest.Fake, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(f.Calls(t)) >= n }, ready, 5*time.Millisecond, "gh was called %d times, want %d", len(f.Calls(t)), n)
	assert.Len(t, f.Calls(t), n)
}

// TestSnapshotCarriesTheGitHubHeader: the snapshot's project carries its
// repo's open PRs and open-issue count beside its colour (ADR-001 A29), as
// one strict snapshot v1 document, with gh asked for exactly that repo.
func TestSnapshotCarriesTheGitHubHeader(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	f := recordedGh(t)
	dir := shortDir(t)
	o := ghOpts(t, dir, th, ghConfig(t), f, nil)
	o.Now = func() time.Time { return t0 }
	run(t, o)

	got := served(t, socketPath(t, dir, th.path), hasHeader)
	assert.Equal(t, &snapshot.Project{Color: "#7c3aed", PRs: recordedPRs(), Issues: new(14)}, got.Project)
	calls(t, f, 4)
	assert.Equal(t, wantFullTeam().Groups, got.Groups, "the team is served as before")
	assert.Contains(t, f.Args(t, "pr"), "--repo=acme/other", "every configured repo is collected")
}

// TestSnapshotHeaderWithoutAColour: a project with a repo but no colour
// still gets its header.
func TestSnapshotHeaderWithoutAColour(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	cfg := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfg, settings.FileName),
		[]byte("schema_version = 1\n[projects.app]\nrepo = \"/x\"\ngithub = \"acme/app\"\n"), 0o600))
	f := ghtest.New(t)
	dir := shortDir(t)
	run(t, ghOpts(t, dir, th, cfg, f, nil))

	got := served(t, socketPath(t, dir, th.path), hasHeader)
	assert.Equal(t, &snapshot.Project{PRs: []snapshot.PR{}, Issues: new(0)}, got.Project, "none open is reported as none, not left out")
	assert.Equal(t, []string{
		"pr", "list", "--repo=acme/app", "--state=open", "--limit=100",
		"--json=number,headRefName,mergeStateStatus,reactionGroups,statusCheckRollup",
	}, f.Args(t, "pr"))
}

// TestGitHubPolledOncePerInterval: GitHub is asked once per interval, per
// repo, however many dashboards read the snapshot meanwhile (ADR-001 #9's
// done-when): the op serves the cache and never runs gh.
func TestGitHubPolledOncePerInterval(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	f := recordedGh(t)
	ticks := make(chan time.Time)
	dir := shortDir(t)
	run(t, ghOpts(t, dir, th, ghConfig(t), f, ticks))
	sock := socketPath(t, dir, th.path)

	read := func() {
		var wg sync.WaitGroup
		for range 40 {
			wg.Go(func() {
				s, err := daemon.Snapshot(t.Context(), sock)
				if assert.NoError(t, err) {
					assert.True(t, hasHeader(s))
				}
			})
		}
		wg.Wait()
	}
	served(t, sock, hasHeader)
	calls(t, f, 4) // two repos, a PR list and a count each
	read()
	polled(t, th.srv, 3)
	read()
	assert.Len(t, f.Calls(t), 4, "reads and team polls do not ask GitHub")

	ticks <- time.Time{}
	calls(t, f, 8)
	read()
	polled(t, th.srv, 3)
	assert.Len(t, f.Calls(t), 8, "one more interval, one more round")
}

// TestGitHubKeepsTheLastGoodStateFiveIntervals: once gh starts failing, a
// project's last good PRs and count stay reported for five intervals of the
// configured length, then are not reported at all, never shown stale for
// longer. The failure is logged when it starts and the recovery when it
// comes, not every interval.
func TestGitHubKeepsTheLastGoodStateFiveIntervals(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	f := recordedGh(t)
	ticks := make(chan time.Time)
	c := &clock{now: t0}
	var logs lockedLog
	dir := shortDir(t)
	o := ghOpts(t, dir, th, ghConfig(t), f, ticks)
	const interval = 20 * time.Second
	o.Team.GitHub.Interval = interval
	o.Now, o.Logger = c.Now, zerolog.New(&logs)
	run(t, o)
	sock := socketPath(t, dir, th.path)
	fresh := func(s snapshot.Snapshot) bool { return hasHeader(s) && !s.At.Before(c.Now()) }

	served(t, sock, fresh)
	calls(t, f, 4)
	f.Fail(t, "HTTP 502: Bad Gateway (https://api.github.com/graphql)\n")
	for i := 1; i <= 5; i++ {
		c.advance(interval)
		ticks <- time.Time{}
		calls(t, f, 4+4*i)
		got := served(t, sock, func(s snapshot.Snapshot) bool { return !s.At.Before(c.Now()) })
		require.True(t, hasHeader(got), "kept %d intervals after the last good read", i)
		assert.Equal(t, recordedPRs(), got.Project.PRs)
		assert.Equal(t, new(14), got.Project.Issues)
	}
	assert.Equal(t, 4, strings.Count(logs.String(), "github: collection failed"), "logged once per repo and call, not every interval")
	assert.Contains(t, logs.String(), "HTTP 502: Bad Gateway")

	c.advance(time.Second)
	got := served(t, sock, func(s snapshot.Snapshot) bool { return !s.At.Before(c.Now()) })
	assert.Equal(t, &snapshot.Project{Color: "#7c3aed"}, got.Project, "past five intervals nothing is reported")

	f.Mode(t, "")
	c.advance(interval)
	ticks <- time.Time{}
	served(t, sock, fresh)
	assert.Eventually(t, func() bool { return strings.Count(logs.String(), "github: collecting again") == 4 }, ready, 5*time.Millisecond,
		"each repo and call logs its recovery once")
}

// TestGitHubHangDoesNotDelayTheTeam: a gh that hangs holds up neither the
// snapshot nor `$team`, and stopping the daemon kills it and its children.
func TestGitHubHangDoesNotDelayTheTeam(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.layout(fullTeam())
	f := ghtest.New(t)
	f.Mode(t, ghtest.ModeHang)
	dir := shortDir(t)
	r := run(t, ghOpts(t, dir, th, ghConfig(t), f, nil))

	var child int
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(f.Child())
		if err != nil {
			return false
		}
		child, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil
	}, ready, 5*time.Millisecond, "gh never started")
	got := served(t, socketPath(t, dir, th.path), anySnapshot)
	assert.True(t, noPRs(got), "nothing is reported while gh hangs")
	polled(t, th.srv, 2)
	require.NoError(t, syscall.Kill(child, 0), "gh is still hanging while the team is served")

	r.cancel()
	require.NoError(t, r.wait(t, ready))
	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
	}, ready, 10*time.Millisecond, "the daemon left gh's child running")
}

// TestGitHubNotCollected: with no repo to collect, gh is never run; with a
// repo but no gh, that is logged once and nothing is reported; a
// configuration that does not resolve collects nothing.
func TestGitHubNotCollected(t *testing.T) {
	t.Parallel()
	noRepo := func(t *testing.T) string { return projectConfig(t) }
	broken := func(t *testing.T) string {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n[projects.app]\ngithub = \"nope\"\n"), 0o600))
		return dir
	}
	tests := map[string]struct {
		config  func(t *testing.T) string
		noGh    bool
		wantLog string
	}{
		"no config":        {config: func(*testing.T) string { return "" }},
		"no repo":          {config: noRepo},
		"no gh":            {config: ghConfig, noGh: true, wantLog: "gh is not on the daemon's PATH"},
		"a broken config":  {config: broken, wantLog: "the header reports no PRs or issues"},
		"no gh, no repo":   {config: noRepo, noGh: true},
		"no gh, no config": {config: func(*testing.T) string { return "" }, noGh: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			th.layout(fullTeam())
			f := ghtest.New(t)
			var logs lockedLog
			dir := shortDir(t)
			o := ghOpts(t, dir, th, tc.config(t), f, nil)
			if tc.noGh {
				o.Team.GitHub.Gh = ""
			}
			o.Logger = zerolog.New(&logs)
			run(t, o)

			got := served(t, socketPath(t, dir, th.path), anySnapshot)
			polled(t, th.srv, 3)
			assert.True(t, noPRs(got))
			assert.Empty(t, f.Calls(t), "gh never ran")
			if tc.wantLog != "" {
				assert.Equal(t, 1, strings.Count(logs.String(), tc.wantLog), "logged once")
			} else {
				assert.NotContains(t, logs.String(), "github:")
			}
		})
	}
}

// TestEachTeamCarriesItsOwnHeader: with every team snapshotted (A24), each
// carries its own project's GitHub header (A29): the tabs team other gets
// acme/other's, beside the ◆ spaces' agentisan with its own and its colour.
func TestEachTeamCarriesItsOwnHeader(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, panes, agents := fullTeam()
	th.layout(append(ws, herdr.WorkspaceInfo{WorkspaceID: "w30", Label: "other"}),
		append(panes, tabPane("w30:p1", "w30", "w30:t1", "claude", "idle", nil)),
		append(agents, named("w30:p1", "o-coder-1")))
	th.mu.Lock()
	th.tabs = []map[string]any{tabOf("w30:t1", "w30", "o-coder-1")}
	th.mu.Unlock()
	f := recordedGh(t)
	dir := shortDir(t)
	run(t, ghOpts(t, dir, th, ghConfig(t), f, nil))
	sock := socketPath(t, dir, th.path)

	var other snapshot.Snapshot
	require.Eventually(t, func() bool {
		s, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{WorkspaceID: "w30"})
		other = s
		return err == nil && hasHeader(s)
	}, ready, 5*time.Millisecond, "the tabs team never carried its header: %+v", other.Project)
	assert.Equal(t, "other", other.Team)
	assert.Equal(t, &snapshot.Project{PRs: recordedPRs(), Issues: new(14)}, other.Project)
	assert.Contains(t, f.Args(t, "pr"), "--repo=acme/other")

	spaces := served(t, sock, hasHeader)
	assert.Equal(t, "agentisan", spaces.Team)
	assert.Equal(t, &snapshot.Project{Color: "#7c3aed", PRs: recordedPRs(), Issues: new(14)}, spaces.Project)
}

// TestEachTeamCarriesOnlyItsOwnHeader: only the tabs team's project has a
// repo, so a team given another project's header is caught: other carries
// acme/other's, and the ◆ spaces' agentisan, with no repo, carries none.
func TestEachTeamCarriesOnlyItsOwnHeader(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	ws, panes, agents := fullTeam()
	th.layout(append(ws, herdr.WorkspaceInfo{WorkspaceID: "w30", Label: "other"}),
		append(panes, tabPane("w30:p1", "w30", "w30:t1", "claude", "idle", nil)),
		append(agents, named("w30:p1", "o-coder-1")))
	th.mu.Lock()
	th.tabs = []map[string]any{tabOf("w30:t1", "w30", "o-coder-1")}
	th.mu.Unlock()
	cfg := t.TempDir()
	body := "schema_version = 1\n[spaces]\nproject = \"agentisan\"\n" +
		"[projects.agentisan]\nrepo = \"~/Development/agentisan\"\ncolor = \"#7c3aed\"\n" +
		"[projects.other]\nrepo = \"/elsewhere\"\nshort = \"o\"\ngithub = \"acme/other\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(cfg, settings.FileName), []byte(body), 0o600))
	f := recordedGh(t)
	dir := shortDir(t)
	run(t, ghOpts(t, dir, th, cfg, f, nil))
	sock := socketPath(t, dir, th.path)

	require.Eventually(t, func() bool {
		s, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{WorkspaceID: "w30"})
		return err == nil && hasHeader(s)
	}, ready, 5*time.Millisecond, "other never carried its own header")
	calls(t, f, 2)
	polled(t, th.srv, 3)
	spaces, err := daemon.SnapshotOf(t.Context(), sock, daemon.Selector{})
	require.NoError(t, err)
	assert.Equal(t, "agentisan", spaces.Team)
	assert.True(t, noPRs(spaces), "agentisan has no repo, so no header: %+v", spaces.Project)
	assert.Equal(t, &snapshot.Project{Color: "#7c3aed"}, spaces.Project)
}

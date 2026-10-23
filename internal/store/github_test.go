package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

const ghRepo = "abanna/herdr-agentisan"

func github(t *testing.T, s *store.Store, project string) store.GitHubState {
	t.Helper()
	st, err := s.GitHub(t.Context(), project, ghRepo)
	require.NoError(t, err)
	return st
}

// TestGitHubState: a repo is unseeded until a collection is recorded; then
// it holds its open PRs and the events told, keyed by project as well as
// repo, so two projects on one repo are each told. A resolved PR leaves its
// project's open set, and only its project's, and keeps its events.
func TestGitHubState(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	assert.Equal(t, store.GitHubState{Kinds: map[string]bool{}, Open: []int{}, Seen: map[store.GitHubEvent]bool{}}, github(t, s, "app"))

	passed := store.GitHubEvent{PR: 5, Kind: "ci_passed", Key: "1e0fb1b"}
	approved := store.GitHubEvent{PR: 3, Kind: "approved"}
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{
		Project: "app", Repo: ghRepo, Collected: true, Open: []int{5, 3}, Seen: []store.GitHubEvent{passed, approved},
	}, t1))
	assert.Equal(t, store.GitHubState{Seeded: true, Kinds: map[string]bool{}, Open: []int{3, 5}, Seen: map[store.GitHubEvent]bool{passed: true, approved: true}}, github(t, s, "app"))
	assert.False(t, github(t, s, "web").Seeded, "another project on the same repo")
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "web", Repo: ghRepo, Collected: true, Open: []int{3}}, t1))

	merged := store.GitHubEvent{PR: 3, Kind: "merged", Key: "e5f63b8"}
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "app", Repo: ghRepo, Seen: []store.GitHubEvent{merged}, Resolved: []int{3}}, t1))
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "app", Repo: ghRepo, Seen: []store.GitHubEvent{merged}}, t1), "recorded twice")
	got := github(t, s, "app")
	assert.Equal(t, []int{5}, got.Open)
	assert.Equal(t, map[store.GitHubEvent]bool{passed: true, approved: true, merged: true}, got.Seen)
	assert.Equal(t, store.GitHubState{Seeded: true, Kinds: map[string]bool{}, Open: []int{3}, Seen: map[store.GitHubEvent]bool{}}, github(t, s, "web"),
		"app's merge leaves web's PR open until web looks it up itself")
}

// TestGitHubSeedKinds: a kind's own seed mark is the repo's and project's,
// and once written stays; it is not the repo's seed, which a kind added
// later finds already written.
func TestGitHubSeedKinds(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "app", Repo: ghRepo, Collected: true}, t1))
	got := github(t, s, "app")
	assert.True(t, got.Seeded)
	assert.Empty(t, got.Kinds, "seeded before any kind was")

	review := store.GitHubEvent{PR: 4, Kind: "review", Key: "alice 2026-10-22T23:20:14Z"}
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{
		Project: "app", Repo: ghRepo, Collected: true, SeedKinds: []string{"review"}, Seen: []store.GitHubEvent{review},
	}, t1))
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "app", Repo: ghRepo, Collected: true, SeedKinds: []string{"review"}}, t1), "marked twice")
	assert.Equal(t, store.GitHubState{Seeded: true, Kinds: map[string]bool{"review": true}, Open: []int{}, Seen: map[store.GitHubEvent]bool{review: true}}, github(t, s, "app"))
	assert.Empty(t, github(t, s, "web").Kinds, "another project on the same repo")
	other, err := s.GitHub(t.Context(), "app", "acme/other")
	require.NoError(t, err)
	assert.Empty(t, other.Kinds, "another repo of the same project")
}

// TestUpdateGitHubIsOneTransaction: an update that fails part way writes
// nothing, so an event is never recorded as told without its PR resolved.
func TestUpdateGitHubIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "app", Repo: ghRepo, Collected: true, Open: []int{3}}, t1))
	_, err := store.DB(s).ExecContext(t.Context(),
		"CREATE TEMP TRIGGER refuse_resolve BEFORE DELETE ON github_open BEGIN SELECT RAISE(ABORT, 'refused'); END")
	require.NoError(t, err)
	err = s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "app", Repo: ghRepo, Seen: []store.GitHubEvent{{PR: 3, Kind: "merged", Key: "e5f63b8"}}, Resolved: []int{3}}, t1)
	require.ErrorIs(t, err, store.ErrStore)
	got := github(t, s, "app")
	assert.Empty(t, got.Seen, "rolled back whole")
	assert.Equal(t, []int{3}, got.Open)
}

// TestUpdateGitHubRefuses what the schema would, before writing anything.
func TestUpdateGitHubRefuses(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ok := store.GitHubUpdate{Project: "app", Repo: ghRepo, Collected: true}
	for name, u := range map[string]store.GitHubUpdate{
		"no project":        {Repo: ghRepo, Collected: true},
		"no repo":           {Project: "app", Collected: true},
		"an open PR 0":      {Project: "app", Repo: ghRepo, Collected: true, Open: []int{4, 0}},
		"a resolved PR -1":  {Project: "app", Repo: ghRepo, Collected: true, Resolved: []int{-1}},
		"an event's PR 0":   {Project: "app", Repo: ghRepo, Collected: true, Seen: []store.GitHubEvent{{Kind: "dirty", Key: "k"}}},
		"an event, no kind": {Project: "app", Repo: ghRepo, Collected: true, Seen: []store.GitHubEvent{{PR: 4}}},
		"a seed, no kind":   {Project: "app", Repo: ghRepo, Collected: true, SeedKinds: []string{"review", ""}},
	} {
		require.ErrorIs(t, s.UpdateGitHub(t.Context(), u, t1), store.ErrInvalid, name)
	}
	require.ErrorIs(t, s.UpdateGitHub(t.Context(), ok, time.Time{}), store.ErrInvalid, "the zero time")
	assert.False(t, github(t, s, "app").Seeded, "nothing was written")
	_, err := store.DB(s).ExecContext(t.Context(), "INSERT INTO github_seen (project, repo, pr, kind, key, first_seen, last_seen) VALUES ('app', 'r', 1, '', '', 0, 0)")
	require.Error(t, err, "the schema refuses a kind-less event too")
	_, err = store.DB(s).ExecContext(t.Context(), "INSERT INTO github_seeded_kinds (project, repo, kind, first_seen, last_seen) VALUES ('app', 'r', '', 0, 0)")
	require.Error(t, err, "and a kind-less seed")
}

// TestPruneGitHub: every row goes once the collector has not observed it for
// 30 days, and stays at exactly 30. Observing an open PR again keeps its
// events, and a collection keeps the repo's seed marks, its kinds' too; a
// clock that steps back moves no row back.
func TestPruneGitHub(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC)
	edge := now.Add(-store.GitHubRetention)
	s := open(t, dbPath(t))
	write := func(project string, at time.Time, u store.GitHubUpdate) {
		u.Project, u.Repo = project, ghRepo
		require.NoError(t, s.UpdateGitHub(t.Context(), u, at))
	}
	ev := func(pr int) []store.GitHubEvent { return []store.GitHubEvent{{PR: pr, Kind: "ci_passed", Key: "h"}} }
	review := []string{"review"}
	write("old", edge.Add(-time.Nanosecond), store.GitHubUpdate{Collected: true, SeedKinds: review, Open: []int{1}, Seen: ev(1)})
	write("edge", edge, store.GitHubUpdate{Collected: true, SeedKinds: review, Open: []int{1}, Seen: ev(1)})
	write("kept", edge.Add(-time.Hour), store.GitHubUpdate{Collected: true, SeedKinds: review, Open: []int{1}, Seen: slices.Concat(ev(1), ev(2))})
	write("kept", now, store.GitHubUpdate{Collected: true, Open: []int{1}})
	write("kept", edge.Add(-2*time.Hour), store.GitHubUpdate{Collected: true, Open: []int{1}, Seen: ev(1)})

	n, err := s.PruneGitHub(t.Context(), now)
	require.NoError(t, err)
	assert.EqualValues(t, 5, n, "old's two marks, open row and event, and kept's event of the PR no longer open")
	assert.Equal(t, store.GitHubState{Kinds: map[string]bool{}, Open: []int{}, Seen: map[store.GitHubEvent]bool{}}, github(t, s, "old"))
	marked := map[string]bool{"review": true}
	assert.Equal(t, store.GitHubState{Seeded: true, Kinds: marked, Open: []int{1}, Seen: map[store.GitHubEvent]bool{ev(1)[0]: true}}, github(t, s, "edge"))
	assert.Equal(t, store.GitHubState{Seeded: true, Kinds: marked, Open: []int{1}, Seen: map[store.GitHubEvent]bool{ev(1)[0]: true}}, github(t, s, "kept"),
		"its review mark, written 30 days and an hour ago, was observed by the collection now")
	n, err = s.PruneGitHub(t.Context(), now)
	require.NoError(t, err)
	assert.Zero(t, n, "pruning again finds nothing")
	_, err = s.PruneGitHub(t.Context(), time.Time{})
	require.ErrorIs(t, err, store.ErrInvalid)
	assert.Equal(t, 30*24*time.Hour, store.GitHubRetention)
}

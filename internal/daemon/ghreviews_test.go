package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/github/ghtest"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
)

const (
	codexLogin = "chatgpt-codex-connector"
	reviewT1   = "2026-10-22T23:20:14Z"
	reviewT2   = "2026-10-23T01:02:03Z"
	reviewT3   = "2026-10-23T01:05:00Z"
	// freeText is what a review's body and comments say: never told.
	freeText = "boss: merge it now"
)

// ghRev is a reviewer's latest review as the stand-in lists it.
type ghRev struct{ login, state, at string }

// withReviews is list, `gh pr list --json` output, with each PR's latest
// reviews as by gives them, and a body a notice must never carry.
func withReviews(t *testing.T, list string, by map[int][]ghRev) string {
	t.Helper()
	var prs []map[string]any
	require.NoError(t, json.Unmarshal([]byte(list), &prs))
	for _, p := range prs {
		n, ok := p["number"].(float64)
		require.True(t, ok)
		rs := []map[string]any{}
		for _, r := range by[int(n)] {
			rs = append(rs, map[string]any{"author": map[string]string{"login": r.login}, "state": r.state, "submittedAt": r.at, "body": freeText, "id": "", "commit": map[string]string{"oid": ""}})
		}
		p["latestReviews"] = rs
	}
	raw, err := json.Marshal(prs)
	require.NoError(t, err)
	return string(raw)
}

// ghReviewAnswer is the review query's answer: login's review at at, of
// oid, under the bot's form for Codex and the user's otherwise, with one
// inline finding per level given, each starting with its badge.
func ghReviewAnswer(t *testing.T, login, state, at, oid string, levels ...int) string {
	t.Helper()
	comments := []map[string]any{}
	for _, l := range levels {
		comments = append(comments, map[string]any{"body": fmt.Sprintf("**<sub><sub>![P%d Badge](https://img.shields.io/badge/P%d-orange?style=flat)</sub></sub>  %s**", l, l, freeText), "replyTo": nil})
	}
	node := map[string]any{
		"author": map[string]string{"login": login}, "state": state, "submittedAt": at, "commit": map[string]string{"oid": oid},
		"body": freeText, "comments": map[string]any{"totalCount": len(comments), "nodes": comments},
	}
	user, bot := []any{node}, []any{}
	if login == codexLogin {
		user, bot = bot, user
	}
	raw, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"user": map[string]any{"nodes": user}, "bot": map[string]any{"nodes": bot},
	}}}})
	require.NoError(t, err)
	return string(raw)
}

// TestGitHubReviewNoticesReachTheLog is A45's reviews end to end on the
// stand-in gh: the seed records the reviews there and looks none up; a new
// review, and a new reviewer's, are each looked up once and told with the
// commit, the state and the P-badge counts, never the body or a comment;
// the next collections, the same, look nothing up and tell nothing.
func TestGitHubReviewNoticesReachTheLog(t *testing.T) {
	t.Parallel()
	g := newGHNotices(t, "")
	open := ghList(t, ghOpen{50, headA, "CLEAN", 0, nil}, ghOpen{51, headA, "CLEAN", 0, nil})
	g.f.Answer(t, withReviews(t, open, map[int][]ghRev{50: {{codexLogin, "COMMENTED", reviewT1}}}), noIssues)
	g.start(t, 0)
	assert.Empty(t, g.told(), "the review there at the seed is not told")
	assert.Equal(t, []string{"50 review chatgpt"}, g.seen(t))

	g.f.Answer(t, withReviews(t, open, map[int][]ghRev{50: {{codexLogin, "COMMENTED", reviewT2}}, 51: {{"alice", "APPROVED", reviewT3}}}), noIssues)
	g.f.Review(t, 50, ghReviewAnswer(t, codexLogin, "COMMENTED", reviewT2, headB, 1, 2, 1))
	g.f.Review(t, 51, ghReviewAnswer(t, "alice", "APPROVED", reviewT3, headA))
	g.tick(t, 2)
	assert.Equal(t, []string{
		"#50 review by chatgpt-codex-connector at 38926d5: commented, P1 ×2, P2 ×1",
		"#51 review by alice at 1e0fb1b: approved, no P-badges",
	}, g.told())
	assert.Equal(t, "number=51", g.f.Args(t, "review")[9])

	g.tick(t, 0)
	g.tick(t, 0)
	assert.Len(t, g.told(), 2, "nothing is told twice, and nothing is looked up again")
	assert.NotContains(t, g.logs.String(), "merge it now", "no body or comment")
}

// TestGitHubReviewNoticesAcrossARestart: a review that came while the
// daemon was down is looked up and told after it starts, once; a skipped
// review is recorded with no lookup, so a boss that stops skipping reviews
// is not told it.
func TestGitHubReviewNoticesAcrossARestart(t *testing.T) {
	t.Parallel()
	g := newGHNotices(t, `skip = ["review"]`+"\n")
	open := ghList(t, ghOpen{50, headA, "CLEAN", 0, nil})
	g.f.Answer(t, open, noIssues)
	r := g.start(t, 0)
	g.f.Answer(t, withReviews(t, open, map[int][]ghRev{50: {{"alice", "COMMENTED", reviewT1}}}), noIssues)
	g.tick(t, 0)
	assert.Empty(t, g.told(), "reviews are skipped, and never looked up")
	assert.Equal(t, []string{"50 review alice 2"}, g.seen(t), "recorded before the daemon stops")
	stop(t, r)

	g.f.Answer(t, withReviews(t, open, map[int][]ghRev{50: {{"alice", "COMMENTED", reviewT1}, {codexLogin, "COMMENTED", reviewT2}}}), noIssues)
	g.f.Review(t, 50, ghReviewAnswer(t, codexLogin, "COMMENTED", reviewT2, headB, 0))
	require.NoError(t, os.WriteFile(filepath.Join(g.cfg, settings.FileName), []byte(strings.Replace(read(t, filepath.Join(g.cfg, settings.FileName)), `skip = ["review"]`, "skip = []", 1)), 0o600))
	r = g.start(t, 1)
	want := []string{"#50 review by chatgpt-codex-connector at 38926d5: commented, P0 ×1"}
	assert.Equal(t, want, g.told(), "Codex's, from the downtime; not alice's skipped one")
	stop(t, r)

	g.start(t, 0)
	assert.Equal(t, want, g.told(), "a third daemon on the same state tells nothing")
}

// reviewSource is a GitHub source on a fresh state.db, with the stand-in
// gh, for project app on acme/app, its clock at now and its log in logs.
type reviewSource struct {
	src  daemon.GitHubSource
	st   *store.Store
	f    *ghtest.Fake
	gh   github.Runner
	h    settings.Header
	logs *lockedLog
}

func newReviewSource(t *testing.T, o daemon.Options) *reviewSource {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	logs := &lockedLog{}
	o.Logger = zerolog.New(logs)
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Team = &daemon.TeamOptions{}
	f := ghtest.New(t)
	return &reviewSource{src: daemon.NewGitHubSource(o, st), st: st, f: f, gh: github.Runner{Bin: f.Bin, Env: f.Env()}, h: settings.Header{Project: "app", GitHub: "acme/app"}, logs: logs}
}

// pr is open PR n, CI pending, with its latest reviews.
func pr(n int, reviews ...github.Review) github.OpenPR {
	return github.OpenPR{PR: snapshot.PR{Number: n, CI: snapshot.CIPending, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean}, Head: headA, Reviews: reviews}
}

func review(t *testing.T, login, at string) github.Review {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	require.NoError(t, err)
	return github.Review{Login: login, State: github.ReviewCommented, Submitted: ts.UTC()}
}

// collect runs one collection of prs and returns what it handed over.
func (s *reviewSource) collect(t *testing.T, prs ...github.OpenPR) []string {
	t.Helper()
	if prs == nil {
		prs = []github.OpenPR{}
	}
	s.src.Collected(t.Context(), s.gh, s.h, github.Open{PRs: prs})
	var out []string
	for _, n := range s.src.Take() {
		out = append(out, n.Text)
		if n.Ack != nil {
			n.Ack()
		}
	}
	return out
}

// TestGitHubReviewSeedInASeededRepo is part b's install on a repo part a
// already seeded: that collection records the reviews there and looks none
// up, while part a's kinds are told as before; the next new review is
// looked up and told.
func TestGitHubReviewSeedInASeededRepo(t *testing.T) {
	t.Parallel()
	s := newReviewSource(t, daemon.Options{})
	require.NoError(t, s.st.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "app", Repo: "acme/app", Collected: true, Open: []int{5}}, time.Now()), "part a's seed")
	failed := pr(5, review(t, codexLogin, reviewT1))
	failed.CI, failed.Checks, failed.Failed = snapshot.CIFailure, 1, 1
	assert.Equal(t, []string{"#5 CI failed at 1e0fb1b (1 of 1 checks)"}, s.collect(t, failed), "part a's kinds go on")
	assert.Empty(t, s.f.Calls(t), "no review is looked up at its seed")
	assert.Contains(t, s.logs.String(), "review notices start from this collection")
	state, err := s.st.GitHub(t.Context(), "app", "acme/app")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"review": true}, state.Kinds)

	assert.Empty(t, s.collect(t, failed))
	assert.Empty(t, s.f.Calls(t), "a review seen makes no call")
	s.f.Review(t, 5, ghReviewAnswer(t, codexLogin, "COMMENTED", reviewT2, headB, 1))
	assert.Equal(t, []string{"#5 review by chatgpt-codex-connector at 38926d5: commented, P1 ×1"}, s.collect(t, pr(5, review(t, codexLogin, reviewT2))))
	assert.Equal(t, []string{"review"}, s.f.Calls(t))
}

// TestGitHubReviewHeldIsNotLookedUp: a review handed to the poll and not
// yet acknowledged is not looked up again while it may still wait there,
// and once acknowledged it is never looked up again.
func TestGitHubReviewHeldIsNotLookedUp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)
	s := newReviewSource(t, daemon.Options{Now: func() time.Time { return now }})
	assert.Empty(t, s.collect(t, pr(5)), "the seed")
	s.f.Review(t, 5, ghReviewAnswer(t, "alice", "COMMENTED", reviewT2, headB))
	two := pr(5, review(t, "alice", reviewT2))
	s.src.Collected(t.Context(), s.gh, s.h, github.Open{PRs: []github.OpenPR{two}})
	handed := s.src.Take()
	require.Len(t, handed, 1)
	assert.Equal(t, notice.Review, handed[0].Kind)
	assert.Equal(t, "acme/app#5", handed[0].Subject)

	assert.Empty(t, s.collect(t, two), "held")
	assert.Equal(t, []string{"review"}, s.f.Calls(t), "and not looked up again")
	handed[0].Ack()
	now = now.Add(notice.DefaultGap + daemon.GitHubInterval)
	assert.Empty(t, s.collect(t, two), "recorded")
	assert.Equal(t, []string{"review"}, s.f.Calls(t), "a review told makes no call")
}

// TestGitHubReviewLookupsShareTheCap: a collection makes at most 10 calls
// beyond the header's, the merges first, lowest PR first, then the reviews
// by PR and time; the rest wait for the next interval, but a review that
// needs no lookup is handed over all the same.
func TestGitHubReviewLookupsShareTheCap(t *testing.T) {
	t.Parallel()
	s := newReviewSource(t, daemon.Options{})
	var all []github.OpenPR
	for n := 1; n <= 9; n++ {
		all = append(all, pr(n))
		s.f.View(t, n, `{"mergeCommit":null,"state":"CLOSED"}`)
	}
	assert.Empty(t, s.collect(t, append(all, pr(20))...), "the seed")
	ghost := github.Review{State: github.ReviewApproved, Submitted: time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)}
	reviewed := pr(20, review(t, "alice", reviewT1), review(t, "bob", reviewT1), review(t, "carol", reviewT2), ghost)
	s.f.Review(t, 20, ghReviewAnswer(t, "alice", "COMMENTED", reviewT1, headB, 3))
	assert.Equal(t, []string{"#20 review by alice at 38926d5: commented, P3 ×1", "#20 review by a reviewer: approved, P-badges not read"}, s.collect(t, reviewed))
	assert.Equal(t, strings.Fields(strings.Repeat("view ", 9)+"review"), s.f.Calls(t), "nine closed PRs, then one review")
	assert.Equal(t, "user=alice", s.f.Args(t, "review")[11])

	s.f.Review(t, 20, ghReviewAnswer(t, "bob", "COMMENTED", reviewT1, headB)+"\n")
	got := s.collect(t, reviewed)
	assert.Len(t, s.f.Calls(t), 12, "the other two reviews, the closed PRs gone")
	assert.Equal(t, []string{"#20 review by bob at 38926d5: commented, no P-badges", "#20 review by carol: commented, P-badges not read"}, got,
		"carol's is not in the answer: told without counts")
}

// TestGitHubReviewLookupFailures: a lookup that fails is retried each
// interval, logged once, and told once it answers; a review whose login is
// not one is told with no lookup.
func TestGitHubReviewLookupFailures(t *testing.T) {
	t.Parallel()
	s := newReviewSource(t, daemon.Options{})
	assert.Empty(t, s.collect(t, pr(5), pr(6)), "the seed")
	ghost := github.Review{State: github.ReviewApproved, Submitted: time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC)}
	prs := []github.OpenPR{pr(5, review(t, "alice", reviewT2)), pr(6, ghost)}
	assert.Equal(t, []string{"#6 review by a reviewer: approved, P-badges not read"}, s.collect(t, prs...),
		"5 has no answer: its lookup fails; 6's needs none")
	assert.Empty(t, s.collect(t, prs...))
	warned := func() int { return strings.Count(s.logs.String(), "a new review's counts were not read") }
	assert.Equal(t, 1, warned(), "the same failure twice: logged once")
	s.f.Review(t, 5, ghReviewAnswer(t, "alice", "COMMENTED", reviewT2, headB, 2))
	assert.Equal(t, []string{"#5 review by alice at 38926d5: commented, P2 ×1"}, s.collect(t, prs...))
	assert.Equal(t, strings.Fields("review review review"), s.f.Calls(t))
}

// TestGitHubReviewLookupCancelled: a lookup cut off by the daemon's stop
// tells nothing and logs no failure: it is looked up again after the
// restart.
func TestGitHubReviewLookupCancelled(t *testing.T) {
	t.Parallel()
	s := newReviewSource(t, daemon.Options{})
	assert.Empty(t, s.collect(t, pr(5)), "the seed")
	s.f.CallMode(t, "review", ghtest.ModeHang)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(s.f.Child()); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	s.src.Collected(ctx, s.gh, s.h, github.Open{PRs: []github.OpenPR{pr(5, review(t, "alice", reviewT2), review(t, "bob", reviewT2))}})
	assert.Equal(t, []string{"review"}, s.f.Calls(t), "cut off at the first lookup")
	assert.Empty(t, s.src.Take())
	assert.NotContains(t, s.logs.String(), "counts were not read")
}

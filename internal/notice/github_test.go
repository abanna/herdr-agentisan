package notice_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

const (
	headA = "1e0fb1b9c2a54e1f9d3b7a6c5e4d3c2b1a0f9e8d"
	headB = "38926d5fcdb4c034c40465b3fcf138dd04a00b39"
)

func openPR(n int, head string, ci snapshot.CIState, codex snapshot.CodexState, merge snapshot.MergeState) github.OpenPR {
	return github.OpenPR{PR: snapshot.PR{Number: n, CI: ci, Codex: codex, Merge: merge}, Head: head}
}

// TestGitHubEvents: CI failed or passed and a merge conflict are keyed by
// the head, a 👍 by the PR alone (ADR-001 A45). Pending or unknown CI and a
// clean merge tell nothing; a PR with no head oid tells only its 👍.
func TestGitHubEvents(t *testing.T) {
	t.Parallel()
	failed := openPR(50, headA, snapshot.CIFailure, snapshot.CodexPending, snapshot.MergeDirty)
	failed.Checks, failed.Failed = 7, 2
	got := notice.GitHubEvents([]github.OpenPR{
		failed,
		openPR(51, headB, snapshot.CISuccess, snapshot.CodexApproved, snapshot.MergeClean),
		openPR(52, headB, snapshot.CIPending, snapshot.CodexPending, snapshot.MergeBlocked),
		openPR(53, headB, snapshot.CIUnknown, snapshot.CodexPending, snapshot.MergeUnknown),
		openPR(54, "", snapshot.CIFailure, snapshot.CodexApproved, snapshot.MergeDirty),
	})
	assert.Equal(t, []notice.GitHubEvent{
		{PR: 50, Kind: notice.CIFailed, Key: headA, Text: "#50 CI failed at 1e0fb1b (2 of 7 checks)"},
		{PR: 50, Kind: notice.Dirty, Key: headA, Text: "#50 has a merge conflict at 1e0fb1b"},
		{PR: 51, Kind: notice.CIPassed, Key: headB, Text: "#51 CI passed at 38926d5"},
		{PR: 51, Kind: notice.Approved, Text: "#51 got a 👍"},
		{PR: 54, Kind: notice.Approved, Text: "#54 got a 👍"},
	}, got)
	assert.Empty(t, notice.GitHubEvents(nil))
}

// TestGitHubMerged names the repo, from the configuration, and the merge
// commit when GitHub named one.
func TestGitHubMerged(t *testing.T) {
	t.Parallel()
	assert.Equal(t, notice.GitHubEvent{PR: 50, Kind: notice.Merged, Key: headA, Text: "herdr-agentisan #50 merged as 1e0fb1b"},
		notice.GitHubMerged("abanna/herdr-agentisan", 50, headA))
	assert.Equal(t, notice.GitHubEvent{PR: 7, Kind: notice.Merged, Text: "other #7 merged"}, notice.GitHubMerged("acme/other", 7, ""))
}

// TestBatcherTellsGitHubAgain: a PR's CI failing at one head and then at the
// next, inside the repeat window, reaches the boss both times, and so does
// each other GitHub kind: the seen-set, not the window, tells each once. A
// window that dropped the second would also acknowledge it, recording it
// told when it never was.
func TestBatcherTellsGitHubAgain(t *testing.T) {
	t.Parallel()
	b := notice.NewBatcher(notice.Configs{Default: notice.Config{Repeat: time.Hour}})
	var told []string
	for i, k := range []notice.Kind{notice.CIFailed, notice.CIFailed, notice.CIPassed, notice.CIPassed, notice.Dirty, notice.Dirty, notice.Approved, notice.Merged, notice.Review, notice.Review} {
		b.Add(t0.Add(time.Duration(i)*time.Second), []notice.Notice{note("ha", "acme/app#50", k, string(k))})
		for _, m := range b.Flush(t0.Add(time.Duration(i) * time.Second)) {
			told = append(told, m.Lines...)
		}
	}
	assert.Equal(t, "ci_failed ci_failed ci_passed ci_passed dirty dirty approved merged review review", strings.Join(told, " "))
}

// TestGitHubReviews: each PR's reviews in the list's order, keyed by login
// and the time in UTC, with the line told when its counts cannot be read:
// no commit, no counts, and "a reviewer" for a login that is not one.
func TestGitHubReviews(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 22, 23, 20, 14, 0, time.UTC)
	codex := github.Review{Login: "chatgpt-codex-connector", State: github.ReviewCommented, Submitted: at}
	ghost := github.Review{State: github.ReviewApproved, Submitted: at.Add(time.Minute)}
	odd := github.Review{Login: "alice", Submitted: at.Add(500 * time.Millisecond).In(time.FixedZone("x", 3600))}
	a := openPR(50, headA, snapshot.CIPending, snapshot.CodexPending, snapshot.MergeClean)
	a.Reviews = []github.Review{codex, ghost}
	b := openPR(51, "", snapshot.CIPending, snapshot.CodexPending, snapshot.MergeClean)
	b.Reviews = []github.Review{odd}
	got := notice.GitHubReviews([]github.OpenPR{a, b, openPR(52, headA, snapshot.CIPending, snapshot.CodexPending, snapshot.MergeClean)})
	assert.Equal(t, []notice.GitHubReview{
		{GitHubEvent: notice.GitHubEvent{PR: 50, Kind: notice.Review, Key: "chatgpt-codex-connector 2026-10-22T23:20:14Z", Text: "#50 review by chatgpt-codex-connector: commented, P-badges not read"}, Review: codex},
		{GitHubEvent: notice.GitHubEvent{PR: 50, Kind: notice.Review, Key: " 2026-10-22T23:21:14Z", Text: "#50 review by a reviewer: approved, P-badges not read"}, Review: ghost},
		{GitHubEvent: notice.GitHubEvent{PR: 51, Kind: notice.Review, Key: "alice 2026-10-22T23:20:14.5Z", Text: "#51 review by alice: reviewed, P-badges not read"}, Review: odd},
	}, got, "a PR with no head still tells its reviews; a time in another zone is keyed in UTC")
	assert.Empty(t, notice.GitHubReviews(nil))
}

// TestGitHubReviewed: the line once the counts are read: the commit
// reviewed, the state as words, and each level's count, P0 first and none
// at zero; comments past those read say so. The key stays the list's.
func TestGitHubReviewed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 22, 23, 20, 14, 0, time.UTC)
	r := notice.GitHubReview{GitHubEvent: notice.GitHubEvent{PR: 928, Kind: notice.Review, Key: "k", Text: "unread"}, Review: github.Review{Login: "alice", Submitted: at}}
	line := func(state, commit string, badges [4]int, comments, read int) string {
		d := github.ReviewDetail{Review: github.Review{Login: "alice", State: state, Submitted: at}, Commit: commit, Badges: badges, Comments: comments, Read: read}
		e := notice.GitHubReviewed(r, d)
		assert.Equal(t, notice.GitHubEvent{PR: 928, Kind: notice.Review, Key: "k", Text: e.Text}, e)
		return e.Text
	}
	assert.Equal(t, "#928 review by alice at 1e0fb1b: commented, P1 ×2", line(github.ReviewCommented, headA, [4]int{0, 2, 0, 0}, 2, 2))
	assert.Equal(t, "#928 review by alice at 1e0fb1b: changes requested, P0 ×1, P2 ×3, P3 ×4", line(github.ReviewChangesRequested, headA, [4]int{1, 0, 3, 4}, 7, 7))
	assert.Equal(t, "#928 review by alice at 38926d5: approved, no P-badges", line(github.ReviewApproved, headB, [4]int{}, 0, 0))
	assert.Equal(t, "#928 review by alice: dismissed, no P-badges", line(github.ReviewDismissed, "", [4]int{}, 0, 0))
	assert.Equal(t, "#928 review by alice at 1e0fb1b: reviewed, P1 ×97, P2 ×3 (first 100 of 140 comments)", line("", headA, [4]int{0, 97, 3, 0}, 140, 100))
	assert.Equal(t, "#928 review by alice at 1e0fb1b: commented, no P-badges (first 100 of 101 comments)", line(github.ReviewCommented, headA, [4]int{}, 101, 100))
	ghost := r
	ghost.Review.Login = ""
	assert.Equal(t, "#928 review by a reviewer at 1e0fb1b: approved, no P-badges",
		notice.GitHubReviewed(ghost, github.ReviewDetail{Review: github.Review{State: github.ReviewApproved}, Commit: headA}).Text)
}

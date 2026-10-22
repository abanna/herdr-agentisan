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
	for i, k := range []notice.Kind{notice.CIFailed, notice.CIFailed, notice.CIPassed, notice.CIPassed, notice.Dirty, notice.Dirty, notice.Approved, notice.Merged} {
		b.Add(t0.Add(time.Duration(i)*time.Second), []notice.Notice{note("ha", "acme/app#50", k, string(k))})
		for _, m := range b.Flush(t0.Add(time.Duration(i) * time.Second)) {
			told = append(told, m.Lines...)
		}
	}
	assert.Equal(t, "ci_failed ci_failed ci_passed ci_passed dirty dirty approved merged", strings.Join(told, " "))
}

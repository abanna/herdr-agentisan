package store_test

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// daemonEnvelope is e as the daemon would build it, from the notice it hands
// the poll (internal/daemon/ghnotices.go): the subject is repo#pr, the worker
// is "", and the receipt is the seen-set key's, at version 1.
func daemonEnvelope(project, repo string, e notice.GitHubEvent) store.NoticeEnvelope {
	return store.NoticeEnvelope{
		Version: 1, Project: project, Source: "github", Kind: string(e.Kind),
		Subject: repo + "#" + strconv.Itoa(e.PR), ObservedAt: time.Date(2026, 11, 16, 0, 0, 0, 1, time.UTC), Text: e.Text,
		Receipt: &store.NoticeReceipt{Version: 1, Repo: repo, PR: int64(e.PR), Kind: string(e.Kind), Key: e.Key},
	}
}

// githubEventsOf is every event the notice constructors make of prs, merged
// as each commit, and reviewed at each commit.
func githubEventsOf(repo string, prs []gh.OpenPR, commits []string) []notice.GitHubEvent {
	out := notice.GitHubEvents(prs)
	for _, r := range notice.GitHubReviews(prs) {
		out = append(out, r.GitHubEvent)
		for _, c := range commits {
			out = append(out, notice.GitHubReviewed(r, gh.ReviewDetail{Review: r.Review, Commit: c, Badges: [4]int{math.MaxInt, math.MaxInt, math.MaxInt, math.MaxInt}, Comments: math.MaxInt}))
		}
	}
	for _, p := range prs {
		for _, c := range commits {
			out = append(out, notice.GitHubMerged(repo, p.Number, c))
		}
	}
	return out
}

// C1: every event a real GitHub repository can produce builds a valid
// envelope. The fixtures go to the largest values GitHub itself produces: a
// 39-character owner and a 100-character repo name, PR 2^31-1 (GraphQL's Int),
// SHA-1 and SHA-256 oids, a 39-character login and a deleted account's "",
// every CI, merge and review state, and counts at MaxInt; the project is at
// the envelope's own 256 bytes. Settings and the gh parse bound these to the
// envelope (NERD-5632, TestNoticeBoundsAreTheEnvelopes), and
// TestNoticeEventsPastGitHubsBoundsAreRefused pins what lies past them.
func TestNoticeEventsFitTheEnvelope(t *testing.T) {
	sha1, sha256 := strings.Repeat("0123456789", 4), strings.Repeat("abcdef0123456789", 4)
	commits := []string{"", sha1, sha256}
	var reviews []gh.Review
	for _, login := range []string{"", "a", "A" + strings.Repeat("b-", 19)} {
		for _, state := range []string{gh.ReviewApproved, gh.ReviewChangesRequested, gh.ReviewCommented, gh.ReviewDismissed, ""} {
			for _, at := range []time.Time{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 23, 0, 0, 0, 123456789, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)} {
				reviews = append(reviews, gh.Review{Login: login, State: state, Submitted: at})
			}
		}
	}
	var prs []gh.OpenPR
	for _, n := range []int{1, math.MaxInt32} {
		for _, head := range commits {
			for _, ci := range []snapshot.CIState{snapshot.CIFailure, snapshot.CISuccess, snapshot.CIPending, snapshot.CIUnknown} {
				for _, merge := range []snapshot.MergeState{snapshot.MergeDirty, snapshot.MergeClean} {
					p := gh.OpenPR{
						PR:   snapshot.PR{Number: n, CI: ci, Codex: snapshot.CodexApproved, Merge: merge},
						Head: head, Checks: math.MaxInt, Failed: math.MaxInt,
					}
					if head == sha256 && ci == snapshot.CIFailure && merge == snapshot.MergeDirty {
						p.Reviews = reviews // once per PR number: a review's line names only the number
					}
					prs = append(prs, p)
				}
			}
		}
	}
	kinds, deleted := map[notice.Kind]int{}, 0
	for _, repo := range []string{"a/b", "A" + strings.Repeat("b-", 19) + "/" + strings.Repeat("n._-", 25)} {
		for _, project := range []string{"", "p", strings.Repeat("p", 256)} {
			for _, e := range githubEventsOf(repo, prs, commits) {
				envelope := daemonEnvelope(project, repo, e)
				id, err := store.GitHubNoticeCaptureID(envelope)
				require.NoError(t, err, "%s %q: %q", e.Kind, e.Key, e.Text)
				require.Len(t, id, 68)
				kinds[e.Kind]++
				if e.Kind == notice.Review && strings.HasPrefix(e.Key, " ") {
					deleted++
				}
			}
		}
	}
	// No kind is vacuous: each of the six is made and checked.
	assert.Len(t, kinds, 6)
	for _, k := range []notice.Kind{notice.Merged, notice.CIFailed, notice.CIPassed, notice.Dirty, notice.Approved, notice.Review} {
		assert.Positive(t, kinds[k], k)
	}
	assert.Positive(t, deleted, "a deleted account's review is checked")
	quiet := gh.OpenPR{PR: snapshot.PR{Number: 1, CI: snapshot.CIPending, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean}, Head: sha1}
	assert.Empty(t, notice.GitHubEvents([]gh.OpenPR{quiet}), "pending CI tells nothing")
	quiet.CI = snapshot.CIUnknown
	assert.Empty(t, notice.GitHubEvents([]gh.OpenPR{quiet}), "unknown CI tells nothing")
}

// The other side of C1's line: inputs real GitHub cannot produce, which
// settings and the gh parse now refuse too (NERD-5632), build an envelope
// version 1 refuses, whole, with no truncation and the input unchanged, so
// admission fails closed. One case for each: a repo, a project, a merged line
// and a PR number.
func TestNoticeEventsPastGitHubsBoundsAreRefused(t *testing.T) {
	sha1 := strings.Repeat("0123456789", 4)
	failed := func(n int) []notice.GitHubEvent {
		return notice.GitHubEvents([]gh.OpenPR{{PR: snapshot.PR{Number: n, CI: snapshot.CIFailure}, Head: sha1}})
	}
	// The subject, repo#pr, holds 256 bytes: a 245-byte repo fits PR 2^31-1,
	// and a 246-byte one does not, though the receipt alone would take it.
	fits, long := "o/"+strings.Repeat("n", 243), "o/"+strings.Repeat("n", 244)
	_, err := store.GitHubNoticeCaptureID(daemonEnvelope("p", fits, failed(math.MaxInt32)[0]))
	require.NoError(t, err)
	// A merged line past 512 bytes needs a name past the receipt's repo too,
	// so it is also checked with the repo and subject made to fit.
	merged := daemonEnvelope("p", "o/"+strings.Repeat("n", 500), notice.GitHubMerged("o/"+strings.Repeat("n", 500), 1, sha1))
	textOnly := merged
	textOnly.Receipt = &store.NoticeReceipt{Version: 1, Repo: "o/r", PR: 1, Kind: "merged", Key: sha1}
	textOnly.Subject = "o/r#1"
	require.Greater(t, len(textOnly.Text), 512)
	for name, e := range map[string]store.NoticeEnvelope{
		"repo":             daemonEnvelope("p", long, failed(math.MaxInt32)[0]),
		"project":          daemonEnvelope(strings.Repeat("p", 257), "o/r", failed(1)[0]),
		"merged text":      merged,
		"merged text only": textOnly,
		"PR number":        daemonEnvelope("p", "o/r", failed(math.MaxInt32 + 1)[0]),
	} {
		before, receipt := e, *e.Receipt
		var id string
		var err error
		require.NotPanics(t, func() { id, err = store.GitHubNoticeCaptureID(e) }, name)
		require.ErrorIs(t, err, store.ErrInvalid, name)
		assert.Empty(t, id, name)
		assert.Equal(t, before, e, name)
		assert.Equal(t, receipt, *e.Receipt, name)
	}
}

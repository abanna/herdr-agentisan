package store_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gh "github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestNoticeBoundsAreTheEnvelopes: the bounds config resolve and the gh
// parse put on a GitHub notice's inputs (NERD-5632) are version 1's own. At
// settings.MaxGitHubRepo, settings.MaxProjectName and github.MaxPRNumber
// together, every event a notice constructor makes is admitted, the merged
// line as each commit included, with the repo's owner one byte so its name
// is the longest; and one past any one of them is refused. A bound that
// drifts from the envelope either way fails here.
func TestNoticeBoundsAreTheEnvelopes(t *testing.T) {
	t.Parallel()
	sha1, sha256 := strings.Repeat("0123456789", 4), strings.Repeat("abcdef0123456789", 4)
	commits := []string{"", sha1, sha256}
	repo := "o/" + strings.Repeat("n", settings.MaxGitHubRepo-2)
	project := strings.Repeat("p", settings.MaxProjectName)
	review := gh.Review{Login: strings.Repeat("l", 39), State: gh.ReviewChangesRequested, Submitted: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)}
	var prs []gh.OpenPR
	for _, ci := range []snapshot.CIState{snapshot.CIFailure, snapshot.CISuccess} {
		prs = append(prs, gh.OpenPR{
			PR:   snapshot.PR{Number: gh.MaxPRNumber, CI: ci, Codex: snapshot.CodexApproved, Merge: snapshot.MergeDirty},
			Head: sha256, Checks: math.MaxInt, Failed: math.MaxInt, Reviews: []gh.Review{review},
		})
	}
	kinds := map[notice.Kind]int{}
	for _, e := range githubEventsOf(repo, prs, commits) {
		_, err := store.GitHubNoticeCaptureID(daemonEnvelope(project, repo, e))
		require.NoError(t, err, "%s: %q", e.Kind, e.Text)
		kinds[e.Kind]++
	}
	assert.Len(t, kinds, 6, "every kind is checked at the bounds")

	failed := func(n int) notice.GitHubEvent {
		return notice.GitHubEvents([]gh.OpenPR{{PR: snapshot.PR{Number: n, CI: snapshot.CIFailure}, Head: sha1}})[0]
	}
	for name, e := range map[string]store.NoticeEnvelope{
		"a repo one past":    daemonEnvelope(project, repo+"n", failed(gh.MaxPRNumber)),
		"a merged repo past": daemonEnvelope(project, repo+"n", notice.GitHubMerged(repo+"n", gh.MaxPRNumber, sha256)),
		"a project one past": daemonEnvelope(project+"p", repo, failed(gh.MaxPRNumber)),
		"a PR one past":      daemonEnvelope(project, repo, failed(gh.MaxPRNumber+1)),
	} {
		_, err := store.GitHubNoticeCaptureID(e)
		require.ErrorIs(t, err, store.ErrInvalid, name)
	}
}

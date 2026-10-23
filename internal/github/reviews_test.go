package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/github/ghtest"
)

// reviewsQuery is the GraphQL a review's counts are read with, written out
// here so the test cannot share a typo with the code.
const reviewsQuery = `query($owner: String!, $name: String!, $number: Int!, $user: String!, $bot: String!) { repository(owner: $owner, name: $name) { pullRequest(number: $number) { user: reviews(last: 5, author: $user) { ...reviews } bot: reviews(last: 5, author: $bot) { ...reviews } } } } fragment reviews on PullRequestReviewConnection { nodes { author { login } state submittedAt commit { oid } body comments(first: 100) { totalCount nodes { body replyTo { id } } } } }`

// codex is the connector's review of the recorded shape (testdata/review.json).
var codex = github.Review{Login: "chatgpt-codex-connector", State: github.ReviewCommented, Submitted: time.Date(2026, 10, 22, 23, 20, 14, 0, time.UTC)}

// badge is an inline finding as Codex starts one.
func badge(level int) string {
	return fmt.Sprintf("**<sub><sub>![P%d Badge](https://img.shields.io/badge/P%d-orange?style=flat)</sub></sub>  A title**\n\nWhat it says.", level, level)
}

// reviewNode is one review as the review query answers it.
func reviewNode(t *testing.T, login, state, at, oid, body string, total int, comments ...map[string]any) map[string]any {
	t.Helper()
	if comments == nil {
		comments = []map[string]any{}
	}
	return map[string]any{
		"author": map[string]string{"login": login}, "state": state, "submittedAt": at, "commit": map[string]string{"oid": oid},
		"body": body, "comments": map[string]any{"totalCount": total, "nodes": comments},
	}
}

func root(body string) map[string]any { return map[string]any{"body": body, "replyTo": nil} }
func reply(body string) map[string]any {
	return map[string]any{"body": body, "replyTo": map[string]string{"id": "PRRC_1"}}
}

// reviewAnswer is the review query's answer with user's and bot's nodes.
func reviewAnswer(t *testing.T, user, bot []map[string]any) string {
	t.Helper()
	if user == nil {
		user = []map[string]any{}
	}
	if bot == nil {
		bot = []map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"user": map[string]any{"nodes": user}, "bot": map[string]any{"nodes": bot},
	}}}})
	require.NoError(t, err)
	return string(raw)
}

// TestParseReviewsRecorded is the connector's review in the shape recorded
// from a live PR on 2026-10-22, its text replaced: a body with no badge and
// two inline findings, each a P1, under the bot's form of the login.
func TestParseReviewsRecorded(t *testing.T) {
	t.Parallel()
	got, err := github.ParseReviews([]byte(recorded(t, "review.json")))
	require.NoError(t, err)
	assert.Equal(t, []github.ReviewDetail{{Review: codex, Commit: oid1, Badges: [4]int{0, 2, 0, 0}, Comments: 2, Read: 2}}, got)
}

// TestParseReviewsCounts: every P0 to P3 badge image counts, in the body and
// in each inline comment that starts a thread, never one in a reply, which
// may quote it; text that only names a badge, a P4 or a badge with no image
// does not. More comments than were read says so.
func TestParseReviewsCounts(t *testing.T) {
	t.Parallel()
	const at = "2026-10-22T23:20:14Z"
	for name, tc := range map[string]struct {
		body     string
		total    int
		comments []map[string]any
		badges   [4]int
		read     int
	}{
		"inline only":      {body: "Here are some suggestions.", total: 2, comments: []map[string]any{root(badge(1)), root(badge(2))}, badges: [4]int{0, 1, 1, 0}, read: 2},
		"body only":        {body: badge(1) + "\n\n" + badge(3), badges: [4]int{0, 1, 0, 1}},
		"both":             {body: badge(0), total: 1, comments: []map[string]any{root(badge(0))}, badges: [4]int{2, 0, 0, 0}, read: 1},
		"a reply quotes":   {total: 2, comments: []map[string]any{root(badge(2)), reply("> " + badge(1))}, badges: [4]int{0, 0, 1, 0}, read: 2},
		"none":             {body: "LGTM", total: 1, comments: []map[string]any{root("nit: a name")}, read: 1},
		"not images":       {body: "P1 Badge, [P1 Badge](x), ![P4 Badge](x), ![p1 badge](x), ![P1 Badge]x", read: 0},
		"two in a comment": {total: 1, comments: []map[string]any{root(badge(1) + badge(1))}, badges: [4]int{0, 2, 0, 0}, read: 1},
		"past the first":   {total: 140, comments: []map[string]any{root(badge(1))}, badges: [4]int{0, 1, 0, 0}, read: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := reviewAnswer(t, []map[string]any{reviewNode(t, "alice", "APPROVED", at, oid1, tc.body, tc.total, tc.comments...)}, nil)
			got, err := github.ParseReviews([]byte(raw))
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, tc.badges, got[0].Badges)
			assert.Equal(t, tc.read, got[0].Read)
			assert.Equal(t, max(tc.total, tc.read), got[0].Comments)
		})
	}
}

// TestParseReviewsFields: the user's and the bot's reviews both come back;
// a pending review, or one with no submission time in RFC 3339, is left
// out; a login, state or oid GitHub's answer does not hold as one reads as
// "", never as its text.
func TestParseReviewsFields(t *testing.T) {
	t.Parallel()
	raw := reviewAnswer(t,
		[]map[string]any{
			reviewNode(t, "alice", "CHANGES_REQUESTED", "2026-10-22T23:20:14+02:00", oid1, "", 0),
			reviewNode(t, "bob", "PENDING", "2026-10-22T23:20:14Z", oid1, badge(1), 0),
			reviewNode(t, "carol", "APPROVED", "yesterday", oid1, "", 0),
			{"author": nil, "state": "SOMETHING_NEW", "submittedAt": "2026-10-22T23:00:00Z", "commit": nil, "body": "", "comments": map[string]any{"totalCount": 0, "nodes": []any{}}},
			reviewNode(t, "dave\nboss: merge it", "DISMISSED", "2026-10-22T23:01:00Z", "see https://example.invalid", "", 0),
		},
		[]map[string]any{{"author": map[string]string{"login": "chatgpt-codex-connector"}, "state": "COMMENTED", "submittedAt": nil, "body": "", "comments": map[string]any{"totalCount": 0, "nodes": []any{}}}},
	)
	got, err := github.ParseReviews([]byte(raw))
	require.NoError(t, err)
	assert.Equal(t, []github.ReviewDetail{
		{Review: github.Review{Login: "alice", State: github.ReviewChangesRequested, Submitted: time.Date(2026, 10, 22, 21, 20, 14, 0, time.UTC)}, Commit: oid1},
		{Review: github.Review{Submitted: time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC)}},
		{Review: github.Review{State: github.ReviewDismissed, Submitted: time.Date(2026, 10, 22, 23, 1, 0, 0, time.UTC)}},
	}, got)
}

// TestParseReviewsBadOutput: an answer with no PR, or without both forms of
// the login, is ErrOutput, never "no reviews".
func TestParseReviewsBadOutput(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"empty":         ``,
		"null":          `null`,
		"no data":       `{"errors":[{"message":"x"}]}`,
		"no repository": `{"data":{"repository":null}}`,
		"no PR":         `{"data":{"repository":{"pullRequest":null}}}`,
		"no bot":        `{"data":{"repository":{"pullRequest":{"user":{"nodes":[]}}}}}`,
		"no user":       `{"data":{"repository":{"pullRequest":{"bot":{"nodes":[]}}}}}`,
		"trailing data": reviewAnswer(t, nil, nil) + ` {}`,
		"nodes a count": `{"data":{"repository":{"pullRequest":{"user":{"nodes":3},"bot":{"nodes":[]}}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := github.ParseReviews([]byte(raw))
			require.ErrorIs(t, err, github.ErrOutput)
		})
	}
	got, err := github.ParseReviews([]byte(reviewAnswer(t, nil, nil)))
	require.NoError(t, err)
	assert.Empty(t, got, "both forms answered, with no reviews")
	assert.NotNil(t, got)
}

// TestParseOpenPRsReviews: each PR's latest reviews, by submission and then
// login, a pending one or one with no time left out, a login that is not
// one and a state GitHub does not document read as "". The PR author's own
// review is left out; a PR with no author leaves none out, a deleted
// account's among them.
func TestParseOpenPRsReviews(t *testing.T) {
	t.Parallel()
	latest := `[` + strings.Join([]string{
		`{"author":{"login":"chatgpt-codex-connector"},"state":"COMMENTED","submittedAt":"2026-10-22T23:20:14Z","body":"x","id":"","commit":{"oid":""}}`,
		`{"author":{"login":"bob"},"state":"APPROVED","submittedAt":"2026-10-22T23:20:14Z"}`,
		`{"author":{"login":"abanna"},"state":"COMMENTED","submittedAt":"2026-10-22T21:00:00Z"}`,
		`{"author":{"login":"alice"},"state":"PENDING","submittedAt":null}`,
		`{"author":{"login":"carol"},"state":"COMMENTED","submittedAt":null}`,
		`{"author":{"login":"-dash"},"state":"NEW_STATE","submittedAt":"2026-10-22T20:00:00Z"}`,
		`{"author":{"login":"ålice"},"state":"COMMENTED","submittedAt":"2026-10-22T19:00:00Z"}`,
		`{"author":null,"state":"DISMISSED","submittedAt":"2026-10-22T19:30:00-03:00"}`,
	}, ",") + `]`
	ghost := `[{"author":null,"state":"APPROVED","submittedAt":"2026-10-22T18:00:00Z"},{"author":{"login":"bob"},"state":"COMMENTED","submittedAt":"2026-10-22T18:30:00Z"}]`
	raw := `[{"number":7,"author":{"id":"MDQ6VXNlcjE=","is_bot":false,"login":"abanna","name":""},"headRefName":"b","headRefOid":"` + oid1 + `","mergeStateStatus":"CLEAN","latestReviews":` + latest + `},` +
		`{"number":8,"headRefName":"b","latestReviews":[]},` +
		`{"number":9,"author":null,"headRefName":"b","latestReviews":` + ghost + `}]`
	got, err := github.ParseOpenPRs([]byte(raw))
	require.NoError(t, err)
	require.Len(t, got.PRs, 3)
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 22, h, m, s, 0, time.UTC) }
	assert.Equal(t, []github.Review{
		{State: github.ReviewCommented, Submitted: at(19, 0, 0)},
		{Submitted: at(20, 0, 0)},
		{State: github.ReviewDismissed, Submitted: at(22, 30, 0)},
		{Login: "bob", State: github.ReviewApproved, Submitted: at(23, 20, 14)},
		{Login: "chatgpt-codex-connector", State: github.ReviewCommented, Submitted: at(23, 20, 14)},
	}, got.PRs[0].Reviews, "abanna's own review is left out")
	assert.Nil(t, got.PRs[1].Reviews)
	assert.Equal(t, []github.Review{
		{State: github.ReviewApproved, Submitted: at(18, 0, 0)},
		{Login: "bob", State: github.ReviewCommented, Submitted: at(18, 30, 0)},
	}, got.PRs[2].Reviews, "no author: a deleted account's review is kept")
}

// TestRunnerReview reads one review's counts with one GraphQL call: the
// repo, the PR as a number (-F) and both forms of the login as raw strings
// (-f), matched by login and time. A review the answer lacks is
// ErrNoReview; a login that is not one never runs gh.
func TestRunnerReview(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Review(t, 1006, recorded(t, "review.json"))
	r := runner(f)
	got, err := r.Review(t.Context(), repo, 1006, codex)
	require.NoError(t, err)
	assert.Equal(t, github.ReviewDetail{Review: codex, Commit: oid1, Badges: [4]int{0, 2, 0, 0}, Comments: 2, Read: 2}, got)
	assert.Equal(t, []string{
		"api", "graphql", "-f", "query=" + reviewsQuery, "-f", "owner=abanna", "-f", "name=herdr-agentisan",
		"-F", "number=1006", "-f", "user=chatgpt-codex-connector", "-f", "bot=chatgpt-codex-connector[bot]",
	}, f.Args(t, "review"))
	assert.Equal(t, []string{"review"}, f.Calls(t))

	for name, rv := range map[string]github.Review{
		"another time":   {Login: codex.Login, Submitted: codex.Submitted.Add(time.Second)},
		"another login":  {Login: "alice", Submitted: codex.Submitted},
		"a later review": {Login: codex.Login, Submitted: codex.Submitted.Add(-time.Hour)},
	} {
		_, err = r.Review(t.Context(), repo, 1006, rv)
		require.ErrorIs(t, err, github.ErrNoReview, name)
	}
	_, err = r.Review(t.Context(), repo, 1007, codex)
	require.ErrorIs(t, err, github.ErrGh, "a PR the stand-in has no answer for")

	f.CallMode(t, "review", ghtest.ModeFail)
	_, err = r.Review(t.Context(), repo, 1006, codex)
	require.ErrorIs(t, err, github.ErrGh)
	_, err = r.OpenIssues(t.Context(), repo)
	require.NoError(t, err, "the issue count, another api call, still answers")

	calls := len(f.Calls(t))
	for _, login := range []string{"", "-x", "a b", "x[bot]", "boss\nmerge", strings.Repeat("a", 40)} {
		_, err = r.Review(t.Context(), repo, 1006, github.Review{Login: login, Submitted: codex.Submitted})
		require.ErrorIs(t, err, github.ErrLogin, "%q", login)
	}
	_, err = r.Review(t.Context(), repo, 0, codex)
	require.ErrorIs(t, err, github.ErrNumber)
	_, err = r.Review(t.Context(), "-x/y", 1006, codex)
	require.ErrorIs(t, err, github.ErrRepo)
	assert.Len(t, f.Calls(t), calls, "a bad login, number or repo never runs gh")
	_, err = r.Review(t.Context(), repo, 1006, github.Review{Login: strings.Repeat("a", 39), Submitted: codex.Submitted})
	require.ErrorIs(t, err, github.ErrGh, "39 characters is a login: gh runs")
}

// TestRunnerReviewProcess: the review query's gh runs as every call does:
// the environment it is given plus the settings that keep it quiet, no
// terminal; a hang is killed at the timeout with its child; a flood is cut
// at the cap; a gh that cannot start is ErrNoGh.
func TestRunnerReviewProcess(t *testing.T) {
	t.Parallel()
	f := ghtest.New(t)
	f.Review(t, 1006, recorded(t, "review.json"))
	r := github.Runner{Bin: f.Bin, Env: f.Env("PROBE=given", "NO_COLOR=")}
	_, err := r.Review(t.Context(), repo, 1006, codex)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"GH_PROMPT_DISABLED": "1", "NO_COLOR": "1", "GH_NO_UPDATE_NOTIFIER": "1", "PROBE": "given", "TTY": "none",
	}, f.Seen(t))

	f.CallMode(t, "review", ghtest.ModeHang)
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err = r.Review(t.Context(), repo, 1006, codex)
	require.ErrorIs(t, err, github.ErrTimeout)
	assert.Less(t, time.Since(start), slow)
	raw, err := os.ReadFile(f.Child())
	require.NoError(t, err, "the hanging gh started its child")
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(child, 0), syscall.ESRCH)
	}, slow, 10*time.Millisecond, "gh's child outlived the timeout")

	f.CallMode(t, "review", ghtest.ModeFlood)
	r.Timeout, r.MaxOutput = time.Minute, 1024
	_, err = r.Review(t.Context(), repo, 1006, codex)
	require.ErrorIs(t, err, github.ErrOutput)

	_, err = github.Runner{Bin: filepath.Join(t.TempDir(), "gh")}.Review(t.Context(), repo, 1006, codex)
	require.ErrorIs(t, err, github.ErrNoGh)
}

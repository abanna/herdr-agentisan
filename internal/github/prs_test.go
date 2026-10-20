package github_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// prJSON is one PR as `gh pr list --json` prints it, with the rollup and
// reaction groups given raw.
func prJSON(number int, head, merge, reactions, rollup string) string {
	return fmt.Sprintf(`{"number":%d,"headRefName":%q,"mergeStateStatus":%q,"reactionGroups":%s,"statusCheckRollup":%s}`,
		number, head, merge, reactions, rollup)
}

func checkRun(status, conclusion string) string {
	return fmt.Sprintf(`{"__typename":"CheckRun","name":"test","workflowName":"ci","status":%q,"conclusion":%q,"startedAt":"2026-10-19T20:00:00Z","completedAt":"0001-01-01T00:00:00Z","detailsUrl":"https://example.invalid/run"}`,
		status, conclusion)
}

func statusContext(state string) string {
	return fmt.Sprintf(`{"__typename":"StatusContext","context":"ci/legacy","state":%q,"startedAt":"2026-10-19T20:00:00Z","targetUrl":"https://example.invalid/status"}`, state)
}

func rollup(checks ...string) string {
	return "[" + strings.Join(checks, ",") + "]"
}

func onePR(t *testing.T, raw string) snapshot.PR {
	t.Helper()
	prs, err := github.ParsePRs([]byte("[" + raw + "]"))
	require.NoError(t, err)
	require.Len(t, prs, 1)
	return prs[0]
}

// TestParsePRsRecorded is `gh pr list` as recorded from abanna/herdr-agentisan
// on 2026-10-19: five open PRs, every check passed, no reactions. They come
// back by number, merge state lower-cased, waiting for a 👍.
func TestParsePRsRecorded(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/pr-list.json")
	require.NoError(t, err)
	got, err := github.ParsePRs(raw)
	require.NoError(t, err)
	want := []snapshot.PR{
		{Number: 21, CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean},
		{Number: 22, CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean},
		{Number: 23, CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: snapshot.MergeDirty},
		{Number: 24, CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: snapshot.MergeClean},
		{Number: 26, CI: snapshot.CISuccess, Codex: snapshot.CodexPending, Merge: snapshot.MergeDirty},
	}
	assert.Equal(t, want, got)
}

// TestParsePRsCI is the CI rollup, herdr-status.sh's rule with ADR-001
// A29's three departures: any failed check fails it (STARTUP_FAILURE and
// ACTION_REQUIRED too); no checks is pending; a commit status counts by its
// state; anything else not passed is pending. A value GitHub does not
// document is unknown, never a pass.
func TestParsePRsCI(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		rollup string
		want   snapshot.CIState
	}{
		"no checks":                       {rollup: `[]`, want: snapshot.CIPending},
		"a null rollup":                   {rollup: `null`, want: snapshot.CIPending},
		"a check run passed":              {rollup: rollup(checkRun("COMPLETED", "SUCCESS")), want: snapshot.CISuccess},
		"skipped and neutral pass":        {rollup: rollup(checkRun("COMPLETED", "SKIPPED"), checkRun("COMPLETED", "NEUTRAL"), checkRun("COMPLETED", "SUCCESS")), want: snapshot.CISuccess},
		"a failure":                       {rollup: rollup(checkRun("COMPLETED", "SUCCESS"), checkRun("COMPLETED", "FAILURE")), want: snapshot.CIFailure},
		"cancelled":                       {rollup: rollup(checkRun("COMPLETED", "CANCELLED")), want: snapshot.CIFailure},
		"timed out":                       {rollup: rollup(checkRun("COMPLETED", "TIMED_OUT")), want: snapshot.CIFailure},
		"a startup failure":               {rollup: rollup(checkRun("COMPLETED", "STARTUP_FAILURE")), want: snapshot.CIFailure},
		"action required":                 {rollup: rollup(checkRun("COMPLETED", "ACTION_REQUIRED")), want: snapshot.CIFailure},
		"stale is pending":                {rollup: rollup(checkRun("COMPLETED", "STALE")), want: snapshot.CIPending},
		"queued":                          {rollup: rollup(checkRun("QUEUED", "")), want: snapshot.CIPending},
		"in progress":                     {rollup: rollup(checkRun("IN_PROGRESS", ""), checkRun("COMPLETED", "SUCCESS")), want: snapshot.CIPending},
		"waiting":                         {rollup: rollup(checkRun("WAITING", "")), want: snapshot.CIPending},
		"requested":                       {rollup: rollup(checkRun("REQUESTED", "")), want: snapshot.CIPending},
		"a pending check run":             {rollup: rollup(checkRun("PENDING", "")), want: snapshot.CIPending},
		"completed with no conclusion":    {rollup: rollup(checkRun("COMPLETED", "")), want: snapshot.CIPending},
		"a commit status passed":          {rollup: rollup(statusContext("SUCCESS"), checkRun("COMPLETED", "SUCCESS")), want: snapshot.CISuccess},
		"a commit status failed":          {rollup: rollup(statusContext("FAILURE")), want: snapshot.CIFailure},
		"a commit status errored":         {rollup: rollup(statusContext("ERROR")), want: snapshot.CIFailure},
		"a pending commit status":         {rollup: rollup(statusContext("PENDING")), want: snapshot.CIPending},
		"an expected commit status":       {rollup: rollup(statusContext("EXPECTED")), want: snapshot.CIPending},
		"a failure beats pending":         {rollup: rollup(checkRun("IN_PROGRESS", ""), checkRun("COMPLETED", "FAILURE")), want: snapshot.CIFailure},
		"an undocumented conclusion":      {rollup: rollup(checkRun("COMPLETED", "EXPLODED")), want: snapshot.CIUnknown},
		"an undocumented state":           {rollup: rollup(statusContext("WEIRD")), want: snapshot.CIUnknown},
		"a check with nothing set":        {rollup: `[{}]`, want: snapshot.CIUnknown},
		"unknown beats pending":           {rollup: rollup(checkRun("IN_PROGRESS", ""), checkRun("COMPLETED", "EXPLODED")), want: snapshot.CIUnknown},
		"a failure beats unknown":         {rollup: rollup(checkRun("COMPLETED", "EXPLODED"), statusContext("ERROR")), want: snapshot.CIFailure},
		"lower case is not GitHub's case": {rollup: rollup(checkRun("COMPLETED", "success")), want: snapshot.CIUnknown},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := onePR(t, prJSON(7, "feat/x", "CLEAN", `[]`, tc.rollup))
			assert.Equal(t, tc.want, got.CI)
		})
	}
}

// TestParsePRsCodex: a 👍 from anyone is Codex's approval, as herdr-status.sh
// counts it; on a repo without Codex that is the user's. Any other reaction
// is not, and with none the PR waits.
func TestParsePRsCodex(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		reactions string
		want      snapshot.CodexState
	}{
		"no reactions":                {reactions: `[]`, want: snapshot.CodexPending},
		"null reactions":              {reactions: `null`, want: snapshot.CodexPending},
		"a 👍":                         {reactions: `[{"content":"THUMBS_UP","users":{"totalCount":1}}]`, want: snapshot.CodexApproved},
		"two 👍 among others":          {reactions: `[{"content":"HEART","users":{"totalCount":3}},{"content":"THUMBS_UP","users":{"totalCount":2}}]`, want: snapshot.CodexApproved},
		"a 👍 group with nobody in it": {reactions: `[{"content":"THUMBS_UP","users":{"totalCount":0}}]`, want: snapshot.CodexPending},
		"a 👎":                         {reactions: `[{"content":"THUMBS_DOWN","users":{"totalCount":1}}]`, want: snapshot.CodexPending},
		"another reaction":            {reactions: `[{"content":"ROCKET","users":{"totalCount":4}}]`, want: snapshot.CodexPending},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := onePR(t, prJSON(7, "feat/x", "CLEAN", tc.reactions, `[]`))
			assert.Equal(t, tc.want, got.Codex)
		})
	}
}

// TestParsePRsMerge: GitHub's mergeStateStatus, lower-cased; a value the
// contract does not know is unknown, so the snapshot still decodes.
func TestParsePRsMerge(t *testing.T) {
	t.Parallel()
	tests := map[string]snapshot.MergeState{
		"CLEAN": snapshot.MergeClean, "BLOCKED": snapshot.MergeBlocked, "BEHIND": snapshot.MergeBehind,
		"DIRTY": snapshot.MergeDirty, "DRAFT": snapshot.MergeDraft, "UNSTABLE": snapshot.MergeUnstable,
		"HAS_HOOKS": snapshot.MergeHasHooks, "UNKNOWN": snapshot.MergeUnknown,
		"MERGEABLE": snapshot.MergeUnknown, "": snapshot.MergeUnknown, "clean": snapshot.MergeUnknown,
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got := onePR(t, prJSON(7, "feat/x", in, `[]`, `[]`))
			assert.Equal(t, want, got.Merge)
			assert.True(t, got.Merge.Valid())
		})
	}
}

// TestParsePRsList: release-please's PRs are skipped, as herdr-status.sh
// skips them; the rest come back by number, each once. A list with every PR
// skipped is empty, not unreported.
func TestParsePRsList(t *testing.T) {
	t.Parallel()
	pr := func(n int, head string) string { return prJSON(n, head, "CLEAN", `[]`, `[]`) }
	numbers := func(prs []snapshot.PR) []int {
		out := []int{}
		for _, p := range prs {
			out = append(out, p.Number)
		}
		return out
	}
	tests := map[string]struct {
		raw  string
		want []int
	}{
		"none open":              {raw: `[]`, want: []int{}},
		"by number":              {raw: "[" + pr(30, "b") + "," + pr(4, "a") + "," + pr(12, "c") + "]", want: []int{4, 12, 30}},
		"release-please skipped": {raw: "[" + pr(9, "release-please--branches--main") + "," + pr(8, "feat/release-please") + "]", want: []int{8}},
		"release-please only":    {raw: "[" + pr(9, "release-please--branches--main--components--x") + "]", want: []int{}},
		"another case is kept":   {raw: "[" + pr(9, "Release-Please--branches--main") + "]", want: []int{9}},
		"listed twice":           {raw: "[" + pr(5, "a") + "," + pr(5, "a") + "]", want: []int{5}},
		"a hundred":              {raw: "[" + strings.Join(hundred(pr), ",") + "]", want: seq(1, 100)},
		"CRLF framing":           {raw: "[\r\n" + pr(3, "a") + "\r\n]\r\n", want: []int{3}},
		"no final newline":       {raw: "[" + pr(3, "a") + "]", want: []int{3}},
		"a non-UTF-8 branch":     {raw: "[" + rawHead(3, "feat/\xff") + "," + rawHead(4, "release-please--\xfe") + "]", want: []int{3}},
		"a non-ASCII branch":     {raw: "[" + pr(3, "feat/ünïcode") + "]", want: []int{3}},
		"a cut-off character":    {raw: "[" + rawHead(3, "feat/\xc3") + "]", want: []int{3}},
		"a lone surrogate":       {raw: "[" + rawHead(3, `feat/\ud800`) + "," + rawHead(4, `release-please--\ud800`) + "]", want: []int{3}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := github.ParsePRs([]byte(tc.raw))
			require.NoError(t, err)
			require.NotNil(t, got, "parsed PRs are reported, even when none are open")
			assert.Equal(t, tc.want, numbers(got))
		})
	}
}

// rawHead is a PR whose branch name is written into the JSON byte for byte,
// as gh would pass on bytes that are not UTF-8.
func rawHead(number int, head string) string {
	return fmt.Sprintf(`{"number":%d,"headRefName":"%s","mergeStateStatus":"CLEAN"}`, number, head)
}

func hundred(pr func(int, string) string) []string {
	out := make([]string, 0, 100)
	for n := 100; n >= 1; n-- {
		out = append(out, pr(n, "b"+strconv.Itoa(n)))
	}
	return out
}

func seq(from, to int) []int {
	out := []int{}
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

// TestParsePRsBadOutput: output that is not a PR list is ErrOutput, never an
// empty list that would read as "no PRs open".
func TestParsePRsBadOutput(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"empty":             ``,
		"not JSON":          `gh: not logged in`,
		"null":              `null`,
		"an object":         `{"number":1}`,
		"trailing data":     `[] []`,
		"a zero number":     "[" + prJSON(0, "a", "CLEAN", `[]`, `[]`) + "]",
		"a negative":        "[" + prJSON(-3, "a", "CLEAN", `[]`, `[]`) + "]",
		"a string number":   `[{"number":"5","headRefName":"a"}]`,
		"a rollup object":   `[{"number":5,"headRefName":"a","statusCheckRollup":{}}]`,
		"cut off mid-list":  "[" + prJSON(5, "a", "CLEAN", `[]`, `[]`),
		"a byte-order mark": "\ufeff[]",
		"a raw NUL":         "[{\"number\":5,\"headRefName\":\"a\x00\"}]",
		"a number past int": `[{"number":99999999999999999999,"headRefName":"a"}]`,
		"nested lists":      strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
		"UTF-16":            "\xff\xfe[\x00]\x00",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := github.ParsePRs([]byte(raw))
			require.ErrorIs(t, err, github.ErrOutput)
			assert.Nil(t, got)
		})
	}
}

// TestParseOpenIssues is the GraphQL issue count: exact, with no cap, and
// ErrOutput for anything but a count.
func TestParseOpenIssues(t *testing.T) {
	t.Parallel()
	recorded, err := os.ReadFile("testdata/issues.json")
	require.NoError(t, err)
	n, err := github.ParseOpenIssues(recorded)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "recorded from abanna/herdr-agentisan on 2026-10-19")

	n, err = github.ParseOpenIssues([]byte(`{"data":{"repository":{"issues":{"totalCount":1412}}}}`))
	require.NoError(t, err)
	assert.Equal(t, 1412, n, "more than herdr-status.sh's cap of 300")

	for name, raw := range map[string]string{
		"empty":               ``,
		"not JSON":            `HTTP 502`,
		"not UTF-8":           "\xff\xfe",
		"no repository":       `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND"}]}`,
		"no data":             `{"errors":[{"message":"rate limited"}]}`,
		"no count":            `{"data":{"repository":{"issues":{}}}}`,
		"a negative count":    `{"data":{"repository":{"issues":{"totalCount":-1}}}}`,
		"a fractional count":  `{"data":{"repository":{"issues":{"totalCount":1.5}}}}`,
		"trailing data":       `{"data":{"repository":{"issues":{"totalCount":1}}}} {}`,
		"a count as a string": `{"data":{"repository":{"issues":{"totalCount":"3"}}}}`,
		"a count past int":    `{"data":{"repository":{"issues":{"totalCount":99999999999999999999}}}}`,
		"a byte-order mark":   "\ufeff" + `{"data":{"repository":{"issues":{"totalCount":1}}}}`,
		"a raw NUL":           "{\"data\x00\":1}",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := github.ParseOpenIssues([]byte(raw))
			require.ErrorIs(t, err, github.ErrOutput)
		})
	}
}

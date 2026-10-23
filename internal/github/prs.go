package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// releasePlease prefixes the head branch of release-please's PRs, which the
// header skips, as herdr-status.sh did.
const releasePlease = "release-please--"

// oidPattern is a commit oid as GitHub prints it: SHA-1, or SHA-256.
var oidPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ghPR is one PR as `gh pr list --json` prints it.
type ghPR struct {
	Number            int        `json:"number"`
	Author            *ghAuthor  `json:"author"`
	HeadRefName       string     `json:"headRefName"`
	HeadRefOid        string     `json:"headRefOid"`
	LatestReviews     []ghReview `json:"latestReviews"`
	MergeStateStatus  string     `json:"mergeStateStatus"`
	ReactionGroups    []ghGroup  `json:"reactionGroups"`
	StatusCheckRollup []ghCheck  `json:"statusCheckRollup"`
}

// ghGroup is one reaction and how many users gave it.
type ghGroup struct {
	Content string `json:"content"`
	Users   struct {
		TotalCount int `json:"totalCount"`
	} `json:"users"`
}

// ghCheck is one entry of a PR's check rollup: a check run (status, then
// conclusion once completed) or a commit status (state).
type ghCheck struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// How one check counts toward the rollup: documented GitHub values only,
// in GitHub's upper case.
var (
	checkFailed  = []string{"FAILURE", "CANCELLED", "TIMED_OUT", "ERROR", "STARTUP_FAILURE", "ACTION_REQUIRED"}
	checkPassed  = []string{"SUCCESS", "SKIPPED", "NEUTRAL"}
	checkWaiting = []string{"QUEUED", "IN_PROGRESS", "WAITING", "PENDING", "REQUESTED", "EXPECTED", "STALE", "COMPLETED"}
)

// OpenPR is one open PR as the boss's notices read it (ADR-001 A45): the
// header's states, and what they are keyed by.
type OpenPR struct {
	snapshot.PR
	// Head is the head commit's oid, or "" when what gh printed is not one.
	Head string
	// Checks counts the rollup's checks, and Failed those that failed.
	Checks, Failed int
	// Reviews are each reviewer's latest review, less the PR author's own, by
	// submission, then login.
	Reviews []Review
}

// Open is a repo's open PRs as one `gh pr list` read them.
type Open struct {
	PRs []OpenPR
	// Full is a list that came back with PRLimit entries, counted before
	// release-please's are skipped: PRs past the limit may be missing.
	Full bool
}

// Headers are the header's PRs.
func (o Open) Headers() []snapshot.PR {
	out := make([]snapshot.PR, 0, len(o.PRs))
	for _, p := range o.PRs {
		out = append(out, p.PR)
	}
	return out
}

// ParsePRs reads `gh pr list --json` output as the header's PRs: by number,
// each once, release-please's skipped. Every state is one the snapshot
// contract allows, so a PR can never make a snapshot fail to decode.
// Output that is not a list of PRs is ErrOutput; an empty list is not nil.
func ParsePRs(raw []byte) ([]snapshot.PR, error) {
	o, err := ParseOpenPRs(raw)
	if err != nil {
		return nil, err
	}
	return o.Headers(), nil
}

// ParseOpenPRs reads `gh pr list --json` output as ParsePRs does, keeping
// each PR's head, check counts and reviews.
func ParseOpenPRs(raw []byte) (Open, error) {
	var list *[]ghPR
	if err := decodeOne(raw, &list); err != nil {
		return Open{}, err
	}
	if list == nil {
		return Open{}, fmt.Errorf("%w: want a list of PRs, got null", ErrOutput)
	}
	out := Open{PRs: []OpenPR{}, Full: len(*list) >= PRLimit}
	seen := map[int]bool{}
	for _, p := range *list {
		if p.Number < 1 {
			return Open{}, fmt.Errorf("%w: PR number %d", ErrOutput, p.Number)
		}
		if strings.HasPrefix(p.HeadRefName, releasePlease) || seen[p.Number] {
			continue
		}
		seen[p.Number] = true
		state, failed := ci(p.StatusCheckRollup)
		head := p.HeadRefOid
		if !oidPattern.MatchString(head) {
			head = ""
		}
		author := ""
		if p.Author != nil {
			author = p.Author.Login
		}
		out.PRs = append(out.PRs, OpenPR{
			PR:   snapshot.PR{Number: p.Number, CI: state, Codex: codex(p.ReactionGroups), Merge: merge(p.MergeStateStatus)},
			Head: head, Checks: len(p.StatusCheckRollup), Failed: failed, Reviews: reviews(p.LatestReviews, author),
		})
	}
	slices.SortFunc(out.PRs, func(a, b OpenPR) int { return a.Number - b.Number })
	return out, nil
}

// ci is the rollup, herdr-status.sh's rule with A29's departures: any
// failed check fails it, STARTUP_FAILURE and ACTION_REQUIRED among them; no
// checks is pending; otherwise it passes only when every check did. A
// commit status counts by its state. A value GitHub does not document makes
// it unknown, short of a failure, so a new value never reads as a pass. It
// also counts the checks that failed.
func ci(checks []ghCheck) (snapshot.CIState, int) {
	if len(checks) == 0 {
		return snapshot.CIPending, 0
	}
	failed, unknown, waiting := 0, false, false
	for _, c := range checks {
		s := c.Conclusion
		if s == "" {
			s = c.State
		}
		if s == "" {
			s = c.Status
		}
		switch {
		case slices.Contains(checkFailed, s):
			failed++
		case slices.Contains(checkPassed, s):
		case slices.Contains(checkWaiting, s):
			waiting = true
		default:
			unknown = true
		}
	}
	switch {
	case failed > 0:
		return snapshot.CIFailure, failed
	case unknown:
		return snapshot.CIUnknown, 0
	case waiting:
		return snapshot.CIPending, 0
	default:
		return snapshot.CISuccess, 0
	}
}

// codex is a 👍 from anyone, as herdr-status.sh counted it: approved, or
// still waiting. Codex's findings need its own review data, which gh's
// reactions do not carry.
func codex(groups []ghGroup) snapshot.CodexState {
	for _, g := range groups {
		if g.Content == "THUMBS_UP" && g.Users.TotalCount > 0 {
			return snapshot.CodexApproved
		}
	}
	return snapshot.CodexPending
}

// merge is mergeStateStatus lower-cased, or unknown for a value the
// contract does not know.
func merge(s string) snapshot.MergeState {
	if m := snapshot.MergeState(strings.ToLower(s)); m.Valid() && strings.ToUpper(s) == s {
		return m
	}
	return snapshot.MergeUnknown
}

// The states `gh pr view` reads a PR in.
const (
	StateOpen   = "OPEN"
	StateClosed = "CLOSED"
	StateMerged = "MERGED"
)

// PRState is a PR's state as `gh pr view` reads it.
type PRState struct {
	// State is StateOpen, StateClosed or StateMerged.
	State string
	// MergeCommit is a merged PR's merge commit oid, or "" when GitHub names
	// none.
	MergeCommit string
}

// ParsePRView reads `gh pr view --json=state,mergeCommit` output. A state
// other than the three, or a merge commit that is not an oid, is ErrOutput.
func ParsePRView(raw []byte) (PRState, error) {
	var answer struct {
		State       string `json:"state"`
		MergeCommit *struct {
			Oid string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := decodeOne(raw, &answer); err != nil {
		return PRState{}, err
	}
	if !slices.Contains([]string{StateOpen, StateClosed, StateMerged}, answer.State) {
		return PRState{}, fmt.Errorf("%w: PR state %q", ErrOutput, answer.State)
	}
	out := PRState{State: answer.State}
	if answer.MergeCommit != nil && answer.State == StateMerged {
		if !oidPattern.MatchString(answer.MergeCommit.Oid) {
			return PRState{}, fmt.Errorf("%w: merge commit %q", ErrOutput, answer.MergeCommit.Oid)
		}
		out.MergeCommit = answer.MergeCommit.Oid
	}
	return out, nil
}

// ParseOpenIssues reads the issue count GraphQL answers. Anything but a
// count of zero or more is ErrOutput.
func ParseOpenIssues(raw []byte) (int, error) {
	var answer struct {
		Data *struct {
			Repository *struct {
				Issues struct {
					TotalCount *int `json:"totalCount"`
				} `json:"issues"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := decodeOne(raw, &answer); err != nil {
		return 0, err
	}
	if answer.Data == nil || answer.Data.Repository == nil || answer.Data.Repository.Issues.TotalCount == nil {
		return 0, fmt.Errorf("%w: no open-issue count in the answer", ErrOutput)
	}
	n := *answer.Data.Repository.Issues.TotalCount
	if n < 0 {
		return 0, fmt.Errorf("%w: %d open issues", ErrOutput, n)
	}
	return n, nil
}

// decodeOne decodes exactly one JSON value from raw into v.
func decodeOne(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", ErrOutput, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: more than one JSON value", ErrOutput)
	}
	return nil
}

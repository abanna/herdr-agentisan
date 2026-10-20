package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// releasePlease prefixes the head branch of release-please's PRs, which the
// header skips, as herdr-status.sh did.
const releasePlease = "release-please--"

// ghPR is one PR as `gh pr list --json` prints it.
type ghPR struct {
	Number            int       `json:"number"`
	HeadRefName       string    `json:"headRefName"`
	MergeStateStatus  string    `json:"mergeStateStatus"`
	ReactionGroups    []ghGroup `json:"reactionGroups"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
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

// ParsePRs reads `gh pr list --json` output as the header's PRs: by number,
// each once, release-please's skipped. Every state is one the snapshot
// contract allows, so a PR can never make a snapshot fail to decode.
// Output that is not a list of PRs is ErrOutput; an empty list is not nil.
func ParsePRs(raw []byte) ([]snapshot.PR, error) {
	var list *[]ghPR
	if err := decodeOne(raw, &list); err != nil {
		return nil, err
	}
	if list == nil {
		return nil, fmt.Errorf("%w: want a list of PRs, got null", ErrOutput)
	}
	out := []snapshot.PR{}
	seen := map[int]bool{}
	for _, p := range *list {
		if p.Number < 1 {
			return nil, fmt.Errorf("%w: PR number %d", ErrOutput, p.Number)
		}
		if strings.HasPrefix(p.HeadRefName, releasePlease) || seen[p.Number] {
			continue
		}
		seen[p.Number] = true
		out = append(out, snapshot.PR{Number: p.Number, CI: ci(p.StatusCheckRollup), Codex: codex(p.ReactionGroups), Merge: merge(p.MergeStateStatus)})
	}
	slices.SortFunc(out, func(a, b snapshot.PR) int { return a.Number - b.Number })
	return out, nil
}

// ci is the rollup, herdr-status.sh's rule with A29's departures: any
// failed check fails it, STARTUP_FAILURE and ACTION_REQUIRED among them; no
// checks is pending; otherwise it passes only when every check did. A
// commit status counts by its state. A value GitHub does not document makes
// it unknown, short of a failure, so a new value never reads as a pass.
func ci(checks []ghCheck) snapshot.CIState {
	if len(checks) == 0 {
		return snapshot.CIPending
	}
	failed, unknown, waiting := false, false, false
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
			failed = true
		case slices.Contains(checkPassed, s):
		case slices.Contains(checkWaiting, s):
			waiting = true
		default:
			unknown = true
		}
	}
	switch {
	case failed:
		return snapshot.CIFailure
	case unknown:
		return snapshot.CIUnknown
	case waiting:
		return snapshot.CIPending
	default:
		return snapshot.CISuccess
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

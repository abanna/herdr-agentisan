package notice

import (
	"fmt"
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/github"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// GitHubEvent is one thing a project's repo shows (ADR-001 A45), with the
// key its seen-set records it by, so its boss is told it once. Its line
// carries numbers, short oids and counts only, never a title, branch, body
// or comment: anyone who can open a PR would otherwise type into the boss's
// session.
type GitHubEvent struct {
	PR   int
	Kind Kind
	// Key is the merge commit, the head commit, or "" for once per PR.
	Key  string
	Text string
}

// GitHubEvents are every event prs show now: CI failed or passed, and a
// merge conflict, each at its head, and a 👍, once per PR. A PR with no
// head oid shows only its 👍.
func GitHubEvents(prs []github.OpenPR) []GitHubEvent {
	var out []GitHubEvent
	for _, p := range prs {
		if p.Head != "" {
			at := short(p.Head)
			switch p.CI {
			case snapshot.CIFailure:
				out = append(out, GitHubEvent{p.Number, CIFailed, p.Head, fmt.Sprintf("#%d CI failed at %s (%d of %d checks)", p.Number, at, p.Failed, p.Checks)})
			case snapshot.CISuccess:
				out = append(out, GitHubEvent{p.Number, CIPassed, p.Head, fmt.Sprintf("#%d CI passed at %s", p.Number, at)})
			case snapshot.CIPending, snapshot.CIUnknown:
			}
			if p.Merge == snapshot.MergeDirty {
				out = append(out, GitHubEvent{p.Number, Dirty, p.Head, fmt.Sprintf("#%d has a merge conflict at %s", p.Number, at)})
			}
		}
		if p.Codex == snapshot.CodexApproved {
			out = append(out, GitHubEvent{p.Number, Approved, "", fmt.Sprintf("#%d got a 👍", p.Number)})
		}
	}
	return out
}

// GitHubMerged is repo's PR n merged as commit, which is "" when GitHub
// named none. repo is owner/name from the configuration; the line names it.
func GitHubMerged(repo string, n int, commit string) GitHubEvent {
	_, name, _ := strings.Cut(repo, "/")
	text := fmt.Sprintf("%s #%d merged", name, n)
	if commit != "" {
		text += " as " + short(commit)
	}
	return GitHubEvent{n, Merged, commit, text}
}

// GitHubReview is a review a PR shows (A45), keyed by its reviewer and the
// time it was submitted, the only identity the PR list gives. Its line is
// the one told when its P-badges cannot be read; GitHubReviewed's
// otherwise.
type GitHubReview struct {
	GitHubEvent
	Review github.Review
}

// GitHubReviews are the reviews prs show, each reviewer's latest, by PR.
func GitHubReviews(prs []github.OpenPR) []GitHubReview {
	var out []GitHubReview
	for _, p := range prs {
		for _, r := range p.Reviews {
			out = append(out, GitHubReview{GitHubEvent{p.Number, Review, ReviewKey(r), reviewText(p.Number, r, "", "P-badges not read")}, r})
		}
	}
	return out
}

// ReviewKey is r's seen-set key: its reviewer's login and when it was
// submitted.
func ReviewKey(r github.Review) string {
	return r.Login + " " + r.Submitted.UTC().Format(time.RFC3339Nano)
}

// GitHubReviewed is r with its line from d, as the review query read it:
// the commit reviewed, the state and the P-badge counts, P0 first.
func GitHubReviewed(r GitHubReview, d github.ReviewDetail) GitHubEvent {
	var counts []string
	for level, n := range d.Badges {
		if n > 0 {
			counts = append(counts, fmt.Sprintf("P%d ×%d", level, n))
		}
	}
	found := "no P-badges"
	if len(counts) > 0 {
		found = strings.Join(counts, ", ")
	}
	if d.Read < d.Comments {
		found += fmt.Sprintf(" (first %d of %d comments)", d.Read, d.Comments)
	}
	e := r.GitHubEvent
	e.Text = reviewText(r.PR, d.Review, d.Commit, found)
	return e
}

// reviewStates are the words a review's line says its state in.
var reviewStates = map[string]string{
	github.ReviewApproved:         "approved",
	github.ReviewChangesRequested: "changes requested",
	github.ReviewCommented:        "commented",
	github.ReviewDismissed:        "dismissed",
	"":                            "reviewed",
}

// reviewText is "#50 review by alice at 1e0fb1b: approved, P1 ×2": a login
// that is not one is "a reviewer", and a commit not read is left out.
func reviewText(n int, r github.Review, commit, found string) string {
	who := r.Login
	if who == "" {
		who = "a reviewer"
	}
	text := fmt.Sprintf("#%d review by %s", n, who)
	if commit != "" {
		text += " at " + short(commit)
	}
	return text + ": " + reviewStates[r.State] + ", " + found
}

// short is an oid's first 7 characters, as git abbreviates it.
func short(oid string) string { return oid[:min(7, len(oid))] }

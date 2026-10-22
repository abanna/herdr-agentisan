package notice

import (
	"fmt"
	"strings"

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

// short is an oid's first 7 characters, as git abbreviates it.
func short(oid string) string { return oid[:min(7, len(oid))] }

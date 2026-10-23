package dashboard

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// TabBarWidth is the most of a line herdr's tab bar shows: herdr 0.9.3 keeps
// the first 80 characters, not cells, of a command's last line, and cuts the
// rest with no "…" (MAX_STATUS_TEXT_CHARS, src/app/tab_bar_status.rs).
const TabBarWidth = 80

// TabBarBudget is how long the tab-bar line waits for the daemon by
// default: under herdr's default timeout_seconds of 2, so the line is
// printed before herdr kills the command and blanks its entry.
const TabBarBudget = time.Second

// The tab-bar line for a daemon that gave no snapshot (ADR-001 A41). herdr
// blanks a command's entry when it exits non-zero or outlives its timeout,
// so the line says what went wrong instead.
const (
	// TabBarDown is the line when no daemon answers at the socket.
	TabBarDown = "agentisan: daemon down"
	// TabBarStarting is the line before the daemon's first team poll.
	TabBarStarting = "agentisan: starting"
	// TabBarNotAnswering is the line when the daemon took longer than the
	// command's budget.
	TabBarNotAnswering = "agentisan: not answering"
	// TabBarError is the line for any other failure: a refusal, or an
	// answer that breaks the protocol or the snapshot contract.
	TabBarError = "agentisan: daemon error"
)

// TabBar is the tab-bar line of the team sel selects, from the daemon at
// socket, within ctx (ADR-001 A41): TabBarLine of its snapshot, nothing for
// a workspace that is no team's, or one of the TabBar lines above. The
// snapshot op answers from the daemon's cache, so this never calls herdr or
// gh.
func TabBar(ctx context.Context, socket string, sel daemon.Selector) string {
	snap, err := daemon.SnapshotOf(ctx, socket, sel)
	switch {
	case err == nil:
		return TabBarLine(snap)
	case errors.Is(err, daemon.ErrNoTeam):
		return ""
	case errors.Is(err, daemon.ErrNotReady):
		return TabBarStarting
	case errors.Is(err, context.DeadlineExceeded):
		return TabBarNotAnswering
	case errors.Is(err, daemon.ErrUnavailable):
		return TabBarDown
	default:
		return TabBarError
	}
}

// TabBarLine is herdr-status.sh's line for s: "PRs #878⏳ #886✓ · slots 2/2 ·
// issues 14", each PR with the header's one mark (prMark), at most
// TabBarWidth characters. The slots read as the header's (slotsText), a
// held slot included. The PRs give way first, to a count, so the slots
// and the issues always show. The board's unread badge, "board 3", leads
// when anything is unread (A60), so herdr's cut never drops it; with none
// the line is as it was before the board. What the snapshot does not report
// is left out, never shown as zero, and a snapshot that reports nothing is
// "", which hides the entry. The line names no team: it follows the focused
// workspace, whose team the tab bar already shows.
func TabBarLine(s snapshot.Snapshot) string {
	p := s.Project
	if p == nil {
		return ""
	}
	var head []string
	if b := boardBadge(p); b != "" {
		head = append(head, b)
	}
	var tail []string
	if p.Slots != nil {
		tail = append(tail, slotsText(*p.Slots))
	}
	if p.Issues != nil {
		tail = append(tail, "issues "+strconv.Itoa(*p.Issues))
	}
	var line string
	for _, d := range []prDetail{prCompact, prCount} {
		parts := slices.Clone(head)
		if prs := tabBarPRs(p.PRs, d); prs != "" {
			parts = append(parts, prs)
		}
		parts = append(parts, tail...)
		line = strings.Join(parts, sep)
		if utf8.RuneCountInString(line) <= TabBarWidth {
			return line
		}
	}
	return cutRunes(line, TabBarWidth)
}

// tabBarPRs is the PRs at detail d, prCompact or prCount; "" when they are
// not reported.
func tabBarPRs(prs []snapshot.PR, d prDetail) string {
	switch {
	case prs == nil:
		return ""
	case len(prs) == 0:
		return "PRs none"
	case d == prCount:
		return "PRs " + strconv.Itoa(len(prs))
	}
	var b strings.Builder
	b.WriteString("PRs")
	for _, pr := range prs {
		b.WriteString(" #" + strconv.Itoa(pr.Number) + prMark(pr).text)
	}
	return b.String()
}

// cutRunes is s cut to at most n characters, ending in "…" when cut.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

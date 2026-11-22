package team

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The daemon writes `$team` to each team's workspace on every poll.
const (
	// Key is the token's name.
	Key = "team"
	// TTL is three times the 3 s poll. Every push renews it, so a stopped
	// or crashed daemon's tokens are gone within about 9 s; nothing clears
	// them on exit.
	TTL = 9 * time.Second
)

// Counts is what `$team` shows for one team.
type Counts struct {
	// Panes counts every pane, plain shells and unknown statuses included.
	Panes   int
	Working int
	Blocked int
	// Ready counts the panes carrying a handoff token, whatever their
	// status.
	Ready int
}

// String is the format ADR-001 A28 gives `$team`, in place of A17's frozen
// v12 glyphs: "N agents · W working · R ready", then " · B blocked" when
// any are, and "1 agent" for one. Idle and done are not counted.
func (c Counts) String() string {
	noun := "agents"
	if c.Panes == 1 {
		noun = "agent"
	}
	s := fmt.Sprintf("%d %s · %d working · %d ready", c.Panes, noun, c.Working, c.Ready)
	if c.Blocked > 0 {
		s += fmt.Sprintf(" · %d blocked", c.Blocked)
	}
	return s
}

// Count counts workers for `$team`.
func Count(workers []Worker) Counts {
	c := Counts{Panes: len(workers)}
	for _, w := range workers {
		switch w.Pane.Status {
		case StatusWorking:
			c.Working++
		case StatusBlocked:
			c.Blocked++
		case StatusIdle, StatusDone, StatusUnknown:
		}
		if w.Pane.Handoff {
			c.Ready++
		}
	}
	return c
}

// Token is one `$team` value and the target it goes to.
type Token struct {
	Target Target
	Value  string
}

// Tokens derives one `$team` token per workspace from each team's workers.
func Tokens(m Model) []Token {
	var out []Token
	for _, pr := range m.Projects {
		if pr.Target == (Target{}) {
			continue
		}
		var workers []Worker
		for _, g := range pr.Groups {
			workers = append(workers, g.Workers...)
		}
		out = append(out, Token{Target: pr.Target, Value: Count(workers).String()})
	}
	slices.SortFunc(out, func(a, b Token) int { return strings.Compare(a.Target.WorkspaceID, b.Target.WorkspaceID) })
	return out
}

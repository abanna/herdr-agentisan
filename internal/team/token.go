package team

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The `$team` token (ADR-001 token contract): written by the daemon to every
// group target on every poll, with source agentisan.
const (
	// Key is the token's name.
	Key = "team"
	// TTL is three times the 3 s poll. Every push renews it, so a stopped
	// or crashed daemon's tokens are gone within about 9 s; nothing clears
	// them on exit.
	TTL = 9 * time.Second
)

// Counts is what `$team` shows for one group target.
type Counts struct {
	// Panes counts every pane, plain shells and unknown statuses included.
	Panes   int
	Working int
	// Idle counts idle and done.
	Idle    int
	Blocked int
	// Ready counts the panes carrying a handoff token, whatever their
	// status.
	Ready int
}

// String is the frozen format, byte for byte what herdr-dashboard-v12.py
// writes (v12:142-143): "N · ◐w ●i", then " ⚠b" and " ⟳r" when not zero.
func (c Counts) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d · ◐%d ●%d", c.Panes, c.Working, c.Idle)
	if c.Blocked > 0 {
		fmt.Fprintf(&b, " ⚠%d", c.Blocked)
	}
	if c.Ready > 0 {
		fmt.Fprintf(&b, " ⟳%d", c.Ready)
	}
	return b.String()
}

// Count counts workers for `$team`.
func Count(workers []Worker) Counts {
	c := Counts{Panes: len(workers)}
	for _, w := range workers {
		switch w.Pane.Status {
		case StatusWorking:
			c.Working++
		case StatusIdle, StatusDone:
			c.Idle++
		case StatusBlocked:
			c.Blocked++
		case StatusUnknown:
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

// Tokens derives `$team` for every group target in m, each from the workers
// sitting at that target, an empty one included. The boss's place gets none.
// They are sorted by target.
func Tokens(m Model) []Token {
	var out []Token
	for _, pr := range m.Projects {
		for _, g := range pr.Groups {
			for _, t := range g.Targets {
				var at []Worker
				for _, w := range g.Workers {
					if w.Target == t {
						at = append(at, w)
					}
				}
				out = append(out, Token{Target: t, Value: Count(at).String()})
			}
		}
	}
	slices.SortFunc(out, func(a, b Token) int { return strings.Compare(a.Target.WorkspaceID, b.Target.WorkspaceID) })
	return out
}

package team

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// Snapshot is the team of project in m as the snapshot v1 contract describes
// it (ADR-001 D5, D6, A18), built at at, from that project's entry in m
// alone:
//
//   - one group per group of the project: those its Order names first, in
//     that order, then the rest by name, each with one row per worker in the
//     model's order (by target, then pane id). The order is the resolver's
//     (Spaces.GroupOrder for the ◆ spaces); the dashboard keeps it;
//   - the boss: the first agent pane, by pane id, in the boss's place, or
//     none when no agent runs there. The contract has one boss, and a shell
//     there (this dashboard's own pane, say) is not one. Its started_at is
//     when clock first saw that pane run an agent there (A31);
//   - the project's Color, when it has one, as its only reported state.
//
// A project with no entry in m, one herdr shows no workspace of, is its name
// and nothing else.
//
// Each row is named by agent.list's name, else the pane's label, else its
// pane id, as v12 names it; a row named any way but the first is Unnamed, so
// a jump goes by pane id. Its status is herdr's, "ready" when it carries a
// handoff token, and "unknown" for a plain shell. ctx is the token when it is
// a bare integer from 0 to 100 and left out otherwise; item and stage are
// the tokens; the stage's start comes from clock, which may be nil. The
// model is left out: the daemon does not know it.
func Snapshot(m Model, project string, at time.Time, clock *StageClock) snapshot.Snapshot {
	s := snapshot.Snapshot{V: snapshot.Version, At: at.UTC(), Team: project, Groups: []snapshot.Group{}}
	i := slices.IndexFunc(m.Projects, func(p Project) bool { return p.Name == project })
	if i < 0 {
		return s
	}
	pr := m.Projects[i]
	if pr.Color != "" {
		s.Project = &snapshot.Project{Color: pr.Color}
	}
	for _, w := range pr.Boss {
		if w.Pane.IsAgent() {
			s.Boss = &snapshot.Boss{Agent: row(w.Pane, clock)}
			if since, ok := clock.BossSince(w.Pane.ID); ok {
				since = since.UTC()
				s.Boss.StartedAt = &since
			}
			break
		}
	}
	groups := slices.Clone(pr.Groups)
	slices.SortStableFunc(groups, func(a, b Group) int {
		ra, rb := rank(pr.Order, a.Name), rank(pr.Order, b.Name)
		return cmp.Or(cmp.Compare(ra, rb), strings.Compare(a.Name, b.Name))
	})
	for _, g := range groups {
		out := snapshot.Group{Name: g.Name, Agents: make([]snapshot.Agent, 0, len(g.Workers))}
		for _, w := range g.Workers {
			out.Agents = append(out.Agents, row(w.Pane, clock))
		}
		s.Groups = append(s.Groups, out)
	}
	return s
}

// rank is name's place in order, matched exactly, or past its end.
func rank(order []string, name string) int {
	if i := slices.Index(order, name); i >= 0 {
		return i
	}
	return len(order)
}

// row is one pane as a snapshot row.
func row(p Pane, clock *StageClock) snapshot.Agent {
	a := snapshot.Agent{Name: p.Name, PaneID: p.ID, Status: rowStatus(p), Item: p.Item, Stage: p.Stage}
	if a.Name == "" {
		a.Unnamed = true
		a.Name = p.ID
		if strings.TrimSpace(p.Label) != "" {
			a.Name = p.Label
		}
	}
	if n, ok := ParseCtx(p.Ctx); ok {
		a.Ctx = &n
	}
	if since, ok := clock.Since(p.ID, p.Stage); ok {
		since = since.UTC()
		a.StageStartedAt = &since
	}
	return a
}

// rowStatus is the contract's status for p.
func rowStatus(p Pane) snapshot.Status {
	switch {
	case p.Handoff:
		return snapshot.StatusReady
	case !p.IsAgent():
		return snapshot.StatusUnknown
	}
	switch p.Status {
	case StatusWorking:
		return snapshot.StatusWorking
	case StatusIdle:
		return snapshot.StatusIdle
	case StatusDone:
		return snapshot.StatusDone
	case StatusBlocked:
		return snapshot.StatusBlocked
	case StatusUnknown:
		return snapshot.StatusUnknown
	default:
		return snapshot.StatusUnknown
	}
}

// ParseCtx reads a ctx token as `report statusline` writes it: a bare
// integer from 0 to snapshot.MaxCtx in its canonical spelling, with no sign,
// space or leading zero. Anything else is not a ctx.
func ParseCtx(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > snapshot.MaxCtx || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

// StageClock remembers, for each pane, the stage it holds and when the
// daemon first saw it there: stage_started_at (ADR-001 A18). For each agent
// pane in a boss's place it also remembers when the daemon first saw it
// there: the boss's started_at, its runtime's start (A31). It is held in
// memory only, so a restarted daemon starts it fresh. The zero value is
// ready to use; a nil clock knows no start. It is not safe for concurrent
// use: the team poll alone observes it.
type StageClock struct {
	seen map[string]stageSeen
	boss map[string]time.Time
}

type stageSeen struct {
	stage string
	since time.Time
}

// Observe records the stages of every pane in m at now: a pane whose stage
// is new, or changed, starts at now; one whose stage is unchanged keeps its
// start. A pane that is not in m, or holds no stage, is forgotten, so the
// same stage seen again later starts again. Likewise an agent pane in a
// boss's place starts at now when it is new there, and keeps its start
// while it stays; one that leaves that place, or stops running an agent, is
// forgotten.
func (c *StageClock) Observe(m Model, now time.Time) {
	if c == nil {
		return
	}
	next := map[string]stageSeen{}
	boss := map[string]time.Time{}
	note := func(p Pane) {
		if p.Stage == "" {
			return
		}
		if prev, ok := c.seen[p.ID]; ok && prev.stage == p.Stage {
			next[p.ID] = prev
			return
		}
		next[p.ID] = stageSeen{stage: p.Stage, since: now}
	}
	for _, pr := range m.Projects {
		for _, w := range pr.Boss {
			note(w.Pane)
			if !w.Pane.IsAgent() {
				continue
			}
			if since, ok := c.boss[w.Pane.ID]; ok {
				boss[w.Pane.ID] = since
			} else {
				boss[w.Pane.ID] = now
			}
		}
		for _, g := range pr.Groups {
			for _, w := range g.Workers {
				note(w.Pane)
			}
		}
	}
	c.seen, c.boss = next, boss
}

// BossSince is when the pane paneID was first seen running an agent in a
// boss's place, and false when the clock does not know it there.
func (c *StageClock) BossSince(paneID string) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	since, ok := c.boss[paneID]
	return since, ok
}

// Since is when the pane paneID was first seen at stage, and false when the
// clock does not know it there.
func (c *StageClock) Since(paneID, stage string) (time.Time, bool) {
	if c == nil || stage == "" {
		return time.Time{}, false
	}
	s, ok := c.seen[paneID]
	if !ok || s.stage != stage {
		return time.Time{}, false
	}
	return s.since, true
}

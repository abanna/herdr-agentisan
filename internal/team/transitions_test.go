package team_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// with returns liveLayout with each pane in status set to its status, the
// panes in drop removed, and the panes in add appended.
func with(status map[string]string, drop []string, add ...herdr.PaneInfo) team.Layout {
	l := liveLayout()
	var panes []herdr.PaneInfo
	for _, pane := range l.Panes {
		if s, ok := status[pane.PaneID]; ok {
			pane.AgentStatus = s
		}
		keep := true
		for _, d := range drop {
			keep = keep && pane.PaneID != d
		}
		if keep {
			panes = append(panes, pane)
		}
	}
	l.Panes = append(panes, add...)
	return l
}

// TestTransitions is the status diff D10's events come from. It is keyed by
// pane id; the logical id rides along and is never compared.
func TestTransitions(t *testing.T) {
	t.Parallel()
	spaces := team.Spaces{Project: "agentisan"}
	rows := []store.Worker{
		{LogicalID: "boss", PaneID: "wJ:p1", TerminalID: "term_wJ:p1"},
		{LogicalID: "coders-1", PaneID: "wN:p1", TerminalID: "term_wN:p1"},
	}
	build := func(l team.Layout, rows []store.Worker) team.Model { return team.Build(l, spaces, rows) }
	before := build(liveLayout(), rows)
	wN := team.Target{WorkspaceID: "wN"}
	coder1 := func(status team.Status) team.Worker {
		return team.Worker{ID: "coders-1", Bound: true, Target: wN, Pane: team.Pane{ID: "wN:p1", TerminalID: "term_wN:p1", Agent: "claude", Status: status}}
	}

	tests := map[string]struct {
		prev, next team.Model
		want       []team.Transition
	}{
		"nothing changed": {prev: before, next: build(liveLayout(), rows)},
		"a bound worker's status changes": {
			prev: before, next: build(with(map[string]string{"wN:p1": "blocked"}, nil), rows),
			want: []team.Transition{{Project: "agentisan", Group: "coders", Worker: coder1(team.StatusBlocked), From: "working", To: "blocked"}},
		},
		"the boss's status changes": {
			prev: before, next: build(with(map[string]string{"wJ:p1": "idle"}, nil), rows),
			want: []team.Transition{{
				Project: "agentisan", Boss: true,
				Worker: team.Worker{ID: "boss", Bound: true, Pane: team.Pane{ID: "wJ:p1", TerminalID: "term_wJ:p1", Agent: "claude", Status: team.StatusIdle}},
				From:   "working", To: "idle",
			}},
		},
		"a pane appears": {
			prev: before, next: build(with(nil, nil, p("wY:p2", "wY", "working")), rows),
			want: []team.Transition{{
				Project: "agentisan", Group: "qa",
				Worker: team.Worker{ID: "wY:p2", Target: team.Target{WorkspaceID: "wY"}, Pane: team.Pane{ID: "wY:p2", TerminalID: "term_wY:p2", Agent: "claude", Status: team.StatusWorking}},
				From:   team.Absent, To: "working",
			}},
		},
		"a pane leaves, as the older model had it": {
			prev: before, next: build(with(nil, []string{"wN:p1"}), rows),
			want: []team.Transition{{Project: "agentisan", Group: "coders", Worker: coder1(team.StatusWorking), From: "working", To: team.Absent}},
		},
		"a rotation: the new pane appears and the old leaves, under one logical id": {
			prev: before,
			next: build(with(nil, []string{"wN:p1"}, p("wN:p9", "wN", "working")), []store.Worker{{LogicalID: "boss", PaneID: "wJ:p1", TerminalID: "term_wJ:p1"}, {LogicalID: "coders-1", PaneID: "wN:p9", TerminalID: "term_wN:p9"}}),
			want: []team.Transition{
				{Project: "agentisan", Group: "coders", Worker: team.Worker{ID: "coders-1", Bound: true, Target: wN, Pane: team.Pane{ID: "wN:p9", TerminalID: "term_wN:p9", Agent: "claude", Status: team.StatusWorking}}, From: team.Absent, To: "working"},
				{Project: "agentisan", Group: "coders", Worker: coder1(team.StatusWorking), From: "working", To: team.Absent},
			},
		},
		"a poll that read no workers rows: ids turn provisional, statuses do not change": {
			prev: before, next: build(liveLayout(), nil),
		},
		"a pane that moves group keeping its status": {
			prev: before,
			next: build(with(nil, []string{"wY:p1"}, p("wY:p1", "wN", "idle")), rows),
		},
		"from nothing, every pane appears: why the daemon never diffs against a model it did not build": {
			prev: team.Model{}, next: build(team.Layout{Workspaces: liveLayout().Workspaces, Panes: []herdr.PaneInfo{p("wY:p1", "wY", "idle")}}, nil),
			want: []team.Transition{{
				Project: "agentisan", Group: "qa",
				Worker: team.Worker{ID: "wY:p1", Target: team.Target{WorkspaceID: "wY"}, Pane: team.Pane{ID: "wY:p1", TerminalID: "term_wY:p1", Agent: "claude", Status: team.StatusIdle}},
				From:   team.Absent, To: "idle",
			}},
		},
		"both empty": {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, team.Transitions(tc.prev, tc.next))
		})
	}
}

// TestTransitionsOrder: the newer model's changes come first, in model
// order, then the panes that left, in the older model's order, so a batch
// of events reads the same on every run.
func TestTransitionsOrder(t *testing.T) {
	t.Parallel()
	spaces := team.Spaces{Project: "agentisan"}
	prev := team.Build(liveLayout(), spaces, nil)
	next := team.Build(with(map[string]string{"wY:p1": "working", "wJ:p1": "done"}, []string{"wN:p2", "wN:p1"}), spaces, nil)

	var got []string
	for _, tr := range team.Transitions(prev, next) {
		got = append(got, tr.Worker.Pane.ID+":"+string(tr.From)+">"+string(tr.To))
	}
	assert.Equal(t, []string{"wJ:p1:working>done", "wY:p1:idle>working", "wN:p1:working>absent", "wN:p2:done>absent"}, got)
}

// TestTransitionsReportAPaneOnce: a model that has one pane in two places,
// which no resolver today produces, still gives that pane one transition,
// in the first place it is.
func TestTransitionsReportAPaneOnce(t *testing.T) {
	t.Parallel()
	pane := func(s team.Status) team.Pane { return team.Pane{ID: "wN:p1", Status: s} }
	model := func(s team.Status) team.Model {
		return team.Model{Projects: []team.Project{{Name: "a", Groups: []team.Group{
			{Name: "coders", Workers: []team.Worker{{ID: "wN:p1", Pane: pane(s)}}},
			{Name: "qa", Workers: []team.Worker{{ID: "wN:p1", Pane: pane(s)}}},
		}}}}
	}

	got := team.Transitions(model(team.StatusIdle), model(team.StatusWorking))
	assert.Equal(t, []team.Transition{{Project: "a", Group: "coders", Worker: team.Worker{ID: "wN:p1", Pane: pane(team.StatusWorking)}, From: "idle", To: "working"}}, got)

	gone := team.Transitions(model(team.StatusIdle), team.Model{})
	assert.Equal(t, []team.Transition{{Project: "a", Group: "coders", Worker: team.Worker{ID: "wN:p1", Pane: pane(team.StatusIdle)}, From: "idle", To: team.Absent}}, gone)
}

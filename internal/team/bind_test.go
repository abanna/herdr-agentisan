package team_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// bindLayout is a tabs team's workspace after a live handoff renewed every
// terminal: the coder's tab holds an agent pane and a shell, and herdr still
// names one agent.
func bindLayout() team.Layout {
	return team.Layout{
		Panes: []herdr.PaneInfo{
			{PaneID: "w14:p1", TerminalID: "term_n1", TabID: "w14:t1", Agent: "claude"},
			{PaneID: "w14:p2", TerminalID: "term_n2", TabID: "w14:t2", Agent: "claude"},
			{PaneID: "w14:p3", TerminalID: "term_n3", TabID: "w14:t2"},
			{PaneID: "w14:p4", TerminalID: "term_n4", TabID: "w14:t3", Agent: "codex"},
			{PaneID: "w14:p5", TabID: "w14:t3", Agent: "codex"},
			{PaneID: "w14:p6", TerminalID: "term_n6", TabID: "w14:t3", Agent: "codex"},
		},
		Agents: []herdr.AgentInfo{{PaneID: "w14:p4", Name: "ha-coder-1"}},
		Tabs: []herdr.TabInfo{
			{TabID: "w14:t1", Label: "ha-boss"}, {TabID: "w14:t2", Label: "ha-coder-1"}, {TabID: "w14:t3", Label: "ha-qa-1"},
		},
	}
}

// TestReconcilePlansEachRow is A35's reconcile as a plan: a row bound where
// it was, shell and all, needs nothing; a moved one is rebound; a bound one
// with no shell has it read; one whose terminal is listed nowhere is proven
// against its candidates when it has a shell, in order and each once, and is
// lost when it has none, unless a handoff of it is pending. A lost row is
// the boss's.
func TestReconcilePlansEachRow(t *testing.T) {
	t.Parallel()
	coder := store.Worker{LogicalID: "ha-coder-1", PaneID: "w14:p2", TerminalID: "term_old", ShellPID: 4242, ShellStart: 7, Status: "active"}
	tests := map[string]struct {
		rows []store.Worker
		want []team.Check
	}{
		"bound where it was, with its shell": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w14:p1", TerminalID: "term_n1", ShellPID: 9, Status: "active"}},
		},
		"moved: the terminal under another pane id": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w2:p1", TerminalID: "term_n1", ShellPID: 9, Status: "active"}},
			want: []team.Check{{Row: store.Worker{LogicalID: "a", PaneID: "w2:p1", TerminalID: "term_n1", ShellPID: 9, Status: "active"}, Pane: "w14:p1"}},
		},
		"bound with no shell yet": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w14:p1", TerminalID: "term_n1", Status: "starting"}},
			want: []team.Check{{Row: store.Worker{LogicalID: "a", PaneID: "w14:p1", TerminalID: "term_n1", Status: "starting"}, Pane: "w14:p1"}},
		},
		// The hint, the pane herdr names the id, then the tab's agent
		// panes: the shell at w14:p3 runs no agent.
		"renewed terminals: the candidates in order": {
			rows: []store.Worker{coder},
			want: []team.Check{{Row: coder, Candidates: []herdr.PaneInfo{bindLayout().Panes[1], bindLayout().Panes[3]}}},
		},
		// w14:p5 has no terminal to re-key to, and w14:p6's is the boss's.
		"a candidate is listed once, has a terminal and is no live row's": {
			rows: []store.Worker{
				{LogicalID: "ha-boss", PaneID: "w14:p6", TerminalID: "term_n6", ShellPID: 1, Status: "active"},
				{LogicalID: "ha-qa-1", PaneID: "w14:p4", TerminalID: "term_gone", ShellPID: 5, Status: "active"},
				{LogicalID: "old", PaneID: "w14:p1", TerminalID: "term_n1", ShellPID: 2, Status: store.StatusLost},
			},
			want: []team.Check{{
				Row:        store.Worker{LogicalID: "ha-qa-1", PaneID: "w14:p4", TerminalID: "term_gone", ShellPID: 5, Status: "active"},
				Candidates: []herdr.PaneInfo{bindLayout().Panes[3]},
			}},
		},
		"a shell to prove and no candidate: its pane closed": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w9:p9", TerminalID: "term_old", ShellPID: 3, Status: "active"}},
			want: []team.Check{{Row: store.Worker{LogicalID: "a", PaneID: "w9:p9", TerminalID: "term_old", ShellPID: 3, Status: "active"}}},
		},
		"renewed terminals and no shell to prove: lost": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w14:p1", TerminalID: "term_old", Status: "active"}},
			want: []team.Check{{Row: store.Worker{LogicalID: "a", PaneID: "w14:p1", TerminalID: "term_old", Status: "active"}}},
		},
		"a row from before A35, with no terminal: lost": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w14:p1", Status: "active"}},
			want: []team.Check{{Row: store.Worker{LogicalID: "a", PaneID: "w14:p1", Status: "active"}}},
		},
		"nothing to prove while a handoff is pending": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w14:p1", TerminalID: "term_old", Status: "active", HandoffPending: true}},
		},
		"a lost row is the boss's": {
			rows: []store.Worker{{LogicalID: "a", PaneID: "w14:p1", TerminalID: "term_old", Status: store.StatusLost}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, team.Reconcile(bindLayout(), tc.rows))
		})
	}
}

// TestCheckVerdicts: what the poll does with a check.
func TestCheckVerdicts(t *testing.T) {
	t.Parallel()
	row := store.Worker{PaneID: "w1:p1", ShellPID: 7}
	moved := team.Check{Row: row, Pane: "w2:p1"}
	assert.True(t, moved.Moved())
	assert.False(t, moved.NeedsShell(), "it has its shell")
	assert.False(t, moved.Lost())

	bare := team.Check{Row: store.Worker{PaneID: "w1:p1"}, Pane: "w1:p1"}
	assert.False(t, bare.Moved())
	assert.True(t, bare.NeedsShell())

	assert.False(t, team.Check{Row: row}.Lost(), "a shell to prove first")
	assert.True(t, team.Check{Row: store.Worker{PaneID: "w1:p1"}}.Lost())
	assert.False(t, team.Check{Row: store.Worker{PaneID: "w1:p1", HandoffPending: true}}.Lost(), "the handoff's to settle")
}

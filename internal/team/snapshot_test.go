package team_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// at is the instant every snapshot test builds at.
var at = time.Date(2026, 10, 10, 9, 30, 0, 0, time.UTC)

// spacesOrder is the ◆ spaces resolver's group order, as the daemon passes it.
var spacesOrder = team.Spaces{}.GroupOrder()

// pane is a pane in ws carrying tokens: an agent of kind agent ("" for a
// plain shell) at status.
func pane(id, ws, agent, status string, tokens map[string]string) herdr.PaneInfo {
	return herdr.PaneInfo{PaneID: id, WorkspaceID: ws, Agent: agent, AgentStatus: status, Tokens: tokens}
}

// oneRow builds the model of one pane in ◆ coders, named name by agent.list
// when name is not "", and returns the row the snapshot gives it.
func oneRow(t *testing.T, p herdr.PaneInfo, name string) snapshot.Agent {
	t.Helper()
	p.WorkspaceID = "wN"
	l := team.Layout{Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}}, Panes: []herdr.PaneInfo{p}}
	if name != "" {
		l.Agents = []herdr.AgentInfo{{PaneID: p.PaneID, Name: name}}
	}
	s := team.Snapshot(team.Build(l, team.Spaces{}, nil), "", "", spacesOrder, at, nil)
	require.Len(t, s.Groups, 1)
	require.Len(t, s.Groups[0].Agents, 1)
	return s.Groups[0].Agents[0]
}

//go:fix inline

// TestSnapshotRowNames: a row is named as v12 names it
// (herdr-dashboard-v12.py:180-205): agent.list's name, else the pane's
// label, else its pane id. Only an agent.list name is one agent.focus takes
// (herdr 0.9.3 src/app/terminal_targets.rs:74-104), so every other row is
// marked unnamed and the dashboard jumps to it by pane id.
func TestSnapshotRowNames(t *testing.T) {
	t.Parallel()

	labelled := func(p herdr.PaneInfo, label string) herdr.PaneInfo { p.Label = label; return p }
	tests := map[string]struct {
		pane herdr.PaneInfo
		name string
		want snapshot.Agent
	}{
		"a named agent": {
			pane: pane("wN:p1", "", "claude", "working", nil), name: "pee01",
			want: snapshot.Agent{Name: "pee01", PaneID: "wN:p1", Status: snapshot.StatusWorking},
		},
		"a name beats a label": {
			pane: labelled(pane("wN:p1", "", "claude", "working", nil), "notes"), name: "pee01",
			want: snapshot.Agent{Name: "pee01", PaneID: "wN:p1", Status: snapshot.StatusWorking},
		},
		"an unnamed agent with a label": {
			pane: labelled(pane("wN:p2", "", "claude", "idle", nil), "scratch"),
			want: snapshot.Agent{Name: "scratch", Unnamed: true, PaneID: "wN:p2", Status: snapshot.StatusIdle},
		},
		"an unnamed agent without a label": {
			pane: pane("wN:p2", "", "codex", "idle", nil),
			want: snapshot.Agent{Name: "wN:p2", Unnamed: true, PaneID: "wN:p2", Status: snapshot.StatusIdle},
		},
		"a labelled shell": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "notes"),
			want: snapshot.Agent{Name: "notes", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"an unlabelled shell": {
			pane: pane("wN:p3", "", "", "unknown", nil),
			want: snapshot.Agent{Name: "wN:p3", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// A label of white space alone would draw as a blank name.
		"a blank label is no label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), " \t"),
			want: snapshot.Agent{Name: "wN:p3", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// The label is kept as the user wrote it; the dashboard makes text
		// safe to draw.
		"a non-ASCII label with a control character": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "メモ\x1b[2J"),
			want: snapshot.Agent{Name: "メモ\x1b[2J", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// Nothing normalises, trims or re-encodes a label: the bytes the
		// user gave herdr are the name. Encoding the snapshot as JSON makes
		// invalid UTF-8 valid (TestSnapshotDecodesStrictly).
		"a label with invalid UTF-8": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "no\xfftes"),
			want: snapshot.Agent{Name: "no\xfftes", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a label with a truncated multibyte": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "notes\xe2\x82"),
			want: snapshot.Agent{Name: "notes\xe2\x82", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a NUL in a label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "no\x00tes"),
			want: snapshot.Agent{Name: "no\x00tes", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a byte-order mark before a label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "\xef\xbb\xbfnotes"),
			want: snapshot.Agent{Name: "\xef\xbb\xbfnotes", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"an NFD label is kept as written": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "cafe\xcc\x81"),
			want: snapshot.Agent{Name: "cafe\xcc\x81", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"an option-like label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "--help"),
			want: snapshot.Agent{Name: "--help", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a label with a line break": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "a\r\nb"),
			want: snapshot.Agent{Name: "a\r\nb", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		"a label of line breaks alone is no label": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "\r\n"),
			want: snapshot.Agent{Name: "wN:p3", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
		// A label equal to another agent's name is still a label: the row
		// is unnamed, so a jump goes by its own pane id.
		"a label that looks like an agent name": {
			pane: labelled(pane("wN:p3", "", "", "unknown", nil), "pee01"),
			want: snapshot.Agent{Name: "pee01", Unnamed: true, PaneID: "wN:p3", Status: snapshot.StatusUnknown},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, oneRow(t, tc.pane, tc.name))
		})
	}
}

// TestSnapshotRowStatus: herdr's agent_status, mapped one to one onto the
// contract's; "ready" for a pane carrying a non-empty handoff token,
// whatever its status; and "unknown", the contract's status for a pane with
// no agent, for a plain shell.
func TestSnapshotRowStatus(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pane herdr.PaneInfo
		want snapshot.Status
	}{
		"working":                   {pane: pane("wN:p1", "", "claude", "working", nil), want: snapshot.StatusWorking},
		"idle":                      {pane: pane("wN:p1", "", "claude", "idle", nil), want: snapshot.StatusIdle},
		"done":                      {pane: pane("wN:p1", "", "claude", "done", nil), want: snapshot.StatusDone},
		"blocked":                   {pane: pane("wN:p1", "", "claude", "blocked", nil), want: snapshot.StatusBlocked},
		"unknown":                   {pane: pane("wN:p1", "", "claude", "unknown", nil), want: snapshot.StatusUnknown},
		"a status from a newer one": {pane: pane("wN:p1", "", "claude", "napping", nil), want: snapshot.StatusUnknown},
		"no status at all":          {pane: pane("wN:p1", "", "claude", "", nil), want: snapshot.StatusUnknown},
		"a status in another case":  {pane: pane("wN:p1", "", "claude", "Working", nil), want: snapshot.StatusUnknown},
		"a handoff token":           {pane: pane("wN:p1", "", "claude", "done", map[string]string{"handoff": "⟳ READY TO RESTART"}), want: snapshot.StatusReady},
		"a handoff while working":   {pane: pane("wN:p1", "", "claude", "working", map[string]string{"handoff": "x"}), want: snapshot.StatusReady},
		"an empty handoff token":    {pane: pane("wN:p1", "", "claude", "idle", map[string]string{"handoff": ""}), want: snapshot.StatusIdle},
		"a shell":                   {pane: pane("wN:p1", "", "", "unknown", nil), want: snapshot.StatusUnknown},
		// herdr reports a status for every pane; a shell's is not an
		// agent's, whatever it says.
		"a shell herdr calls idle": {pane: pane("wN:p1", "", "", "idle", nil), want: snapshot.StatusUnknown},
		"a shell with a handoff":   {pane: pane("wN:p1", "", "", "unknown", map[string]string{"handoff": "x"}), want: snapshot.StatusReady},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, oneRow(t, tc.pane, "").Status)
		})
	}
	t.Run("a named agent herdr detects no kind of", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, snapshot.StatusWorking, oneRow(t, pane("wN:p1", "", "", "working", nil), "pee01").Status,
			"agent.list names only agent terminals, so a name makes the pane an agent")
	})
}

// TestSnapshotRowCtx: ctx is the token as `report statusline` writes it, a
// bare integer from 0 to 100 (ADR-001 token contract). Anything else, from
// another writer or a stale format, is left out rather than guessed at.
func TestSnapshotRowCtx(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		ctx  string
		want *int
	}{
		"0":                        {ctx: "0", want: new(0)},
		"42":                       {ctx: "42", want: new(42)},
		"100, the ceiling":         {ctx: "100", want: new(100)},
		"101, one past it":         {ctx: "101", want: nil},
		"-1":                       {ctx: "-1", want: nil},
		"-0":                       {ctx: "-0", want: nil},
		"a plus sign":              {ctx: "+5", want: nil},
		"a leading zero":           {ctx: "07", want: nil},
		"surrounding space":        {ctx: " 5", want: nil},
		"a percent sign":           {ctx: "5%", want: nil},
		"a fraction":               {ctx: "42.5", want: nil},
		"empty":                    {ctx: "", want: nil},
		"not a number":             {ctx: "abc", want: nil},
		"past int64":               {ctx: "99999999999999999999", want: nil},
		"non-ASCII digits":         {ctx: "٤٢", want: nil},
		"a NUL inside":             {ctx: "4\x002", want: nil},
		"invalid UTF-8":            {ctx: "4\xff", want: nil},
		"a trailing newline":       {ctx: "42\n", want: nil},
		"a trailing CRLF":          {ctx: "42\r\n", want: nil},
		"a truncated multibyte":    {ctx: "4\xe2\x82", want: nil},
		"full-width digits":        {ctx: "４２", want: nil},
		"a byte-order mark before": {ctx: "\xef\xbb\xbf42", want: nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := oneRow(t, pane("wN:p1", "", "claude", "working", map[string]string{"ctx": tc.ctx}), "pee01")
			assert.Equal(t, tc.want, row.Ctx)
		})
	}
	t.Run("no ctx token", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, oneRow(t, pane("wN:p1", "", "claude", "working", nil), "pee01").Ctx)
	})
}

// TestSnapshotRowItemAndStage: item and stage are the tokens as `report
// stage` (or any writer) left them, at most 80 characters as herdr caps a
// token value (src/app/api_helpers.rs:208); an absent one is left out.
func TestSnapshotRowItemAndStage(t *testing.T) {
	t.Parallel()

	long := string(bytes.Repeat([]byte("x"), 80))
	tests := map[string]map[string]string{
		"both":                   {"item": "NERD-5255", "stage": "build_test"},
		"item only":              {"item": "NERD-5255"},
		"stage only":             {"stage": "review"},
		"neither":                {},
		"non-ASCII":              {"item": "チケット", "stage": "レビュー"},
		"control characters":     {"item": "a\x1b[31mb", "stage": "c\nd"},
		"herdr's longest values": {"item": long, "stage": long},
		"invalid UTF-8":          {"item": "a\xffb"},
	}
	for name, tokens := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := oneRow(t, pane("wN:p1", "", "claude", "working", tokens), "pee01")
			assert.Equal(t, tokens["item"], row.Item)
			assert.Equal(t, tokens["stage"], row.Stage)
			assert.Nil(t, row.StageStartedAt, "no clock: no start time")
		})
	}
}

// groupsLayout is one ◆ space per name, each with one agent, listed in the
// order given.
func groupsLayout(names ...string) team.Layout {
	var l team.Layout
	for i, n := range names {
		ws := "w" + string(rune('a'+i))
		l.Workspaces = append(l.Workspaces, herdr.WorkspaceInfo{WorkspaceID: ws, Label: "◆ " + n})
		l.Panes = append(l.Panes, pane(ws+":p1", ws, "claude", "idle", nil))
	}
	return l
}

func groupNames(s snapshot.Snapshot) []string {
	var out []string
	for _, g := range s.Groups {
		out = append(out, g.Name)
	}
	return out
}

// TestSnapshotGroupOrder: one group per resolver group, in the order the
// resolver gives, then the rest by name. The ◆ spaces resolver gives v12's
// working order (herdr-dashboard-v12.py:21, without ◆ herdr, which is not in
// the model), then debugger; another resolver, such as one whose groups are the user's
// worker types, gives its own, and the builder hard-codes none. Names match
// exactly, as the resolver matches them (A17): "Coders" is not coders.
func TestSnapshotGroupOrder(t *testing.T) {
	t.Parallel()

	types := []string{"coder", "qa", "precheck", "review", "research", "librarian", "debugger"}
	tests := map[string]struct {
		groups []string
		order  []string
		want   []string
	}{
		"v12's order":                 {groups: []string{"clerk", "research", "codex", "qa", "precheck", "coders"}, order: spacesOrder, want: []string{"coders", "precheck", "qa", "codex", "research", "clerk"}},
		"debugger after clerk":        {groups: []string{"debugger", "alpha", "clerk", "coders"}, order: spacesOrder, want: []string{"coders", "clerk", "debugger", "alpha"}},
		"the rest follow by name":     {groups: []string{"zeta", "beta", "qa", "alpha"}, order: spacesOrder, want: []string{"qa", "alpha", "beta", "zeta"}},
		"another case is another one": {groups: []string{"Coders", "coders", "QA"}, order: spacesOrder, want: []string{"coders", "Coders", "QA"}},
		"non-ASCII names":             {groups: []string{"ü", "a", "coders"}, order: spacesOrder, want: []string{"coders", "a", "ü"}},
		"a single group":              {groups: []string{"research"}, order: spacesOrder, want: []string{"research"}},
		"no group spaces":             {groups: nil, order: spacesOrder, want: nil},
		"a resolver's own order":      {groups: []string{"debugger", "coder", "librarian", "qa", "review", "research", "precheck"}, order: types, want: types},
		"no order: by name":           {groups: []string{"qa", "coders", "clerk"}, order: nil, want: []string{"clerk", "coders", "qa"}},
		"an order naming absent ones": {groups: []string{"research"}, order: types, want: []string{"research"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := team.Snapshot(team.Build(groupsLayout(tc.groups...), team.Spaces{}, nil), "", "", tc.order, at, nil)
			assert.Equal(t, tc.want, groupNames(s))
			assert.NotNil(t, s.Groups, "groups is a list, never null")
		})
	}
	t.Run("the spaces resolver's order is v12's then debugger, a fresh copy each time", func(t *testing.T) {
		t.Parallel()
		got := team.Spaces{}.GroupOrder()
		assert.Equal(t, []string{"coders", "precheck", "qa", "codex", "research", "clerk", "debugger"}, got)
		got[0] = "x"
		assert.Equal(t, "coders", team.Spaces{}.GroupOrder()[0])
	})
}

// TestSnapshotBoss: the boss is an agent pane in ◆ boss. The contract has
// one, so with several the first by pane id is it, and a shell there (this
// dashboard's own pane, say) never is. No agent in ◆ boss, or no ◆ boss,
// leaves the boss out: nothing the daemon does not know is invented.
func TestSnapshotBoss(t *testing.T) {
	t.Parallel()

	space := []herdr.WorkspaceInfo{{WorkspaceID: "wJ", Label: "◆ boss"}}
	tests := map[string]struct {
		workspaces []herdr.WorkspaceInfo
		panes      []herdr.PaneInfo
		agents     []herdr.AgentInfo
		want       *snapshot.Boss
	}{
		"one boss": {
			workspaces: space, panes: []herdr.PaneInfo{pane("wJ:p1", "wJ", "claude", "working", map[string]string{"ctx": "23", "item": "#867", "stage": "review"})},
			agents: []herdr.AgentInfo{{PaneID: "wJ:p1", Name: "boss"}},
			want:   &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "wJ:p1", Status: snapshot.StatusWorking, Ctx: new(23), Item: "#867", Stage: "review"}},
		},
		"the first agent pane by pane id, past a shell": {
			workspaces: space,
			panes:      []herdr.PaneInfo{pane("wJ:p3", "wJ", "claude", "idle", nil), pane("wJ:p1", "wJ", "", "unknown", nil), pane("wJ:p2", "wJ", "claude", "working", nil)},
			agents:     []herdr.AgentInfo{{PaneID: "wJ:p2", Name: "boss"}, {PaneID: "wJ:p3", Name: "boss-next"}},
			want:       &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "wJ:p2", Status: snapshot.StatusWorking}},
		},
		"an unnamed boss": {
			workspaces: space, panes: []herdr.PaneInfo{pane("wJ:p1", "wJ", "claude", "idle", nil)},
			want: &snapshot.Boss{Agent: snapshot.Agent{Name: "wJ:p1", Unnamed: true, PaneID: "wJ:p1", Status: snapshot.StatusIdle}},
		},
		"only shells in ◆ boss":   {workspaces: space, panes: []herdr.PaneInfo{pane("wJ:p1", "wJ", "", "unknown", nil)}},
		"an empty ◆ boss":         {workspaces: space},
		"no ◆ boss":               {workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}}, panes: []herdr.PaneInfo{pane("wN:p1", "wN", "claude", "idle", nil)}},
		"the boss is in no group": {workspaces: append(slices.Clone(space), herdr.WorkspaceInfo{WorkspaceID: "wN", Label: "◆ coders"}), panes: []herdr.PaneInfo{pane("wJ:p1", "wJ", "claude", "idle", nil)}, want: &snapshot.Boss{Agent: snapshot.Agent{Name: "wJ:p1", Unnamed: true, PaneID: "wJ:p1", Status: snapshot.StatusIdle}}},
		"two ◆ boss spaces, one boss": {
			workspaces: append(slices.Clone(space), herdr.WorkspaceInfo{WorkspaceID: "wK", Label: "◆ boss"}),
			panes:      []herdr.PaneInfo{pane("wK:p1", "wK", "claude", "idle", nil), pane("wJ:p1", "wJ", "claude", "working", nil)},
			agents:     []herdr.AgentInfo{{PaneID: "wJ:p1", Name: "boss"}, {PaneID: "wK:p1", Name: "boss2"}},
			want:       &snapshot.Boss{Agent: snapshot.Agent{Name: "boss", PaneID: "wJ:p1", Status: snapshot.StatusWorking}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := team.Layout{Workspaces: tc.workspaces, Panes: tc.panes, Agents: tc.agents}
			s := team.Snapshot(team.Build(l, team.Spaces{}, nil), "", "", spacesOrder, at, nil)
			assert.Equal(t, tc.want, s.Boss)
			for _, g := range s.Groups {
				for _, a := range g.Agents {
					assert.NotEqual(t, "wJ", a.PaneID[:2], "a boss-space pane is never a group's row")
				}
			}
		})
	}
}

// TestSnapshotProject: the team is the project the spaces belong to, by
// name, and its colour, when configured, is the project's only reported
// state: PRs, issues and slots stay unreported (NERD-5257 collects them).
// The unnamed project has no name and no colour.
func TestSnapshotProject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		project, color string
		want           *snapshot.Project
	}{
		"a project with a colour":    {project: "agentisan", color: "#7c3aed", want: &snapshot.Project{Color: "#7c3aed"}},
		"a project without a colour": {project: "agentisan"},
		"the unnamed project":        {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Build(groupsLayout("coders"), team.Spaces{Project: tc.project}, nil)
			s := team.Snapshot(m, tc.project, tc.color, spacesOrder, at, nil)
			assert.Equal(t, tc.project, s.Team)
			assert.Equal(t, tc.want, s.Project)
			assert.Equal(t, []string{"coders"}, groupNames(s))
		})
	}
	t.Run("another project's panes are not the team's", func(t *testing.T) {
		t.Parallel()
		m := team.Build(groupsLayout("coders"), team.Spaces{Project: "a"}, nil)
		s := team.Snapshot(m, "b", "", spacesOrder, at, nil)
		assert.Equal(t, "b", s.Team)
		assert.Empty(t, s.Groups)
		assert.Nil(t, s.Boss)
	})
}

// TestSnapshotDecodesStrictly: whatever the model holds, the snapshot it
// gives encodes to a document snapshot.Decode accepts byte for byte: a
// group space with no panes is a group with an empty agent list, never
// null, and no group spaces is an empty group list.
func TestSnapshotDecodesStrictly(t *testing.T) {
	t.Parallel()

	full := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{
			{WorkspaceID: "wJ", Label: "◆ boss"},
			{WorkspaceID: "wN", Label: "◆ coders"},
			{WorkspaceID: "w0", Label: "◆ research"},
			{WorkspaceID: "w2", Label: "notes"},
		},
		Panes: []herdr.PaneInfo{
			pane("wJ:p1", "wJ", "claude", "working", map[string]string{"ctx": "23", "stage": "review"}),
			pane("wN:p1", "wN", "claude", "working", map[string]string{"ctx": "40", "item": "NERD-1", "stage": "build"}),
			pane("wN:p2", "wN", "", "unknown", map[string]string{"ctx": "nope"}),
			{PaneID: "wN:p3", WorkspaceID: "wN", AgentStatus: "unknown", Label: "no\xfftes", Tokens: map[string]string{"item": "a\x00b", "stage": "c\xe2\x82"}},
			pane("w2:p1", "w2", "claude", "working", nil),
		},
		Agents: []herdr.AgentInfo{{PaneID: "wJ:p1", Name: "boss"}, {PaneID: "wN:p1", Name: "pee01"}},
	}
	clock := &team.StageClock{}
	m := team.Build(full, team.Spaces{Project: "agentisan"}, nil)
	clock.Observe(m, at)

	tests := map[string]snapshot.Snapshot{
		"a full team":      team.Snapshot(m, "agentisan", "#7c3aed", spacesOrder, at, clock),
		"no group spaces":  team.Snapshot(team.Model{}, "", "", spacesOrder, at, clock),
		"a nil clock":      team.Snapshot(m, "agentisan", "", spacesOrder, at, nil),
		"a local-time now": team.Snapshot(m, "agentisan", "", spacesOrder, at.In(time.FixedZone("x", -7*3600)), clock),
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(s)
			require.NoError(t, err)
			got, err := snapshot.Decode(bytes.NewReader(raw))
			require.NoError(t, err, "%s", raw)
			assert.Equal(t, snapshot.Version, got.V)
			assert.True(t, got.At.Equal(at))
			assert.NotContains(t, string(raw), `"groups":null`)
			assert.NotContains(t, string(raw), `"agents":null`)
		})
	}

	raw, err := json.Marshal(tests["a full team"])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `{"name":"research","agents":[]}`, "an empty group space is an empty list")
	assert.Contains(t, string(raw), `"at":"2026-10-10T09:30:00Z"`, "times are UTC")
}

// TestStageClock: stage_started_at is when the daemon first saw a pane's
// current stage (ADR-001 A18), keyed by pane id and value. The same value
// keeps its start, however many polls pass; a new value starts again; and a
// pane that drops its stage, or goes, is forgotten, so the same value later
// starts again. Held in memory only: a new clock, as after a daemon restart,
// starts fresh.
func TestStageClock(t *testing.T) {
	t.Parallel()

	l := func(stages map[string]string) team.Model {
		layout := team.Layout{Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "wJ", Label: "◆ boss"}, {WorkspaceID: "wN", Label: "◆ coders"}}}
		for _, id := range slices.Sorted(maps.Keys(stages)) {
			var tokens map[string]string
			if v := stages[id]; v != "-" {
				tokens = map[string]string{"stage": v}
			}
			layout.Panes = append(layout.Panes, pane(id, id[:2], "claude", "working", tokens))
		}
		return team.Build(layout, team.Spaces{}, nil)
	}
	t0, t1, t2, t3 := at, at.Add(3*time.Second), at.Add(6*time.Second), at.Add(9*time.Second)
	since := func(c *team.StageClock, id, stage string) time.Time {
		got, ok := c.Since(id, stage)
		if !ok {
			return time.Time{}
		}
		return got
	}

	t.Run("the same value keeps its start", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t1)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t2)
		assert.Equal(t, t0, since(c, "wN:p1", "build"))
	})
	t.Run("a new value resets it", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "review"}), t1)
		assert.Equal(t, t1, since(c, "wN:p1", "review"))
		_, ok := c.Since("wN:p1", "build")
		assert.False(t, ok, "the old value is gone")
	})
	t.Run("going back to an earlier value starts again", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "review"}), t1)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t2)
		assert.Equal(t, t2, since(c, "wN:p1", "build"))
	})
	t.Run("a dropped stage is forgotten", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p1": "-"}), t1)
		_, ok := c.Since("wN:p1", "build")
		assert.False(t, ok)
		c.Observe(l(map[string]string{"wN:p1": "build"}), t2)
		assert.Equal(t, t2, since(c, "wN:p1", "build"))
	})
	t.Run("a pane that goes is forgotten", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": "build", "wN:p2": "build"}), t0)
		c.Observe(l(map[string]string{"wN:p2": "build"}), t1)
		c.Observe(l(map[string]string{"wN:p1": "build", "wN:p2": "build"}), t2)
		assert.Equal(t, t2, since(c, "wN:p1", "build"), "back, it starts again")
		assert.Equal(t, t0, since(c, "wN:p2", "build"), "the other pane keeps its start")
	})
	t.Run("panes are timed apart, the boss's too", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wJ:p1": "plan"}), t0)
		c.Observe(l(map[string]string{"wJ:p1": "plan", "wN:p1": "plan"}), t3)
		assert.Equal(t, t0, since(c, "wJ:p1", "plan"))
		assert.Equal(t, t3, since(c, "wN:p1", "plan"))
	})
	t.Run("an empty stage has no start", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		c.Observe(l(map[string]string{"wN:p1": ""}), t0)
		_, ok := c.Since("wN:p1", "")
		assert.False(t, ok)
	})
	t.Run("a restart starts fresh", func(t *testing.T) {
		t.Parallel()
		before := &team.StageClock{}
		before.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		after := &team.StageClock{}
		after.Observe(l(map[string]string{"wN:p1": "build"}), t3)
		assert.Equal(t, t3, since(after, "wN:p1", "build"))
	})
	t.Run("the snapshot carries it, in UTC", func(t *testing.T) {
		t.Parallel()
		c := &team.StageClock{}
		local := t0.In(time.FixedZone("x", 5*3600))
		m := l(map[string]string{"wN:p1": "build", "wJ:p1": "plan"})
		c.Observe(m, local)
		c.Observe(m, t2)
		s := team.Snapshot(m, "", "", spacesOrder, t2, c)
		require.NotNil(t, s.Boss)
		require.NotNil(t, s.Boss.StageStartedAt)
		assert.Equal(t, t0, *s.Boss.StageStartedAt)
		assert.Equal(t, time.UTC, s.Boss.StageStartedAt.Location())
		require.NotNil(t, s.Groups[0].Agents[0].StageStartedAt)
		assert.Equal(t, t0, *s.Groups[0].Agents[0].StageStartedAt)
	})
	t.Run("a nil clock observes nothing", func(t *testing.T) {
		t.Parallel()
		var c *team.StageClock
		c.Observe(l(map[string]string{"wN:p1": "build"}), t0)
		_, ok := c.Since("wN:p1", "build")
		assert.False(t, ok)
	})
}

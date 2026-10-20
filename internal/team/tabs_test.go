package team_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// inTab is p in a tab.
func inTab(id, ws, tab, status string, handoff ...string) herdr.PaneInfo {
	pane := p(id, ws, status, handoff...)
	pane.TabID = tab
	return pane
}

// ha is the herdr-agentisan team as configured: short ha.
var ha = team.Tabs{Project: "herdr-agentisan", Short: "ha", Color: "#7c3aed"}

// TestTabsResolverLabels: in the team's workspace, a tab labelled
// <short>-<type>-<n> is a worker of that type and <short>-boss is the boss's;
// every other tab is outside the model. The type is everything between the
// short code and the last dash, an unconfigured one included; boss is not a
// type; n counts from 1 with no leading zero. The match is exact: case,
// padding and another team's short code all count. Every case has the team's
// $team on its workspace, an empty count included.
func TestTabsResolverLabels(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		label string
		group string // "(boss)" for the boss; "" when outside the model
	}{
		"a worker":                          {label: "ha-coder-1", group: "coder"},
		"a two-digit worker":                {label: "ha-coder-12", group: "coder"},
		"a huge number":                     {label: "ha-coder-99999999999999999999", group: "coder"},
		"every team-model type":             {label: "ha-librarian-3", group: "librarian"},
		"an unconfigured type":              {label: "ha-designer-1", group: "designer"},
		"a type with a dash":                {label: "ha-code-review-2", group: "code-review"},
		"a type ending in a number":         {label: "ha-coder-1-2", group: "coder-1"},
		"a type ending in a dash":           {label: "ha-coder--1", group: "coder-"},
		"a digit type":                      {label: "ha-7-1", group: "7"},
		"the boss":                          {label: "ha-boss", group: "(boss)"},
		"boss is not a type":                {label: "ha-boss-1"},
		"the dashboard":                     {label: "ha-dashboard"},
		"no number":                         {label: "ha-coder"},
		"an empty number":                   {label: "ha-coder-"},
		"zero":                              {label: "ha-coder-0"},
		"a leading zero":                    {label: "ha-coder-01"},
		"a number with a letter":            {label: "ha-coder-1a"},
		"a dash and no number":              {label: "ha-coder--"},
		"no type":                           {label: "ha--1"},
		"the short code alone":              {label: "ha-"},
		"no dash after the short code":      {label: "hacoder-1"},
		"another team's short code":         {label: "as-coder-1"},
		"a longer short code":               {label: "hab-coder-1"},
		"a shorter short code":              {label: "h-coder-1"},
		"upper-case short code":             {label: "HA-coder-1"},
		"upper-case type":                   {label: "ha-Coder-1"},
		"upper-case boss":                   {label: "ha-Boss"},
		"a leading space":                   {label: " ha-coder-1"},
		"a trailing space":                  {label: "ha-coder-1 "},
		"a trailing newline":                {label: "ha-coder-1\n"},
		"a trailing CRLF":                   {label: "ha-coder-1\r\n"},
		"a lone CR inside":                  {label: "ha-co\rder-1"},
		"an NFD type":                       {label: "ha-cafe\u0301-1"},
		"a padded boss":                     {label: "ha-boss "},
		"a NUL in the type":                 {label: "ha-co\x00der-1"},
		"a non-ASCII type":                  {label: "ha-código-1"},
		"a non-ASCII digit":                 {label: "ha-coder-\u0661"},
		"a full-width digit":                {label: "ha-coder-\uff11"},
		"invalid UTF-8 in the type":         {label: "ha-co\xffder-1"},
		"a truncated multibyte number":      {label: "ha-coder-1\xe2\x82"},
		"a BOM before the label":            {label: "\ufeffha-coder-1"},
		"an underscore type":                {label: "ha-code_review-1"},
		"an empty label":                    {label: ""},
		"the project name":                  {label: "herdr-agentisan"},
		"a space-style label":               {label: "◆ coders"},
		"the team name as a short code":     {label: "herdr-agentisan-coder-1"},
		"a type-like label with no team":    {label: "coder-1"},
		"an option-like label":              {label: "--help"},
		"a dashboard-like label with a num": {label: "ha-dashboard-1", group: "dashboard"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := team.Layout{
				Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "w14", Label: "herdr-agentisan"}},
				Tabs:       []herdr.TabInfo{{TabID: "w14:t1", WorkspaceID: "w14", Label: tc.label}},
				Panes:      []herdr.PaneInfo{inTab("w14:p1", "w14", "w14:t1", "working")},
			}
			m := team.Build(l, ha, nil)

			tokens := team.Tokens(m)
			switch tc.group {
			case "":
				assert.Equal(t, map[string]string{"w14": "(team)"}, teamPlaced(m), "not part of the model")
				assert.Equal(t, []team.Token{{Target: team.Target{WorkspaceID: "w14"}, Value: "0 · ◐0 ●0"}}, tokens)
			case "(boss)":
				assert.Equal(t, map[string]string{"w14": "(team)", "w14:p1": "(boss)"}, teamPlaced(m))
				assert.Equal(t, []team.Token{{Target: team.Target{WorkspaceID: "w14"}, Value: "0 · ◐0 ●0"}}, tokens, "the boss is not counted")
			default:
				assert.Equal(t, map[string]string{"w14": "(team)", "w14:p1": tc.group}, teamPlaced(m))
				assert.Equal(t, []team.Token{{Target: team.Target{WorkspaceID: "w14"}, Value: "1 · ◐1 ●0"}}, tokens)
			}
		})
	}
}

// teamPlaced is where Build put each pane, by pane id: its group, or
// "(boss)"; each team's workspace maps to "(team)".
func teamPlaced(m team.Model) map[string]string {
	out := map[string]string{}
	for _, pr := range m.Projects {
		for _, ws := range pr.Workspaces {
			out[ws] = "(team)"
		}
		for _, w := range pr.Boss {
			out[w.Pane.ID] = "(boss)"
		}
		for _, g := range pr.Groups {
			for _, w := range g.Workers {
				out[w.Pane.ID] = g.Name
			}
		}
	}
	return out
}

// TestTabsTeamWorkspace: the team is the workspace labelled exactly with the
// project's name, the first in sidebar order when several are; the others
// are reported as ignored, each once. No such workspace, no team: no site,
// no places, no $team.
func TestTabsTeamWorkspace(t *testing.T) {
	t.Parallel()

	ws := func(id, label string) herdr.WorkspaceInfo { return herdr.WorkspaceInfo{WorkspaceID: id, Label: label} }
	tests := map[string]struct {
		tabs        team.Tabs
		workspaces  []herdr.WorkspaceInfo
		want        string
		wantIgnored []string
	}{
		"the project's workspace": {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w2", "notes"), ws("w14", "herdr-agentisan")}, want: "w14"},
		"the first of three in sidebar order": {
			tabs:        ha,
			workspaces:  []herdr.WorkspaceInfo{ws("w9", "herdr-agentisan"), ws("w14", "herdr-agentisan"), ws("w2", "herdr-agentisan")},
			want:        "w9",
			wantIgnored: []string{"w14", "w2"},
		},
		"a workspace listed twice is not a duplicate": {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w14", "herdr-agentisan"), ws("w14", "herdr-agentisan")}, want: "w14"},
		"a duplicate listed twice is ignored once": {
			tabs:        ha,
			workspaces:  []herdr.WorkspaceInfo{ws("w14", "herdr-agentisan"), ws("w2", "herdr-agentisan"), ws("w2", "herdr-agentisan")},
			want:        "w14",
			wantIgnored: []string{"w2"},
		},
		"no workspace":                 {tabs: ha},
		"a padded label":               {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w1", " herdr-agentisan"), ws("w2", "herdr-agentisan ")}},
		"another case":                 {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w1", "Herdr-agentisan")}},
		"a ◆ label":                    {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w1", "◆ herdr-agentisan")}},
		"a trailing newline":           {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w1", "herdr-agentisan\n")}},
		"a prefix of the name":         {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w1", "herdr")}},
		"the short code":               {tabs: ha, workspaces: []herdr.WorkspaceInfo{ws("w1", "ha")}},
		"an NFD look-alike":            {tabs: team.Tabs{Project: "caf\u00e9", Short: "c"}, workspaces: []herdr.WorkspaceInfo{ws("w1", "cafe\u0301")}},
		"invalid UTF-8 is kept as is":  {tabs: team.Tabs{Project: "a\xff", Short: "a"}, workspaces: []herdr.WorkspaceInfo{ws("w1", "a\xff")}, want: "w1"},
		"the unnamed project is never": {tabs: team.Tabs{Short: "x"}, workspaces: []herdr.WorkspaceInfo{ws("w1", "")}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ignored := tc.tabs.Workspace(tc.workspaces)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantIgnored, ignored)

			m := team.Build(team.Layout{Workspaces: tc.workspaces}, tc.tabs, nil)
			if tc.want == "" {
				assert.Empty(t, m.Projects, "no workspace, no team")
				assert.Empty(t, team.Tokens(m))
				return
			}
			assert.Equal(t, []string{tc.want}, m.Projects[0].Workspaces)
		})
	}
	assert.Empty(t, team.Build(team.Layout{Workspaces: []herdr.WorkspaceInfo{ws("w1", "x")}}, team.Tabs{Project: "x"}, nil).Projects, "no short code, no team")
}

// TestTabsTeamsResolveExactly is NERD-5282's layout: two tabs teams beside
// the ◆ spaces. herdr-agentisan has the boss's tab (the boss and a dashboard
// split, both the boss's), workers of four types, an unconfigured type, tabs
// that do not parse and a duplicate team workspace; assay has only its
// boss's tab. Each team carries its workspaces, the team-model order and its
// colour, and gets one $team summing its workers, the boss left out; the
// boss-only team counts zero.
func TestTabsTeamsResolveExactly(t *testing.T) {
	t.Parallel()
	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{
			{WorkspaceID: "w14", Label: "herdr-agentisan"},
			{WorkspaceID: "w15", Label: "herdr-agentisan"},
			{WorkspaceID: "w20", Label: "assay"},
			{WorkspaceID: "wN", Label: "◆ coders"},
			{WorkspaceID: "w2", Label: "notes"},
		},
		Tabs: []herdr.TabInfo{
			{TabID: "w14:t1", WorkspaceID: "w14", Label: "ha-boss"},
			{TabID: "w14:t2", WorkspaceID: "w14", Label: "ha-coder-1"},
			{TabID: "w14:t3", WorkspaceID: "w14", Label: "ha-coder-2"},
			{TabID: "w14:t4", WorkspaceID: "w14", Label: "ha-qa-1"},
			{TabID: "w14:t5", WorkspaceID: "w14", Label: "ha-review-1"},
			{TabID: "w14:t6", WorkspaceID: "w14", Label: "ha-designer-1"},
			{TabID: "w14:t7", WorkspaceID: "w14", Label: "search"},
			{TabID: "w14:t8", WorkspaceID: "w14", Label: "ha-boss-1"},
			{TabID: "w15:t1", WorkspaceID: "w15", Label: "ha-coder-3"},
			{TabID: "w20:t1", WorkspaceID: "w20", Label: "as-boss"},
			{TabID: "w2:t1", WorkspaceID: "w2", Label: "ha-coder-4"},
		},
		Panes: []herdr.PaneInfo{
			inTab("w14:p1", "w14", "w14:t1", "working"),
			{PaneID: "w14:pD", WorkspaceID: "w14", TabID: "w14:t1", AgentStatus: "unknown"},
			inTab("w14:p2", "w14", "w14:t2", "working"), inTab("w14:p3", "w14", "w14:t3", "idle", "⟳ READY TO RESTART"),
			inTab("w14:p4", "w14", "w14:t4", "blocked"), inTab("w14:p5", "w14", "w14:t5", "done"),
			{PaneID: "w14:p6", WorkspaceID: "w14", TabID: "w14:t6", AgentStatus: "unknown"},
			inTab("w14:p7", "w14", "w14:t7", "working"), inTab("w14:p8", "w14", "w14:t8", "working"),
			// A tab tab.list does not have yet: placed nowhere.
			inTab("w14:p9", "w14", "w14:t9", "working"),
			inTab("w15:p1", "w15", "w15:t1", "working"), inTab("w20:p1", "w20", "w20:t1", "idle"),
			inTab("wN:p1", "wN", "wN:t1", "working"), inTab("w2:p1", "w2", "w2:t1", "working"),
		},
	}
	r := team.Resolvers{
		team.Spaces{Project: "agentisan", Color: "#0ea5e9"},
		ha,
		team.Tabs{Project: "assay", Short: "as"},
	}

	got := team.Build(l, r, nil)

	worker := func(id, tab string, st team.Status, agent string, handoff bool) team.Worker {
		return team.Worker{ID: id, Target: team.Target{WorkspaceID: tab}, Pane: team.Pane{ID: id, Agent: agent, Status: st, Handoff: handoff}}
	}
	withCtx := func(w team.Worker, ctx string) team.Worker {
		w.Pane.Ctx = ctx
		return w
	}
	w14, wN := team.Target{WorkspaceID: "w14"}, team.Target{WorkspaceID: "wN"}
	order := []string{"coder", "precheck", "qa", "review", "research", "librarian", "debugger"}
	assert.Equal(t, team.Model{Projects: []team.Project{
		{
			Name: "agentisan", Color: "#0ea5e9", Workspaces: []string{"wN"}, Order: team.Spaces{}.GroupOrder(),
			Groups: []team.Group{{Name: "coders", Targets: []team.Target{wN}, Workers: []team.Worker{worker("wN:p1", "wN", team.StatusWorking, "claude", false)}}},
		},
		{
			Name: "assay", Workspaces: []string{"w20"}, Order: order, Target: team.Target{WorkspaceID: "w20"},
			Boss: []team.Worker{{ID: "w20:p1", Pane: team.Pane{ID: "w20:p1", Agent: "claude", Status: team.StatusIdle}}},
		},
		{
			Name: "herdr-agentisan", Color: "#7c3aed", Workspaces: []string{"w14"}, Order: order, Target: w14,
			Boss: []team.Worker{
				{ID: "w14:p1", Pane: team.Pane{ID: "w14:p1", Agent: "claude", Status: team.StatusWorking}},
				{ID: "w14:pD", Pane: team.Pane{ID: "w14:pD", Status: team.StatusUnknown}},
			},
			Groups: []team.Group{
				{Name: "coder", Targets: []team.Target{w14}, Workers: []team.Worker{
					worker("w14:p2", "w14", team.StatusWorking, "claude", false), withCtx(worker("w14:p3", "w14", team.StatusIdle, "claude", true), "40"),
				}},
				{Name: "designer", Targets: []team.Target{w14}, Workers: []team.Worker{worker("w14:p6", "w14", team.StatusUnknown, "", false)}},
				{Name: "qa", Targets: []team.Target{w14}, Workers: []team.Worker{worker("w14:p4", "w14", team.StatusBlocked, "claude", false)}},
				{Name: "review", Targets: []team.Target{w14}, Workers: []team.Worker{worker("w14:p5", "w14", team.StatusDone, "claude", false)}},
			},
		},
	}}, got)
	assert.Equal(t, []team.Token{
		{Target: w14, Value: "5 · ◐1 ●2 ⚠1 ⟳1"},
		{Target: team.Target{WorkspaceID: "w20"}, Value: "0 · ◐0 ●0"},
		{Target: wN, Value: "1 · ◐1 ●0"},
	}, team.Tokens(got))

	shuffled := l
	shuffled.Tabs, shuffled.Panes = slices.Clone(l.Tabs), slices.Clone(l.Panes)
	slices.Reverse(shuffled.Tabs)
	slices.Reverse(shuffled.Panes)
	assert.Equal(t, got, team.Build(shuffled, r, nil), "a rebuild from the same tabs and panes in another order is equal")
}

// TestTabsPlacesOnlyItsOwnTabsOnce: a tab in another workspace adds no group
// to the team, even one of a type the team lacks, and a tab tab.list names
// twice is placed once.
func TestTabsPlacesOnlyItsOwnTabsOnce(t *testing.T) {
	t.Parallel()
	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{{WorkspaceID: "w14", Label: "herdr-agentisan"}, {WorkspaceID: "w15", Label: "other"}},
		Panes: []herdr.PaneInfo{
			{PaneID: "w14:p1", WorkspaceID: "w14", TabID: "w14:t2", Agent: "claude", AgentStatus: "idle"},
			{PaneID: "w15:p1", WorkspaceID: "w15", TabID: "w15:t1", Agent: "claude", AgentStatus: "idle"},
		},
		Tabs: []herdr.TabInfo{
			{TabID: "w14:t2", WorkspaceID: "w14", Label: "ha-coder-1"},
			{TabID: "w14:t2", WorkspaceID: "w14", Label: "ha-coder-1"},
			{TabID: "w15:t1", WorkspaceID: "w15", Label: "ha-designer-1"},
		},
	}
	m := team.Build(l, team.Tabs{Project: "herdr-agentisan", Short: "ha"}, nil)
	require.Len(t, m.Projects, 1)
	var names []string
	for _, g := range m.Projects[0].Groups {
		names = append(names, g.Name)
	}
	assert.Equal(t, []string{"coder"}, names, "another workspace's tab adds no group")
	require.Len(t, m.Projects[0].Groups, 1)
	assert.Len(t, m.Projects[0].Groups[0].Workers, 1, "a tab listed twice is placed once")
	assert.Equal(t, []team.Token{{Target: team.Target{WorkspaceID: "w14"}, Value: "1 · ◐0 ●1"}}, team.Tokens(m))
}

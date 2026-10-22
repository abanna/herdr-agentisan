package team_test

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// viewLayout is three teams and an outsider: the ◆ spaces, a group the
// spaces' order does not name (zeta) and the ◆ herdr edit shell among them;
// the herdr-agentisan tabs team, one of whose agents agent.list only names;
// the assay tabs team, with a shell and no agent; and a plain workspace.
func viewLayout() (team.Layout, team.Resolvers) {
	pane := func(id, ws, tab, agent, status string) herdr.PaneInfo {
		return herdr.PaneInfo{PaneID: id, WorkspaceID: ws, TabID: tab, Agent: agent, AgentStatus: status}
	}
	l := team.Layout{
		Workspaces: []herdr.WorkspaceInfo{
			{WorkspaceID: "wJ", Label: "◆ boss"},
			{WorkspaceID: "wN", Label: "◆ coders"},
			{WorkspaceID: "wY", Label: "◆ qa"},
			{WorkspaceID: "wZ", Label: "◆ zeta"},
			{WorkspaceID: "w12", Label: "◆ herdr"},
			{WorkspaceID: "w14", Label: "herdr-agentisan"},
			{WorkspaceID: "w20", Label: "assay"},
			{WorkspaceID: "w2", Label: "notes"},
		},
		Tabs: []herdr.TabInfo{
			{TabID: "w14:t1", WorkspaceID: "w14", Label: "ha-boss"},
			{TabID: "w14:t2", WorkspaceID: "w14", Label: "ha-coder-1"},
			{TabID: "w14:t3", WorkspaceID: "w14", Label: "ha-qa-1"},
			{TabID: "w14:t4", WorkspaceID: "w14", Label: "ha-coder-2"},
			{TabID: "w20:t1", WorkspaceID: "w20", Label: "as-coder-1"},
		},
		Panes: []herdr.PaneInfo{
			pane("wJ:p1", "wJ", "", "claude", "working"),
			pane("wN:p1", "wN", "", "claude", "working"), pane("wN:p2", "wN", "", "", "unknown"), pane("wN:p3", "wN", "", "claude", "blocked"),
			pane("wY:p1", "wY", "", "codex", "idle"), pane("wZ:p1", "wZ", "", "claude", "idle"),
			pane("w12:p1", "w12", "", "claude", "idle"),
			pane("w14:p1", "w14", "w14:t1", "claude", "working"), pane("w14:p2", "w14", "w14:t2", "claude", "working"),
			pane("w14:p3", "w14", "w14:t3", "claude", "done"), pane("w14:p4", "w14", "w14:t4", "", "unknown"),
			pane("w20:p1", "w20", "w20:t1", "", "unknown"),
			pane("w2:p1", "w2", "", "claude", "blocked"),
		},
		Agents: []herdr.AgentInfo{{Name: "ha-coder-2", PaneID: "w14:p4"}},
	}
	return l, team.Resolvers{team.Spaces{Project: "agentisan"}, team.Tabs{Project: "herdr-agentisan", Short: "ha"}, team.Tabs{Project: "assay", Short: "as"}}
}

// blockedOr is the filter of a team holding workspaces: those workspaces,
// or any pane that is blocked.
func blockedOr(workspaces ...[]string) *herdr.ViewFilter {
	var filters []herdr.ViewFilter
	for _, ws := range workspaces {
		filters = append(filters, herdr.ViewFilter{Op: "in", Field: "workspace_id", Values: ws})
	}
	f := herdr.ViewFilter{Op: "any", Filters: append(filters, herdr.ViewFilter{Op: "in", Field: "status", Values: []string{"blocked"}})}
	return &f
}

// TestView is the agents panel's view of the team the focused workspace
// belongs to (ADR-001 A28 addendum): every workspace the team holds, ◆ herdr
// included, or a blocked agent anywhere, labelled with the project's name
// and sorted by the team's own order key, so its panes come first. Outside
// every team there is no view.
func TestView(t *testing.T) {
	t.Parallel()
	l, r := viewLayout()
	m := team.Build(l, r, nil)

	spaces := herdr.AgentView{
		Label:  "agentisan",
		Filter: blockedOr([]string{"w12", "wJ", "wN", "wY", "wZ"}),
		Sort:   []herdr.ViewSort{herdr.ByToken("order-spaces")},
	}
	tabs := herdr.AgentView{
		Label:  "herdr-agentisan",
		Filter: blockedOr([]string{"w14"}),
		Sort:   []herdr.ViewSort{herdr.ByToken("order_ha")},
	}
	tests := map[string]struct {
		workspace string
		want      herdr.AgentView
		ok        bool
	}{
		"a group space":                {workspace: "wN", want: spaces, ok: true},
		"the boss space":               {workspace: "wJ", want: spaces, ok: true},
		"the edit shell is the team's": {workspace: "w12", want: spaces, ok: true},
		"a group the order leaves out": {workspace: "wZ", want: spaces, ok: true},
		"a tabs team's workspace":      {workspace: "w14", want: tabs, ok: true},
		"a team with no agent yet": {workspace: "w20", ok: true, want: herdr.AgentView{
			Label: "assay", Filter: blockedOr([]string{"w20"}), Sort: []herdr.ViewSort{herdr.ByToken("order_as")},
		}},
		"a workspace outside every team": {workspace: "w2"},
		"a workspace herdr never listed": {workspace: "w99"},
		"no focused workspace yet":       {workspace: ""},
		"an alias of a team's workspace": {workspace: "14"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := team.View(m, tc.workspace)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
	_, ok := team.View(team.Model{}, "wN")
	assert.False(t, ok, "no team, no view")
}

// TestViewLabel: the spaces of a configuration that names no [spaces]
// project are labelled ◆ spaces, and a project's name longer than herdr's
// 32 characters is cut to 31 and an ellipsis, since herdr refuses a longer
// label and keeps the view it had (src/app/agent_view.rs:193-205).
func TestViewLabel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		name, want string
	}{
		"the unnamed spaces":    {name: "", want: "◆ spaces"},
		"a short name":          {name: "assay", want: "assay"},
		"exactly 32 characters": {name: strings.Repeat("a", 32), want: strings.Repeat("a", 32)},
		"33 characters":         {name: strings.Repeat("b", 33), want: strings.Repeat("b", 31) + "…"},
		"a long name":           {name: "a-very-long-project-name-for-the-sidebar-view", want: "a-very-long-project-name-for-th…"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Model{Projects: []team.Project{{Name: tc.name, Workspaces: []string{"w1"}}}}
			got, ok := team.View(m, "w1")
			require.True(t, ok)
			assert.Equal(t, tc.want, got.Label)
			assert.LessOrEqual(t, utf8.RuneCountInString(got.Label), 32)
		})
	}
}

// TestViewSplitsWorkspacesByHerdrsLimit: herdr takes 1 to 32 values in one
// in node (src/app/agent_view.rs:14), so a team with more workspaces lists
// them across several, in order, before the blocked node.
func TestViewSplitsWorkspacesByHerdrsLimit(t *testing.T) {
	t.Parallel()
	ids := func(from, to int) []string {
		var out []string
		for i := from; i < to; i++ {
			out = append(out, fmt.Sprintf("w%03d", i))
		}
		return out
	}

	tests := map[string]struct {
		n    int
		want *herdr.ViewFilter
	}{
		"one":         {n: 1, want: blockedOr(ids(0, 1))},
		"32 fit one":  {n: 32, want: blockedOr(ids(0, 32))},
		"33 need two": {n: 33, want: blockedOr(ids(0, 32), ids(32, 33))},
		"64 fit two":  {n: 64, want: blockedOr(ids(0, 32), ids(32, 64))},
		"65 need 3":   {n: 65, want: blockedOr(ids(0, 32), ids(32, 64), ids(64, 65))},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := team.Model{Projects: []team.Project{{Name: "big", Short: "bg", Workspaces: ids(0, tc.n)}}}
			got, ok := team.View(m, "w000")
			require.True(t, ok)
			assert.Equal(t, tc.want, got.Filter)
		})
	}
}

// TestOrderKey: a tabs team's key carries its short code, the spaces' a
// dash no short code can, so the two never collide, and every key is a
// token name herdr takes (src/app/agent_view.rs:18, at most 32 of
// [A-Za-z0-9_-]).
func TestOrderKey(t *testing.T) {
	t.Parallel()
	tokenName := regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

	tests := map[string]struct {
		project team.Project
		want    string
	}{
		"a tabs team":                       {project: team.Project{Name: "herdr-agentisan", Short: "ha"}, want: "order_ha"},
		"the longest short code":            {project: team.Project{Name: "x", Short: "abcdefgh"}, want: "order_abcdefgh"},
		"the spaces":                        {project: team.Project{Name: "agentisan"}, want: "order-spaces"},
		"the unnamed spaces":                {project: team.Project{}, want: "order-spaces"},
		"a tabs team whose short is spaces": {project: team.Project{Name: "s", Short: "spaces"}, want: "order_spaces"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := team.OrderKey(tc.project)
			assert.Equal(t, tc.want, got)
			assert.Regexp(t, tokenName, got)
		})
	}
}

// TestSidebarTokens: each agent pane a team places carries its place in the
// team under the team's key, beside its ctx display: the boss first, then
// the groups in the order the snapshot shows them, each group's workers in
// model order. Only agent panes count, so the places run without gaps; a
// shell, the edit shell's panes and an agent outside every team get no key,
// and a team with no agent pane, assay, gives none.
func TestSidebarTokens(t *testing.T) {
	t.Parallel()
	l, r := viewLayout()
	m := team.Build(l, r, nil)

	got := map[string]map[string]string{}
	for _, pt := range team.SidebarTokens(m, l, nil, time.Time{}) {
		got[pt.PaneID] = pt.Tokens
	}
	ctx := map[string]string{"ctx_ok": "ctx  --", "ctx_warn": "", "ctx_hot": "", "stage_now": "", "stage_old": ""}
	with := func(key, value string) map[string]string {
		out := map[string]string{key: value}
		maps.Copy(out, ctx)
		return out
	}
	assert.Equal(t, map[string]map[string]string{
		"wJ:p1":  with("order-spaces", "000"),
		"wN:p1":  with("order-spaces", "001"),
		"wN:p3":  with("order-spaces", "002"),
		"wY:p1":  with("order-spaces", "003"),
		"wZ:p1":  with("order-spaces", "004"),
		"w12:p1": ctx,
		"w14:p1": with("order_ha", "000"),
		"w14:p2": with("order_ha", "001"),
		"w14:p4": with("order_ha", "002"),
		"w14:p3": with("order_ha", "003"),
		"w2:p1":  ctx,
	}, got)
}

// TestSidebarTokensFollowTheSnapshotsGroupOrder: places follow the groups in
// the order the snapshot shows them (the project's Order), not the order the
// model holds them in.
func TestSidebarTokensFollowTheSnapshotsGroupOrder(t *testing.T) {
	t.Parallel()
	agent := func(id string) team.Worker { return team.Worker{ID: id, Pane: team.Pane{ID: id, Agent: "claude"}} }
	m := team.Model{Projects: []team.Project{{
		Name: "p", Short: "pp", Workspaces: []string{"w1"}, Order: []string{"qa", "coders"},
		Groups: []team.Group{{Name: "coders", Workers: []team.Worker{agent("w1:p1")}}, {Name: "qa", Workers: []team.Worker{agent("w1:p2")}}},
	}}}
	l := team.Layout{Panes: []herdr.PaneInfo{{PaneID: "w1:p1", WorkspaceID: "w1", Agent: "claude"}, {PaneID: "w1:p2", WorkspaceID: "w1", Agent: "claude"}}}

	got := map[string]string{}
	for _, pt := range team.SidebarTokens(m, l, nil, time.Time{}) {
		got[pt.PaneID] = pt.Tokens["order_pp"]
	}
	assert.Equal(t, map[string]string{"w1:p2": "000", "w1:p1": "001"}, got)
}

// TestSidebarTokensWidenPastAThousand: places are three digits, wider only
// when a team has more than 1,000 agents, every place in it then as wide, so
// herdr's string order is still the team's.
func TestSidebarTokensWidenPastAThousand(t *testing.T) {
	t.Parallel()
	for n, last := range map[int]string{1000: "999", 1001: "1000"} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			t.Parallel()
			var l team.Layout
			g := team.Group{Name: "g"}
			for i := range n {
				id := fmt.Sprintf("w1:p%d", i)
				l.Panes = append(l.Panes, herdr.PaneInfo{PaneID: id, WorkspaceID: "w1", Agent: "claude"})
				g.Workers = append(g.Workers, team.Worker{ID: id, Pane: team.Pane{ID: id, Agent: "claude"}})
			}
			m := team.Model{Projects: []team.Project{{Name: "big", Short: "bg", Workspaces: []string{"w1"}, Groups: []team.Group{g}}}}

			place := map[string]string{}
			for _, pt := range team.SidebarTokens(m, l, nil, time.Time{}) {
				place[pt.PaneID] = pt.Tokens["order_bg"]
			}
			values := make([]string, n)
			for i := range n {
				values[i] = place[fmt.Sprintf("w1:p%d", i)]
				assert.Len(t, values[i], len(last), "every place as wide as the last")
			}
			assert.True(t, slices.IsSorted(values), "the places sort as strings in the team's order")
			assert.Equal(t, last, values[n-1])
		})
	}
}

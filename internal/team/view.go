package team

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// The agent view's limits and names (ADR-001 A28 addendum).
const (
	// SpacesLabel labels the spaces' view when [spaces] project names no
	// project.
	SpacesLabel = "◆ spaces"
	// SpacesOrderKey is the spaces' order key. Its dash is in no short
	// code, so it is never a tabs team's.
	SpacesOrderKey = "order-spaces"
	// orderKeyPrefix starts a tabs team's order key, before its short code.
	orderKeyPrefix = "order_"
	// viewLabelMax is the most characters herdr takes in a view's label
	// (src/app/agent_view.rs:17).
	viewLabelMax = 32
	// viewInMax is the most values herdr takes in one in node (:14).
	viewInMax = 32
	// orderWidth is how many digits a place has, at least.
	orderWidth = 3
)

// View is the agent view of the team that holds workspace: the agents
// panel lists the panes in the team's workspaces, and blocked agents from
// anywhere, labelled with the project's name and in the team's order, by
// its order key, so its own panes come first and the rest keep herdr's
// order after them. Outside every team there is none (ok false). The view
// has no Source: the writer gives it its own.
//
// A project's name is [a-z0-9-] (settings), so herdr's trim and control
// strip change nothing; the unnamed spaces are SpacesLabel, and a name past
// 32 characters is cut to 31 and an ellipsis. herdr takes a filter of 64
// nodes at most, so a team past 1,984 workspaces gets a view herdr refuses.
func View(m Model, workspace string) (herdr.AgentView, bool) {
	if workspace == "" {
		return herdr.AgentView{}, false
	}
	for _, pr := range m.Projects {
		if !slices.Contains(pr.Workspaces, workspace) {
			continue
		}
		filters := make([]herdr.ViewFilter, 0, len(pr.Workspaces)/viewInMax+2)
		for ids := range slices.Chunk(pr.Workspaces, viewInMax) {
			filters = append(filters, herdr.In(herdr.ViewWorkspaceID, ids...))
		}
		filters = append(filters, herdr.In(herdr.ViewStatus, string(StatusBlocked)))
		filter := herdr.AnyOf(filters...)
		return herdr.AgentView{
			Label:  viewLabel(pr.Name),
			Filter: &filter,
			Sort:   []herdr.ViewSort{herdr.ByToken(OrderKey(pr))},
		}, true
	}
	return herdr.AgentView{}, false
}

// viewLabel is the view's label for the project name.
func viewLabel(name string) string {
	if name == "" {
		return SpacesLabel
	}
	if r := []rune(name); len(r) > viewLabelMax {
		return string(r[:viewLabelMax-1]) + "…"
	}
	return name
}

// OrderKey is the token a team's agent panes carry their place in it under:
// order_ and the short code for a tabs team, SpacesOrderKey for the spaces.
func OrderKey(pr Project) string {
	if pr.Short == "" {
		return SpacesOrderKey
	}
	return orderKeyPrefix + pr.Short
}

// SidebarTokens are CtxTokens, a team's pane's from m's ctx, which the poll
// gated (GateCtx, A61), each pane also carrying its stage display at
// now, by the stretches idle saw (StageDisplay, A42), and each agent pane a
// team places its place in the team under the team's OrderKey: the boss
// first, then the groups in the order the snapshot shows them, each group's
// workers in the model's order. Only agent panes count, so the places run
// from 0 without a gap, as three digits, or as many as the team's last place
// needs, so herdr's string order is the team's. A pane carries no other
// team's key: one it carried in a team it left expires with the push's TTL,
// and the view's filter, not its key, says which team a pane is in.
func SidebarTokens(m Model, l Layout, idle *IdleClock, now time.Time) []PaneTokens {
	type place struct{ key, value string }
	places := map[string]place{}
	ctxs := map[string]string{}
	for _, pr := range m.Projects {
		var panes []string
		add := func(ws []Worker) {
			for _, w := range ws {
				if w.Pane.IsAgent() {
					panes = append(panes, w.Pane.ID)
					ctxs[w.Pane.ID] = w.Pane.Ctx
				}
			}
		}
		add(pr.Boss)
		for _, g := range orderedGroups(pr) {
			add(g.Workers)
		}
		width := max(orderWidth, len(strconv.Itoa(len(panes)-1)))
		for i, id := range panes {
			places[id] = place{OrderKey(pr), fmt.Sprintf("%0*d", width, i)}
		}
	}
	out := CtxTokens(l)
	stages := stageTokens(l, idle, now)
	for _, pt := range out {
		if p, ok := places[pt.PaneID]; ok {
			pt.Tokens[p.key] = p.value
		}
		if c, ok := ctxs[pt.PaneID]; ok {
			maps.Copy(pt.Tokens, bandTokens(c))
		}
		maps.Copy(pt.Tokens, stages[pt.PaneID])
	}
	return out
}

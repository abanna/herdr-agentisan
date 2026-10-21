package team

import (
	"regexp"
	"slices"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// typeOrder is the team model's worker types in their working order (A19),
// the order a tabs team's groups are shown in.
var typeOrder = []string{"coder", "precheck", "qa", "review", "research", "librarian", "debugger"}

// TypeOrder returns the team model's worker types in their working order.
func TypeOrder() []string { return slices.Clone(typeOrder) }

// typePattern is the alphabet of a worker type, which is a configured
// group's name: lower case, digits and dashes.
var typePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ordinalPattern is a worker's number: from 1, without a leading zero.
var ordinalPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// Tabs is the resolver for one team in the team model (ADR-001 A19): the
// workspace labelled exactly with the project's name, holding one tab per
// worker, labelled <short>-<type>-<n>, and the boss's tab, <short>-boss. A
// worker's group is its type, and every group's target is the team's
// workspace, so its `$team` counts the whole team but the boss. Any other
// tab is outside the model.
type Tabs struct {
	Project string
	// Short is the project's short code, which starts its tab labels and
	// its agents' names. A team without one is not resolved.
	Short string
	Color string
}

// BossTab is the label of the boss's tab.
func (t Tabs) BossTab() string { return t.Short + "-" + BossGroup }

// Type returns the worker type a tab label names, as <short>-<type>-<n>,
// and whether it names one. The type is everything between the short code
// and the last dash, so it may hold dashes itself; boss is not a type.
func (t Tabs) Type(label string) (string, bool) {
	rest, ok := strings.CutPrefix(label, t.Short+"-")
	i := strings.LastIndexByte(rest, '-')
	if !ok || t.Short == "" || i < 0 {
		return "", false
	}
	typ, n := rest[:i], rest[i+1:]
	if !typePattern.MatchString(typ) || typ == BossGroup || !ordinalPattern.MatchString(n) {
		return "", false
	}
	return typ, true
}

// Workspace returns the team's workspace, the first in sidebar order
// labelled exactly with the project's name, or "" when there is none.
// ignored are the other workspaces so labelled, which are not part of the
// model.
func (t Tabs) Workspace(workspaces []herdr.WorkspaceInfo) (id string, ignored []string) {
	for _, ws := range workspaces {
		switch {
		case t.Project == "" || ws.Label != t.Project || ws.WorkspaceID == id:
		case id == "":
			id = ws.WorkspaceID
		case !slices.Contains(ignored, ws.WorkspaceID):
			ignored = append(ignored, ws.WorkspaceID)
		}
	}
	return id, ignored
}

// Resolve places the boss tab's panes with the boss and each worker tab's
// panes in its type's group. Only tabs in the team's workspace count, and a
// tab herdr lists twice is placed once. The team's site is its workspace,
// which is also where its `$team` goes with no worker there yet.
func (t Tabs) Resolve(l Layout) Resolution {
	id, _ := t.Workspace(l.Workspaces)
	if id == "" || t.Short == "" {
		return Resolution{}
	}
	target := Target{WorkspaceID: id}
	byTab := map[string][]herdr.PaneInfo{}
	for _, p := range l.Panes {
		if p.WorkspaceID == id {
			byTab[p.TabID] = append(byTab[p.TabID], p)
		}
	}
	out := Resolution{Sites: []Site{{Project: t.Project, Short: t.Short, Color: t.Color, Workspaces: []string{id}, Order: TypeOrder(), Target: target}}}
	seen := map[string]bool{}
	for _, tab := range l.Tabs {
		if tab.WorkspaceID != id || seen[tab.TabID] {
			continue
		}
		seen[tab.TabID] = true
		pl := Place{Project: t.Project, Panes: byTab[tab.TabID]}
		typ, worker := t.Type(tab.Label)
		switch {
		case worker:
			pl.Group, pl.Target = typ, target
		case tab.Label == t.BossTab():
			pl.Boss = true
		default:
			continue
		}
		out.Places = append(out.Places, pl)
	}
	return out
}

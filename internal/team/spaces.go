package team

import (
	"strings"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// The label prefixes of a group space: today's ◆ and a space, and the
// legacy one herdr-dashboard-v12.py also accepts (v12:122-127).
const (
	SpacePrefix       = "◆ "
	LegacySpacePrefix = "agentisan · "
)

// The group names a space resolves to that are not group targets.
const (
	// BossGroup is the boss space, ◆ boss: its panes are the boss's.
	BossGroup = "boss"
	// EditShellGroup is ◆ herdr, the user's edit shell: one of the team's
	// workspaces (Project.Workspaces, A19), but no group's and not the
	// boss's, so none of its panes is in the model (ADR-001 A13, item 2c).
	// v12 counted it; the port does not (A17).
	EditShellGroup = "herdr"
)

// Spaces is the resolver for today's layout: one workspace per group,
// labelled with SpacePrefix (or LegacySpacePrefix) and the group's name, all
// in one project. A group's target is its workspace. The group is the rest of
// the label with surrounding white space trimmed, matched exactly: "◆ Boss"
// is a group named Boss. A label that is the prefix alone names no group, and
// a workspace without the prefix is not part of the model.
type Spaces struct {
	// Project is the project the spaces belong to: [spaces] project, or
	// its fallback (settings.Resolved.SpacesProject). "" is the unnamed
	// project.
	Project string
	// Color is that project's configured colour; "" when unset.
	Color string
}

// GroupOrder is the order the spaces' groups are shown in: v12's working
// order (herdr-dashboard-v12.py:21, without its herdr, which is not in the
// model), then debugger, the seventh group (ADR-001 Context), where the
// dashboard ranked it before A18. Groups it does not name follow by name
// (Snapshot). Another resolver orders its own groups.
func (Spaces) GroupOrder() []string {
	return []string{"coders", "precheck", "qa", "codex", "research", "clerk", "debugger"}
}

// Resolve places each group space's panes in its group, and the boss
// space's panes with the boss. A workspace herdr lists twice is placed once.
// The team's site is every space, ◆ boss and ◆ herdr included; with no space
// at all there is no team.
func (s Spaces) Resolve(l Layout) Resolution {
	byWorkspace := map[string][]herdr.PaneInfo{}
	for _, p := range l.Panes {
		byWorkspace[p.WorkspaceID] = append(byWorkspace[p.WorkspaceID], p)
	}
	seen := map[string]bool{}
	site := Site{Project: s.Project, Color: s.Color, Order: s.GroupOrder()}
	var out []Place
	for _, ws := range l.Workspaces {
		group, ok := spaceGroup(ws.Label)
		if !ok || seen[ws.WorkspaceID] {
			continue
		}
		seen[ws.WorkspaceID] = true
		site.Workspaces = append(site.Workspaces, ws.WorkspaceID)
		if group == EditShellGroup {
			continue
		}
		pl := Place{Project: s.Project, Panes: byWorkspace[ws.WorkspaceID]}
		if group == BossGroup {
			pl.Boss = true
		} else {
			pl.Group, pl.Target = group, Target{WorkspaceID: ws.WorkspaceID}
		}
		out = append(out, pl)
	}
	if len(site.Workspaces) == 0 {
		return Resolution{}
	}
	return Resolution{Sites: []Site{site}, Places: out}
}

// spaceGroup returns the group a space label names, and whether it names
// one.
func spaceGroup(label string) (string, bool) {
	for _, prefix := range []string{SpacePrefix, LegacySpacePrefix} {
		if rest, ok := strings.CutPrefix(label, prefix); ok {
			group := strings.TrimSpace(rest)
			return group, group != ""
		}
	}
	return "", false
}

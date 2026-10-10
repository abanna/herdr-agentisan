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
	// EditShellGroup is ◆ herdr, the user's edit shell: not part of the
	// model at all (ADR-001 A13, item 2c). v12 counted it; the port does
	// not (A17).
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
}

// Resolve places each group space's panes in its group, and the boss
// space's panes with the boss. A workspace herdr lists twice is placed once.
func (s Spaces) Resolve(l Layout) []Place {
	byWorkspace := map[string][]herdr.PaneInfo{}
	for _, p := range l.Panes {
		byWorkspace[p.WorkspaceID] = append(byWorkspace[p.WorkspaceID], p)
	}
	seen := map[string]bool{}
	var out []Place
	for _, ws := range l.Workspaces {
		group, ok := spaceGroup(ws.Label)
		if !ok || group == EditShellGroup || seen[ws.WorkspaceID] {
			continue
		}
		seen[ws.WorkspaceID] = true
		pl := Place{Project: s.Project, Panes: byWorkspace[ws.WorkspaceID]}
		if group == BossGroup {
			pl.Boss = true
		} else {
			pl.Group, pl.Target = group, Target{WorkspaceID: ws.WorkspaceID}
		}
		out = append(out, pl)
	}
	return out
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

package settings

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// spacesProjectPath is the key naming the project today's ◆ spaces belong
// to (ADR-001 A17).
const spacesProjectPath = "spaces.project"

// spacesSchema is [spaces]. It belongs to the shared file and --set only: a
// project table or a repo file choosing which project the spaces belong to
// would be circular, so there it is an unknown key.
var spacesSchema = &node{kind: kindTable, fields: map[string]*node{
	"project": leafOf(kindString),
}}

// sharedSchema is the shared file's top level and each --set: a profile body
// plus [spaces], [telemetry] and [debugger].
var sharedSchema = func() *node {
	fields := maps.Clone(profileSchema.fields)
	fields["spaces"] = spacesSchema
	fields["telemetry"] = telemetrySchema
	fields["debugger"] = debuggerSchema
	return &node{kind: kindTable, fields: fields}
}()

// spaces checks that spaces.project names a configured project, exactly.
// When no layer sets it and exactly one project is configured, that project
// is its default, reported from the default layer, unless its own table sets
// layout = "tabs" (A19); otherwise the spaces belong to the unnamed project
// and nothing is reported. views are the projects' own merges (projectViews).
func (m merged) spaces(names []string, views map[string]merged) error {
	v, set := m.get(spacesProjectPath)
	if !set {
		if len(names) == 1 && views[names[0]].str("layout") != LayoutTabs {
			table, _ := m.values["spaces"].(map[string]any)
			if table == nil {
				table = map[string]any{}
				m.values["spaces"] = table
			}
			table["project"] = names[0]
			m.sources[spacesProjectPath] = LayerDefault
		}
		return nil
	}
	name, _ := v.(string)
	if slices.Contains(names, name) {
		return nil
	}
	known := "none"
	if len(names) > 0 {
		known = strings.Join(names, ", ")
	}
	return fmt.Errorf("%s: %w %s; known projects: %s", m.at(spacesProjectPath), ErrNoProject, FormatValue(name), known)
}

// SpacesProject is the project today's ◆ spaces belong to: spaces.project as
// resolved, which Load has checked names a configured project, or its
// default. "" is the unnamed project.
func (r Resolved) SpacesProject() string {
	v, _ := lookup(r.Values, spacesProjectPath)
	name, _ := v.(string)
	return name
}

// SpacesColor is the colour of the project the ◆ spaces belong to: its
// team's Color, as the defaults, the shared tables, the project's own table
// and the overrides resolve it; its repo file is not read. "" when that
// project has no colour, or when the spaces belong to the unnamed project.
func (r Resolved) SpacesColor() string {
	for _, t := range r.Teams {
		if t.Layout == LayoutSpaces {
			return t.Color
		}
	}
	return ""
}

// Spaces is what the daemon reads of the configuration once, when it
// starts: the project the ◆ spaces belong to, and that project's colour for
// the snapshot's header (ADR-001 A17, A18). Both are "" for the unnamed
// project.
type Spaces struct {
	Project string
	Color   string
}

// ReadSpaces reads Spaces from the shared file in configDir. No configDir,
// or no file in it, is the unnamed project and not an error: the daemon runs
// with no configuration at all. The read selects no project, so it touches
// no repo, runs no D12 check and reads no environment variable; a colour set
// only in the project's repo file is therefore not seen.
func ReadSpaces(configDir string) (Spaces, error) {
	if configDir == "" {
		return Spaces{}, nil
	}
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return Spaces{}, err
	}
	return Spaces{Project: res.SpacesProject(), Color: res.SpacesColor()}, nil
}

// SpacesProject reads the project the ◆ spaces belong to, as ReadSpaces
// does.
func SpacesProject(configDir string) (string, error) {
	s, err := ReadSpaces(configDir)
	return s.Project, err
}

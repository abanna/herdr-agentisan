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
// plus [spaces].
var sharedSchema = func() *node {
	fields := maps.Clone(profileSchema.fields)
	fields["spaces"] = spacesSchema
	return &node{kind: kindTable, fields: fields}
}()

// spaces checks that spaces.project names a configured project, exactly.
// When no layer sets it and exactly one project is configured, that project
// is its default, reported from the default layer; with none or several, the
// spaces belong to the unnamed project and nothing is reported.
func (m merged) spaces(names []string) error {
	v, set := m.get(spacesProjectPath)
	if !set {
		if len(names) == 1 {
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

// SpacesProject reads the project the ◆ spaces belong to from the shared
// file in configDir, as the daemon does once when it starts. No configDir, or
// no file in it, is the unnamed project and not an error: the daemon runs
// with no configuration at all. The read selects no project, so it touches
// no repo and reads no environment variable.
func SpacesProject(configDir string) (string, error) {
	if configDir == "" {
		return "", nil
	}
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return "", err
	}
	return res.SpacesProject(), nil
}

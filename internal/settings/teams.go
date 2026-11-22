package settings

import (
	"fmt"
	"regexp"
	"slices"
)

// Team layouts in herdr (ADR-001 A17, A19).
const (
	// LayoutTabs is one workspace labelled with the project's name and one
	// tab per worker, labelled <short>-<type>-<n>.
	LayoutTabs = "tabs"
	// LayoutSpaces identifies the daemon and workforce's unnamed ◆ team.
	LayoutSpaces = "spaces"
)

// BossType is the boss's place in a team: its tab is <short>-boss, so no
// worker group may take the name.
const BossType = "boss"

// shortPattern is a short code: it starts a team's tab labels and herdr
// agent names, which herdr keeps unique across the server and caps at 32
// characters.
var shortPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}$`)

// Team is how the daemon finds one configured project's team in herdr.
type Team struct {
	// Project is the project's name, which labels a tabs team's workspace.
	Project string
	// Short is the project's required short code.
	Short string
	// Layout is LayoutTabs or LayoutSpaces.
	Layout string
	// Color is the project's colour as #rrggbb; "" when unset.
	Color string
	// Descriptions are the project's group descriptions, keyed by group
	// name, the ones set to "" left out; nil when there are none (A36).
	Descriptions map[string]string
}

// Teams reads every configured project's team from the shared file in
// configDir, as the daemon does once when it starts: a team added or changed
// later is seen after a daemon restart. No configDir, or no file in it, is
// no team and not an error. The read selects no project, so it touches no
// repo and reads no environment variable.
func Teams(configDir string) ([]Team, error) {
	if configDir == "" {
		return nil, nil
	}
	res, err := Load(LoadOptions{ConfigDir: configDir, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		return nil, err
	}
	return res.Teams, nil
}

// projectViews merges each project's layers short of its repo file: the
// defaults, the shared tables, its own table and the overrides.
func projectViews(shared *source, projects map[string]*source, flags []*source) map[string]merged {
	views := make(map[string]merged, len(projects))
	for name, p := range projects {
		views[name] = merge(slices.Concat([]*source{defaults(), shared, p}, flags)...)
	}
	return views
}

// descriptions are the group descriptions m sets, keyed by group name; nil
// when it sets none but "".
func (m merged) descriptions() map[string]string {
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	var out map[string]string
	for name, raw := range groups {
		g, _ := raw.(map[string]any)
		if d, _ := g["description"].(string); d != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = d
		}
	}
	return out
}

// str is a string leaf; "" when unset.
func (m merged) str(path string) string {
	v, _ := m.get(path)
	s, _ := v.(string)
	return s
}

// teams reads each project's team from its view and checks that short
// codes are unique across projects.
func teams(names []string, views map[string]merged) ([]Team, error) {
	var out []Team
	owner := map[string]string{}
	for _, name := range names {
		v := views[name]
		t := Team{Project: name, Short: v.str("short"), Layout: LayoutTabs, Color: v.str("color"), Descriptions: v.descriptions()}
		switch other, taken := owner[t.Short]; {
		case t.Short == "":
			return nil, fmt.Errorf("projects.%s.short: %w: a tabs project needs a short code, which starts its tab labels and agent names", name, ErrInvalid)
		case !shortPattern.MatchString(t.Short):
			return nil, v.invalidf("short", "%s must match %s", FormatValue(t.Short), shortPattern)
		case taken:
			return nil, v.invalidf("short", "%s is also project %s's short code", FormatValue(t.Short), other)
		}
		owner[t.Short] = name
		out = append(out, t)
	}
	return out, nil
}

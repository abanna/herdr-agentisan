package settings

import (
	"fmt"
	"maps"
	"slices"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// [routing] holds the deployment facts the routing rules leave to the user
// (ADR-001 A50): whether routing decides a spawn's model, each binding's real
// model id, efforts and switch, and the user's own overrides. Like
// [telemetry] it belongs to the shared file and --set: it is one user's, not
// a project's, and a repo file, which whoever commits to the repo writes,
// never chooses which models run. The overrides are the shared file's alone:
// the user's standing choice, the only way to a premium binding.

const (
	routingEnabledPath   = "routing.enabled"
	routingBindingsPath  = "routing.bindings"
	routingOverridesPath = "routing.overrides"
)

// routingSchema is [routing]. Its tables' keys are the rules' own binding and
// role names, so a misspelt one is an unknown key.
var routingSchema = func() *node {
	rules := routing.V1()
	bindings := map[string]*node{}
	for _, name := range rules.BindingNames() {
		bindings[name] = &node{kind: kindTable, fields: map[string]*node{
			"model_id": leafOf(kindString),
			"efforts":  leafOf(kindStrings),
			"enabled":  leafOf(kindBool),
		}}
	}
	overrides := map[string]*node{}
	for _, name := range rules.RoleNames() {
		overrides[name] = leafOf(kindString)
	}
	return &node{kind: kindTable, fields: map[string]*node{
		"enabled":   leafOf(kindBool),
		"bindings":  {kind: kindTable, fields: bindings},
		"overrides": {kind: kindTable, fields: overrides},
	}}
}()

// checkRouting checks [routing] against the rules, key by key, and refuses
// a model key while routing decides the model: the boss's --model and the
// user's overrides name bindings instead.
func (m merged) checkRouting() error {
	d := deploymentOf(m.values)
	rules := routing.V1()
	for _, name := range slices.Sorted(maps.Keys(d.Bindings)) {
		f := d.Bindings[name]
		for _, c := range []struct {
			key   string
			facts routing.Facts
		}{{"model_id", routing.Facts{ModelID: f.ModelID}}, {"efforts", routing.Facts{Efforts: f.Efforts}}} {
			if err := rules.CheckFacts(name, c.facts); err != nil {
				return fmt.Errorf("%s: %w: %w", m.at(routingBindingsPath+"."+name+"."+c.key), ErrInvalid, err)
			}
		}
	}
	for _, role := range slices.Sorted(maps.Keys(d.Overrides)) {
		if err := rules.CheckOverride(role, d.Overrides[role]); err != nil {
			return fmt.Errorf("%s: %w: %w", m.at(routingOverridesPath+"."+role), ErrInvalid, err)
		}
	}
	if !d.Enabled {
		return nil
	}
	paths := []string{"agent.model"}
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		paths = append(paths, "groups."+name+".agent.model")
	}
	for _, path := range paths {
		if _, set := m.get(path); set {
			return m.invalidf(path, "routing decides the model while %s is true: remove it, and name a binding with worker spawn --model or in [routing.overrides]", routingEnabledPath)
		}
	}
	return nil
}

// checkNoRoutingOverrides refuses an override set by --set: an override is
// the user's standing choice, set in the shared file only.
func (s *source) checkNoRoutingOverrides() error {
	if _, ok := lookup(s.values, routingOverridesPath); ok {
		return fmt.Errorf("%s: %w: set the user's overrides in %s, never by --set", s.at(routingOverridesPath), ErrInvalid, FileName)
	}
	return nil
}

// Routing is the resolved [routing] as routing.New takes it. Load has
// checked it against the rules.
func (r Resolved) Routing() routing.Deployment { return deploymentOf(r.Values) }

// deploymentOf reads [routing] out of a resolved tree.
func deploymentOf(values map[string]any) routing.Deployment {
	var d routing.Deployment
	v, _ := lookup(values, routingEnabledPath)
	d.Enabled, _ = v.(bool)
	v, _ = lookup(values, routingBindingsPath)
	if bindings, _ := v.(map[string]any); len(bindings) > 0 {
		d.Bindings = map[string]routing.Facts{}
		for name, raw := range bindings {
			t, _ := raw.(map[string]any)
			var f routing.Facts
			if id, ok := t["model_id"].(string); ok {
				f.ModelID = &id
			}
			f.Efforts, _ = t["efforts"].([]string)
			if on, ok := t["enabled"].(bool); ok {
				f.Enabled = &on
			}
			d.Bindings[name] = f
		}
	}
	v, _ = lookup(values, routingOverridesPath)
	if overrides, _ := v.(map[string]any); len(overrides) > 0 {
		d.Overrides = map[string]string{}
		for role, raw := range overrides {
			d.Overrides[role], _ = raw.(string)
		}
	}
	return d
}

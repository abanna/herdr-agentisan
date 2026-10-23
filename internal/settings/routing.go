package settings

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// [routing] holds the deployment facts the routing rules leave to the user
// (ADR-001 A50): whether routing decides a spawn's model, each binding's real
// model id, efforts and switch, and the user's own overrides. Like
// [telemetry] it belongs to the shared file and --set: it is one user's, not
// a project's, and a repo file, which whoever commits to the repo writes,
// never chooses which models run. The overrides are the shared file's alone:
// the user's standing choice, the only way to a premium binding, a binding's
// name, which binds only workers of its provider, or a list of them, one per
// provider (A63).
// [routing.jev] is whether a spawn asks Jev, the env file its key comes
// from, and the thresholds in whole percents (A57); a repo file never names
// the file a key is read from.

const (
	routingEnabledPath    = "routing.enabled"
	routingBindingsPath   = "routing.bindings"
	routingOverridesPath  = "routing.overrides"
	routingJevPath        = "routing.jev"
	routingJevEnabledPath = "routing.jev.enabled"
	routingJevEnvFilePath = "routing.jev.env_file"
)

// enabledKey switches routing, a binding or Jev.
const enabledKey = "enabled"

// jevPctKeys are [routing.jev]'s thresholds, each a whole percent.
var jevPctKeys = []string{"confidence_pct", "sufficiency_pct"}

// routingSchema is [routing]. Its tables' keys are the rules' own binding and
// role names, so a misspelt one is an unknown key.
var routingSchema = func() *node {
	rules := routing.V1()
	bindings := map[string]*node{}
	for _, name := range rules.BindingNames() {
		bindings[name] = &node{kind: kindTable, fields: map[string]*node{
			"model_id": leafOf(kindString),
			"efforts":  leafOf(kindStrings),
			enabledKey: leafOf(kindBool),
		}}
	}
	overrides := map[string]*node{}
	for _, name := range rules.RoleNames() {
		overrides[name] = leafOf(kindStringOrList)
	}
	jev := map[string]*node{
		enabledKey: leafOf(kindBool),
		"env_file": leafOf(kindString),
	}
	for _, key := range jevPctKeys {
		jev[key] = leafOf(kindInt)
	}
	return &node{kind: kindTable, fields: map[string]*node{
		enabledKey:  leafOf(kindBool),
		"bindings":  {kind: kindTable, fields: bindings},
		"overrides": {kind: kindTable, fields: overrides},
		"jev":       {kind: kindTable, fields: jev},
	}}
}()

// checkRouting checks [routing] against the rules, key by key, and refuses
// a model key while routing decides the model: the boss's --model and the
// user's overrides name bindings instead. It also refuses a codex command
// choosing a profile then, since a profile can set the model and effort
// routing decides; the plugin never edits Codex's config (A43).
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
		if err := rules.CheckOverrides(role, d.Overrides[role]); err != nil {
			return fmt.Errorf("%s: %w: %w", m.at(routingOverridesPath+"."+role), ErrInvalid, err)
		}
	}
	if err := m.checkJev(); err != nil {
		return err
	}
	if !d.Enabled {
		return nil
	}
	agents := []string{agentKey}
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		agents = append(agents, "groups."+name+"."+agentKey)
	}
	for _, agent := range agents {
		if _, set := m.get(agent + ".model"); set {
			return m.invalidf(agent+".model", "routing decides the model while %s is true: remove it, and name a binding with worker spawn --model or in [routing.overrides]", routingEnabledPath)
		}
	}
	for _, agent := range agents {
		raw, _ := m.get(agent + ".command")
		argv, _ := raw.([]string)
		if flag := codexProfileFlag(argv); flag != "" {
			return m.invalidf(agent+".command", "codex's %s chooses a profile, which can set the model and effort routing decides while %s is true: remove it", flag, routingEnabledPath)
		}
	}
	return nil
}

// checkJev checks [routing.jev], whether or not routing is on: the env file
// is a path, as the debugger's is, each threshold a whole percent, and Jev
// enabled names the env file its key comes from (A57).
func (m merged) checkJev() error {
	v, _ := m.get(routingJevEnvFilePath)
	envFile, _ := v.(string)
	if envFile != "" && !validEnvFile(envFile) {
		return m.invalidf(routingJevEnvFilePath, envFileHint)
	}
	for _, key := range jevPctKeys {
		v, ok := m.get(routingJevPath + "." + key)
		if n, _ := v.(int); ok {
			if err := routing.CheckJevPct(n); err != nil {
				return fmt.Errorf("%s: %w: %w", m.at(routingJevPath+"."+key), ErrInvalid, err)
			}
		}
	}
	if on, _ := m.get(routingJevEnabledPath); on == true && envFile == "" {
		return m.invalidf(routingJevEnabledPath, "Jev needs the TypeSafe key: set %s to the env file holding it", routingJevEnvFilePath)
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

// JevEnvFile is routing.jev.env_file, the env file Jev's key is read from,
// with a leading ~/ expanded through lookupEnv's HOME; "" is none.
func (r Resolved) JevEnvFile(lookupEnv func(string) (string, bool)) (string, error) {
	v, _ := lookup(r.Values, routingJevEnvFilePath)
	s, _ := v.(string)
	if s == "" {
		return "", nil
	}
	return ExpandHome(s, lookupEnv)
}

// JevEnvFile is the project's routing.jev.env_file, as Resolved.JevEnvFile
// gives it: a spawn that asks Jev reads the key from it, with the daemon's
// HOME (A57).
func (p Profile) JevEnvFile(lookupEnv func(string) (string, bool)) (string, error) {
	return ExpandHome(p.jevEnvFile, lookupEnv)
}

// Policy is the policy the resolved [routing] makes of the rules: what a
// spawn decides by, named by its version.
func (r Resolved) Policy() (routing.Policy, error) { return policyOf(r.Routing()) }

// Policy is the policy the project's [routing] makes of the rules, the one
// its spawns decide by: the same as Resolved.Policy, so a spawn's decision
// carries the version config resolve prints.
func (p Profile) Policy() (routing.Policy, error) { return policyOf(p.Routing) }

func policyOf(d routing.Deployment) (routing.Policy, error) {
	p, err := routing.New(routing.V1(), d)
	if err != nil {
		return routing.Policy{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return p, nil
}

// CheckRoutingProviders refuses a user override that a spawn of a worker
// type, under a provider it runs and at its role's own effort, would be
// refused for (A50, A63): a binding the deployment disables, leaves
// unresolved, or that does not take the role's effort. An override binds only workers of its binding's
// provider, so one for another provider is no override of the pair, and a
// role's default that fails is the spawn's to refuse. config resolve
// --project runs it whether or not routing is on. Load does not, so one
// type's override never stops the project's other commands.
func (p Profile) CheckRoutingProviders() error {
	policy, err := p.Policy()
	if err != nil {
		return err
	}
	for _, typ := range slices.Sorted(maps.Keys(p.Groups)) {
		if p.Groups[typ].DescribesOnly {
			continue
		}
		role, err := routing.RoleFor(typ)
		if err != nil {
			continue
		}
		// Only a user override is refused here: with no --model or --effort,
		// a pair's Resolve refuses an override only when one applies.
		for _, provider := range p.providersOf(typ) {
			if _, err := policy.Resolve(routing.Request{Role: role, Provider: provider, Type: typ}, time.Time{}); errors.Is(err, routing.ErrOverrideRefused) {
				return fmt.Errorf("%s.%s: %w: the worker type %s under %s: %w", routingOverridesPath, role, ErrInvalid, typ, provider, err)
			}
		}
	}
	return nil
}

// providersOf are the providers a worker of type typ runs, each a pair
// CheckRoutingProviders checks: its agent command's. P16 (NERD-5517) widens
// it to every provider the type may run.
func (p Profile) providersOf(typ string) []string {
	cmd := p.AgentFor(typ).Command
	if len(cmd) == 0 {
		return nil
	}
	return []string{filepath.Base(cmd[0])}
}

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
		d.Overrides = map[string]routing.RoleOverride{}
		for role, raw := range overrides {
			switch v := raw.(type) {
			case string:
				d.Overrides[role] = routing.RoleOverride{v}
			case []string:
				d.Overrides[role] = slices.Clone(v)
			}
		}
	}
	d.Jev = jevOf(values)
	return d
}

// jevOf reads [routing.jev]'s facts out of a resolved tree: nil while it
// sets none, so a deployment without them keeps its version.
func jevOf(values map[string]any) *routing.JevFacts {
	var j routing.JevFacts
	set := false
	if v, ok := lookup(values, routingJevEnabledPath); ok {
		j.Enabled, _ = v.(bool)
		set = true
	}
	for key, to := range map[string]**int{"confidence_pct": &j.ConfidencePct, "sufficiency_pct": &j.SufficiencyPct} {
		v, _ := lookup(values, routingJevPath+"."+key)
		if n, ok := v.(int); ok {
			*to, set = &n, true
		}
	}
	if !set {
		return nil
	}
	return &j
}

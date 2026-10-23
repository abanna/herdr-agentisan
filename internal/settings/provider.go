package settings

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// A worker's provider (ADR-001 A62): every type runs claude or codex, its
// group's default unless a spawn chooses, each provider with a command and a
// model of its own, from [agent.<provider>] and [groups.<g>.agent.<provider>].

// ErrProviderRefused is a worker type that cannot run a provider yet: each is
// a gap the user's rule, that Claude and Codex are interchangeable for every
// type, still has, and its message names what closes it.
var ErrProviderRefused = errors.New("provider refused")

// providerKey is a group's default provider, in its agent table.
const providerKey = "provider"

// providers are the providers a worker can run, the agents D12 names.
var providers = []string{routing.Claude, routing.Codex}

// refusals are the providers a type cannot run yet, each with what lifts the
// refusal (A62).
var refusals = map[[2]string]string{
	{DebuggerType, routing.Codex}: "the debugger runs claude only until Codex has its read fence (NERD-5530)",
	{ReviewType, routing.Claude}:  "a review worker runs codex only until routing v2 (NERD-5529) and a read-only Claude review launch let claude review",
}

// refused is why typ cannot run provider yet, as an ErrProviderRefused; nil
// when it can.
func refused(typ, provider string) error {
	if why, ok := refusals[[2]string{typ, provider}]; ok {
		return fmt.Errorf("%w: %s (a gap, not a rule: ADR-001 A62)", ErrProviderRefused, why)
	}
	return nil
}

// executable is the provider an argv runs: its first element's base name, ""
// when it is empty.
func executable(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return filepath.Base(argv[0])
}

// DefaultProvider is the provider a worker of type typ runs when no spawn
// chooses one: claude for the debugger; else its group's provider; else its
// group's own command's; else codex for the review type; else the top-level
// command's, "" when there is none.
func (p Profile) DefaultProvider(typ string) string {
	g := p.Groups[typ]
	switch {
	case typ == DebuggerType:
		return routing.Claude
	case g.Provider != "":
		return g.Provider
	case len(g.Agent.Command) > 0:
		return executable(g.Agent.Command)
	case typ == ReviewType:
		return routing.Codex
	}
	return executable(p.Agent.Command)
}

// AgentFor is the agent a worker of type typ launches under provider, ""
// being the type's DefaultProvider. It refuses a provider that is neither
// claude nor codex (ErrInvalid) and one the type cannot run yet
// (ErrProviderRefused).
//
// The debugger runs claude, whose arguments the workforce builds, with its
// group's claude model, else its group's model, else the top-level claude
// model, else the top-level model when the top-level command is claude's.
//
// Any other type runs the first of: its group's command for the provider; its
// group's own command, when it is the provider's; for review under codex, the
// built-in read-only codex; the top-level command for the provider; the
// top-level command, when it is the provider's; the provider bare. Its model
// is its group's model for the provider; else its group's model, when the
// provider is the type's default; else, only when the command is the top
// level's, the top-level model for the provider, else the top-level model when
// the top-level command is the provider's. So a group's own command, and
// review's built-in one, run without a top-level model.
//
// A type whose default is "", with no top-level command, gets that command and
// its group's model or the top-level one, as before providers.
func (p Profile) AgentFor(typ, provider string) (Agent, error) {
	switch {
	case provider == "":
		provider = p.DefaultProvider(typ)
	case !slices.Contains(providers, provider):
		return Agent{}, fmt.Errorf("%w: %q is neither claude nor codex", ErrInvalid, provider)
	}
	if err := refused(typ, provider); err != nil {
		return Agent{}, err
	}
	g := p.Groups[typ]
	if typ == DebuggerType {
		model := firstOf(g.Providers[routing.Claude].Model, g.Agent.Model, p.Providers[routing.Claude].Model)
		if model == "" && executable(p.Agent.Command) == routing.Claude {
			model = p.Agent.Model
		}
		return Agent{Command: slices.Clone(debuggerCommand), Model: model}, nil
	}
	if provider == "" {
		return Agent{Command: slices.Clone(p.Agent.Command), Model: firstOf(g.Agent.Model, p.Agent.Model)}, nil
	}
	own := g.Providers[provider]
	var command []string
	top := false
	switch {
	case len(own.Command) > 0:
		command = own.Command
	case len(g.Agent.Command) > 0 && executable(g.Agent.Command) == provider:
		command = g.Agent.Command
	case typ == ReviewType && provider == routing.Codex:
		command = reviewCommand
	case len(p.Providers[provider].Command) > 0:
		command, top = p.Providers[provider].Command, true
	case len(p.Agent.Command) > 0 && executable(p.Agent.Command) == provider:
		command, top = p.Agent.Command, true
	default:
		command, top = []string{provider}, true
	}
	model := own.Model
	if model == "" && provider == p.DefaultProvider(typ) {
		model = g.Agent.Model
	}
	if model == "" && top {
		model = p.Providers[provider].Model
		if model == "" && executable(p.Agent.Command) == provider {
			model = p.Agent.Model
		}
	}
	return Agent{Command: slices.Clone(command), Model: model}, nil
}

// firstOf is the first of ss that is not "".
func firstOf(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// claudeAliases are the model aliases Claude Code takes for its families.
var claudeAliases = []string{"opus", "sonnet", "haiku", "fable", "opusplan"}

// codexModel is a name only Codex runs: an OpenAI o-series model.
var codexModel = regexp.MustCompile(`^o[0-9]`)

// ModelFamily is the provider a model's name says runs it, "" when the name
// is not one it knows: claude for a claude- model, a Bedrock
// anthropic.claude- one (with or without its region) or a Claude Code alias,
// with or without [1m]; codex for a gpt-, codex- or o-series model. A name is
// matched without regard to case, and never guessed at: one it does not know
// belongs to neither.
func ModelFamily(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude-"), strings.HasPrefix(m, "anthropic.claude-"), strings.Contains(m, ".anthropic.claude-"),
		slices.Contains(claudeAliases, strings.TrimSuffix(m, "[1m]")):
		return routing.Claude
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "codex-"), codexModel.MatchString(m):
		return routing.Codex
	}
	return ""
}

// checkFamily refuses model, set at key for provider, when its name says
// another provider runs it. The error names the key and both providers, never
// the model.
func checkFamily(key, model, provider string) error {
	if family := ModelFamily(model); family != "" && provider != "" && family != provider {
		return fmt.Errorf("%s: %w: a %s model, and the agent it is for runs %s", key, ErrInvalid, family, provider)
	}
	return nil
}

// CheckModelProviders refuses a model whose name says another provider runs it
// than the one its table is for (A62): agent.model against the top-level
// command's, agent.<provider>.model against the provider, a group's model
// against the type's default and a group's agent.<provider>.model against the
// provider. config resolve --project runs it, and spawn checks each launch the
// same way; Load does not, so one group's model never stops the project's
// other commands. A name it does not know passes.
func (p Profile) CheckModelProviders() error {
	if err := checkFamily("agent.model", p.Agent.Model, executable(p.Agent.Command)); err != nil {
		return err
	}
	for _, prov := range providers {
		if err := checkFamily("agent."+prov+".model", p.Providers[prov].Model, prov); err != nil {
			return err
		}
	}
	for _, typ := range slices.Sorted(maps.Keys(p.Groups)) {
		g := p.Groups[typ]
		key := "groups." + typ + "." + agentKey
		if err := checkFamily(key+".model", g.Agent.Model, p.DefaultProvider(typ)); err != nil {
			return err
		}
		for _, prov := range providers {
			if err := checkFamily(key+"."+prov+".model", g.Providers[prov].Model, prov); err != nil {
				return err
			}
		}
	}
	return nil
}

// providerAgentSchema is a provider's agent table, top level or a group's:
// a command and a model, which every agent table also has.
func providerAgentSchema() *node {
	return &node{kind: kindTable, fields: map[string]*node{
		commandKey: leafOf(kindStrings),
		modelKey:   leafOf(kindString),
	}}
}

// agentIn reads an agent table's command and model.
func agentIn(t map[string]any) Agent {
	var a Agent
	if cmd, ok := t[commandKey].([]string); ok {
		a.Command = slices.Clone(cmd)
	}
	a.Model, _ = t[modelKey].(string)
	return a
}

// providerAgents reads an agent table's [<provider>] tables, nil when it has
// none.
func providerAgents(a map[string]any) map[string]Agent {
	var out map[string]Agent
	for _, prov := range providers {
		t, ok := a[prov].(map[string]any)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]Agent{}
		}
		out[prov] = agentIn(t)
	}
	return out
}

// checkProviderCommand runs D12 and the launch-flag check on a provider's
// command at path, and refuses one that does not run the provider bare:
// herdr's agent.start runs its own claude or codex by kind from the shell's
// PATH, with arguments only, so a provider's command can carry flags and
// nothing else (no path, no other program, no environment).
func (m merged) checkProviderCommand(path, provider string) error {
	raw, set := m.get(path)
	if !set {
		return nil
	}
	argv, _ := raw.([]string)
	if err := CheckAgentCommand(argv); err != nil {
		return fmt.Errorf("%s: %w", m.at(path), err)
	}
	if argv[0] != provider {
		return m.invalidf(path, "the command runs %s; a %s command names %s bare, then flags only, since herdr runs the agent by kind from the shell's PATH", argv[0], provider, provider)
	}
	if err := checkLaunchFlags(argv); err != nil {
		return fmt.Errorf("%s: %w", m.at(path), err)
	}
	return nil
}

// firstSet is the first key of the table at path that a layer sets, in
// order, "" when none is: a refusal of the table names it.
func (m merged) firstSet(path string) string {
	v, _ := m.get(path)
	t, _ := v.(map[string]any)
	for _, k := range slices.Sorted(maps.Keys(t)) {
		if _, ok := m.origins[path+"."+k]; ok {
			return path + "." + k
		}
	}
	return ""
}

// checkProviderAgents checks every agent table's providers (A62): each
// provider's command (checkProviderCommand); a group's provider, claude or
// codex, and the same as its own command's; and the providers a type cannot
// run yet, whichever key chooses one: the provider key, its own command or a
// table for that provider. The debugger's tables, like its agent table, set a
// model only.
func (m merged) checkProviderAgents() error {
	for _, prov := range providers {
		if err := m.checkProviderCommand(agentKey+"."+prov+".command", prov); err != nil {
			return err
		}
	}
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		agent := "groups." + name + "." + agentKey
		if raw, set := m.get(agent + "." + providerKey); set {
			prov, _ := raw.(string)
			if !slices.Contains(providers, prov) {
				return m.invalidf(agent+"."+providerKey, "%q is neither claude nor codex", prov)
			}
			cmd, _ := m.get(agent + ".command")
			if runs := executable(stringsOf(cmd)); runs != "" && runs != prov {
				return m.invalidf(agent+"."+providerKey, "the group's own command runs %s; set one or the other", runs)
			}
			if err := refused(name, prov); err != nil {
				return fmt.Errorf("%s: %w: %w", m.at(agent+"."+providerKey), ErrInvalid, err)
			}
		}
		if cmd, set := m.get(agent + ".command"); set {
			if err := refused(name, executable(stringsOf(cmd))); err != nil {
				return fmt.Errorf("%s: %w: %w", m.at(agent+".command"), ErrInvalid, err)
			}
		}
		for _, prov := range providers {
			table := agent + "." + prov
			if key := m.firstSet(table); key != "" {
				if err := refused(name, prov); err != nil {
					return fmt.Errorf("%s: %w: %w", m.at(key), ErrInvalid, err)
				}
			}
			if name == DebuggerType {
				if _, set := m.get(table + ".command"); set {
					return m.invalidf(table+".command", "the debugger runs the plugin's read-only launch (ADR-001 A32), so its group sets no command; set a model alone")
				}
			}
			if err := m.checkProviderCommand(table+".command", prov); err != nil {
				return err
			}
		}
	}
	return nil
}

// stringsOf is v as a string list, nil when it is not one.
func stringsOf(v any) []string {
	s, _ := v.([]string)
	return s
}

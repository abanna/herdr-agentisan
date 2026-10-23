package settings

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// What the workforce commands read of a profile besides its groups' counts
// (ADR-001 A23): each type's agent, and how a new tab's shell is waited for.

// ReviewType is the worker type that runs Codex unless its group says
// otherwise.
const ReviewType = "review"

// reviewCommand is the review type's built-in agent command: codex outside
// the shared app-server daemon, which A22 requires for its ctx (`report
// codex` reports nothing under the daemon), in Codex's read-only sandbox,
// which roles/review.md's reviewer works under (A55). It is a rule of the
// per-type lookup, not a default layer: a default groups.review would be a
// group without workers.
var reviewCommand = []string{"codex", "--no-daemon", "--sandbox", "read-only"}

// DebuggerType is the worker type that runs the debugger that ships with the
// plugin (ADR-001 A32): claude with the plugin's read-only launch, which
// the workforce builds. Its group may set a model, never a command, and the
// top-level command never applies to it: a flag there, such as a
// permission mode, would lift the launch's fence. The top-level model
// applies only when the top-level command is claude's: a codex model is no
// claude one.
const DebuggerType = "debugger"

// debuggerCommand is the debugger type's executable; the workforce adds the
// launch's arguments.
var debuggerCommand = []string{routing.Claude}

// DefaultShellReadyAfter is how long spawn waits for a new tab's shell before
// it sends shell_ready_keys, when the profile does not say.
const DefaultShellReadyAfter = 3 * time.Second

// maxShellReadyAfter bounds shell_ready_after_s: the wait must leave a spawn
// most of its budget.
const maxShellReadyAfter = 60

// agentKey is a group's agent table.
const agentKey = "agent"

// commandKey and modelKey are an agent table's command and model.
const (
	commandKey = "command"
	modelKey   = "model"
)

// agentSchema is an agent table, top level or a group's, with a table per
// provider (A62); a group's also names its type's default provider.
func agentSchema(group bool) *node {
	n := providerAgentSchema()
	for _, prov := range providers {
		n.fields[prov] = providerAgentSchema()
	}
	if group {
		n.fields[providerKey] = leafOf(kindString)
	}
	return n
}

// spawnSchema is [spawn].
var spawnSchema = &node{kind: kindTable, fields: map[string]*node{
	"shell_prompt":        leafOf(kindString),
	"shell_ready_keys":    leafOf(kindStrings),
	"shell_ready_after_s": leafOf(kindInt),
}}

// Spawn is how spawn waits for a new tab's shell before it starts the agent
// there.
type Spawn struct {
	// ShellPrompt is a regular expression one line of the shell's prompt
	// matches; "" starts the agent as soon as herdr sees a shell at the
	// pane's foreground, which an interactive question the shell itself
	// asks at startup (a framework's update prompt, say) does not change.
	// herdr trims each line's trailing white space, so a pattern that needs
	// it (`\$ $`) never matches, and the first line from the top that
	// matches ends the wait, so a prompt drawn before a startup question (an
	// instant prompt) ends it at once.
	// herdr matches it with Rust's regex syntax. The check here compiles it
	// with Go's regexp, a first check only: a construct Rust lacks (a
	// literal {, \Q…\E, a nested [) passes here and fails the spawn, and one
	// Go lacks ((?x), a{1001}) is refused here though Rust takes it. A
	// pattern both compile can still match differently: Rust's \d, \w, \s
	// and \b are Unicode's, Go's ASCII's (A53).
	ShellPrompt string
	// ShellReadyKeys are sent once to a new tab whose shell is still not
	// ready after ShellReadyAfter and has a program at its foreground: they
	// answer whatever prompt is up. Empty sends nothing.
	ShellReadyKeys  []string
	ShellReadyAfter time.Duration
}

// maxModel bounds a model's length in bytes.
const maxModel = 128

// modelPattern is what a model may be (ADR-001 A46): a letter or digit, then
// letters, digits and . _ - : / @ [ ], at most maxModel bytes. The workforce
// passes it as one argument after --model, which herdr types into the pane's
// shell bare when every character is in its own safe set, and otherwise inside
// single quotes (PowerShell's for pwsh): none of these is special in either
// form in any shell herdr accepts, fish included, and the first one can never
// start a flag. It still covers opus[1m], gpt-5.1-codex,
// a Bedrock id with its :0 and a Vertex one with its @date.
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)

// CheckModel refuses a model the workforce could not pass to an agent
// safely, one modelPattern does not match. The error never holds the model,
// which may come from a repo file. "" is no model.
func CheckModel(model string) error {
	if model == "" || modelPattern.MatchString(model) {
		return nil
	}
	return fmt.Errorf("%w: a model is a letter or digit, then letters, digits and . _ - : / @ [ ], at most %d bytes", ErrInvalid, maxModel)
}

// checkModels checks agent.model and every group's agent.model as merged,
// whichever layer sets each: a model another layer overrides never reaches
// an agent.
func (m merged) checkModels() error {
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	paths := make([]string, 0, 3*(1+len(groups)))
	agents := make([]string, 0, 1+len(groups))
	agents = append(agents, agentKey)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		agents = append(agents, "groups."+name+"."+agentKey)
	}
	for _, agent := range agents {
		paths = append(paths, agent+".model")
		for _, prov := range providers {
			paths = append(paths, agent+"."+prov+".model")
		}
	}
	for _, path := range paths {
		raw, set := m.get(path)
		if !set {
			continue
		}
		s, _ := raw.(string)
		if err := CheckModel(s); err != nil {
			return fmt.Errorf("%s: %w", m.at(path), err)
		}
	}
	return nil
}

// launchFlags are the agent flags the workforce sets itself: a command that
// carries one would be launched with two. --effort is routing's (A50).
//
// A command can still name a model another way, a codex -p profile or a
// claude --settings file, and is not refused for it (A53): by each CLI's own
// precedence the --model the workforce passes outranks both, and with no
// model set the command's own configuration decides, which only the user's
// layers set (A46). An abbreviation such as --mod is no --model: claude
// matches long flags whole, and codex refuses it as an unexpected argument.
var launchFlags = []string{"--model", "-m", "--name", "-n", "--effort"}

// codexConfigKeys are the Codex settings routing sets (A50): a codex command
// setting one with -c or --config would fight the routed model or effort.
var codexConfigKeys = []string{"model", "model_reasoning_effort"}

// codexHookTrustFlag runs a codex command's enabled hooks without the trust
// a person gives Codex once (A43), and codexHookTrustKey is the setting it
// sets. The user's Q9 keeps that trust a human step, so a worker's command
// carries neither, routing on or off.
const (
	codexHookTrustFlag = "--dangerously-bypass-hook-trust"
	codexHookTrustKey  = "bypass_hook_trust"
)

// checkLaunchFlags refuses a command carrying a flag the workforce sets,
// whether as -m o3, -m=o3 or -mo3: a short flag's value can be attached. A
// codex command is also refused a -c or --config that sets one of
// codexConfigKeys, in any of its forms, and the hook-trust bypass, by its flag
// or its key, or hook trust state. The error names the flag or key, never
// its value.
func checkLaunchFlags(argv []string) error {
	codex := len(argv) > 0 && filepath.Base(argv[0]) == routing.Codex
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		name, _, _ := strings.Cut(arg, "=")
		if !strings.HasPrefix(name, "--") && len(name) > 2 {
			name = name[:2]
		}
		if slices.Contains(launchFlags, name) {
			return fmt.Errorf("%w: %s is set by the workforce: use the model key or [routing], and the worker's name is its logical id", ErrInvalid, name)
		}
		if key := codexConfigKey(argv, i); codex && slices.Contains(codexConfigKeys, key) {
			return fmt.Errorf("%w: codex's %s is set by the workforce: use the model key or [routing]", ErrInvalid, key)
		}
		if bypass := codexHookTrustBypass(argv, i); codex && bypass != "" {
			return fmt.Errorf("%w: codex's %s runs enabled hooks without the trust a person gives Codex once (ADR-001 A43): remove it", ErrInvalid, bypass)
		}
	}
	return nil
}

// codexHookTrustBypass is how argv[i] runs a codex command's hooks without
// their trust: codexHookTrustFlag by its name before any =, or a -c or
// --config setting codexHookTrustKey in any form; "" when it is neither. It
// also covers hook state set directly or through an inline hooks table. It
// names the flag or a fixed key, never a value or a hook identifier.
func codexHookTrustBypass(argv []string, i int) string {
	if name, _, _ := strings.Cut(argv[i], "="); name == codexHookTrustFlag {
		return name
	}
	key, value := codexConfigOverride(argv, i)
	if key == codexHookTrustKey {
		return codexHookTrustKey + " key"
	}
	// State supplied in argv grants the same trust as a human accepting the
	// hook's hash. Refuse the namespace, including entry/leaf assignments,
	// without returning a caller-controlled hook key or value in the error.
	if key == "hooks.state" || strings.HasPrefix(key, "hooks.state.") {
		return "hooks.state key"
	}
	if key != "hooks" {
		return ""
	}
	// Codex parses the value as TOML, falling back to a string on error.
	// Inspect only the effective table's root state field: hook definitions,
	// look-alikes and a string containing "state" do not supply trust state.
	var doc map[string]any
	if err := toml.Unmarshal([]byte("v = "+value), &doc); err != nil {
		return ""
	}
	hooks, ok := doc["v"].(map[string]any)
	if !ok {
		return ""
	}
	if _, state := hooks["state"]; state {
		return "hooks.state key"
	}
	return ""
}

// codexConfigKey is the key argv[i] sets when it is Codex's -c or --config,
// with its key=value separate (-c k=v), joined (--config=k=v, -c=k=v) or
// attached (-ck=v); "" when it is not one. A key may be quoted.
func codexConfigKey(argv []string, i int) string {
	key, _ := codexConfigOverride(argv, i)
	return key
}

// codexConfigOverride extracts the key and raw TOML value from every form
// codexConfigKey accepts. Split only at the first equals: an inline table
// contains its own equals signs. Values are inspected, never echoed.
func codexConfigOverride(argv []string, i int) (string, string) {
	arg, kv := argv[i], ""
	switch {
	case arg == "-c" || arg == "--config":
		if i+1 < len(argv) {
			kv = argv[i+1]
		}
	case strings.HasPrefix(arg, "--config="):
		kv = strings.TrimPrefix(arg, "--config=")
	case strings.HasPrefix(arg, "-c"):
		kv = strings.TrimPrefix(arg[2:], "=")
	default:
		return "", ""
	}
	key, value, _ := strings.Cut(kv, "=")
	return strings.Trim(strings.TrimSpace(key), `"'`), value
}

// codexProfileFlag is how a codex command chooses a profile: -p or
// --profile in any form (-p x, -px, -p=x, --profile x, --profile=x), or a -c
// or --config setting profile, as older Codex reads it; "" when it chooses
// none or is not codex. It names the flag or key, never the profile.
func codexProfileFlag(argv []string) string {
	if len(argv) == 0 || filepath.Base(argv[0]) != routing.Codex {
		return ""
	}
	for i := 1; i < len(argv); i++ {
		name, _, _ := strings.Cut(argv[i], "=")
		if !strings.HasPrefix(name, "--") && len(name) > 2 {
			name = name[:2]
		}
		if name == "-p" || name == "--profile" {
			return name
		}
		if codexConfigKey(argv, i) == "profile" {
			return "profile key"
		}
	}
	return ""
}

// codexProviderFlags choose a codex command's model provider: --oss a local
// one, and --local-provider which, lmstudio or ollama, which Codex reads only
// under --oss and is refused with it.
var codexProviderFlags = []string{"--oss", "--local-provider"}

// codexProviderFlag is how a codex command chooses its model provider: one
// of codexProviderFlags, by its name before any =, or a -c or --config
// setting model_provider in any form; "" when it chooses none or is not
// codex. It names the flag or key, never the provider.
func codexProviderFlag(argv []string) string {
	if len(argv) == 0 || filepath.Base(argv[0]) != routing.Codex {
		return ""
	}
	for i := 1; i < len(argv); i++ {
		if name, _, _ := strings.Cut(argv[i], "="); slices.Contains(codexProviderFlags, name) {
			return name
		}
		if codexConfigKey(argv, i) == "model_provider" {
			return "model_provider key"
		}
	}
	return ""
}

// checkGroupAgents runs D12 and the launch-flag check on every group's own
// command, and refuses one for the debugger type, whose launch is the
// plugin's.
func (m merged) checkGroupAgents() error {
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		path := "groups." + name + ".agent.command"
		raw, set := m.get(path)
		if !set {
			continue
		}
		if name == DebuggerType {
			return m.invalidf(path, "the debugger runs the plugin's read-only launch (ADR-001 A32), so its group sets no command; set agent.model alone")
		}
		argv, _ := raw.([]string)
		if err := CheckAgentCommand(argv); err != nil {
			return fmt.Errorf("%s: %w", m.at(path), err)
		}
		if err := checkLaunchFlags(argv); err != nil {
			return fmt.Errorf("%s: %w", m.at(path), err)
		}
	}
	return nil
}

// checkSpawn checks [spawn]: a prompt pattern that compiles, keys that are
// not empty, and a wait from 0 to maxShellReadyAfter seconds.
func (m merged) checkSpawn() error {
	if v, ok := m.get("spawn.shell_prompt"); ok {
		s, _ := v.(string)
		if s == "" {
			return m.invalidf("spawn.shell_prompt", "an empty pattern matches any line; leave it out instead")
		}
		if _, err := regexp.Compile(s); err != nil {
			return m.invalidf("spawn.shell_prompt", "%s is not a regular expression: %v", FormatValue(s), err)
		}
	}
	if v, ok := m.get("spawn.shell_ready_keys"); ok {
		keys, _ := v.([]string)
		for _, k := range keys {
			if strings.TrimSpace(k) == "" {
				return m.invalidf("spawn.shell_ready_keys", "a key name is empty")
			}
		}
	}
	if v, ok := m.get("spawn.shell_ready_after_s"); ok {
		if n, _ := v.(int); n < 0 || n > maxShellReadyAfter {
			return m.invalidf("spawn.shell_ready_after_s", "%d is outside 0..%d", n, maxShellReadyAfter)
		}
	}
	return nil
}

// buildSpawn reads [spawn] from the merged tree.
func buildSpawn(values map[string]any) Spawn {
	s := Spawn{ShellReadyAfter: DefaultShellReadyAfter}
	if v, ok := lookup(values, "spawn.shell_prompt"); ok {
		s.ShellPrompt, _ = v.(string)
	}
	if v, ok := lookup(values, "spawn.shell_ready_keys"); ok {
		keys, _ := v.([]string)
		s.ShellReadyKeys = slices.Clone(keys)
	}
	if v, ok := lookup(values, "spawn.shell_ready_after_s"); ok {
		n, _ := v.(int)
		s.ShellReadyAfter = time.Duration(n) * time.Second
	}
	return s
}

// groupAgent reads a group's own agent table: its agent, its default
// provider and its providers' agents.
func groupAgent(g map[string]any) (out Agent, provider string, byProvider map[string]Agent) {
	a, _ := g[agentKey].(map[string]any)
	provider, _ = a[providerKey].(string)
	return agentIn(a), provider, providerAgents(a)
}

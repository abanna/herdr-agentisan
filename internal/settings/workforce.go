package settings

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/routing"
)

// What the workforce commands read of a profile besides its groups' counts
// (ADR-001 A23): each type's agent, and how a new tab's shell is waited for.

// ReviewType is the worker type that runs Codex unless its group says
// otherwise.
const ReviewType = "review"

// reviewCommand is the review type's built-in agent command: codex outside
// the shared app-server daemon, which A22 requires for its ctx (`report
// codex` reports nothing under the daemon). It is a rule of the per-type
// lookup, not a default layer: a default groups.review would be a group
// without workers.
var reviewCommand = []string{"codex", "--no-daemon"}

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

// agentSchema is an agent table, top level or a group's.
func agentSchema() *node {
	return &node{kind: kindTable, fields: map[string]*node{
		"command": leafOf(kindStrings),
		"model":   leafOf(kindString),
	}}
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

// AgentFor is the agent a worker of type typ launches: for the debugger
// type, claude, whose arguments the workforce builds, with the group's model,
// else the top-level one when the top-level command is claude's; else its
// group's own command and model when its
// group sets a command, without the top-level model, which belongs to the
// top-level command; else codex for the review type; else the top-level
// command, with the group's model or the top-level one.
func (p Profile) AgentFor(typ string) Agent {
	g := p.Groups[typ]
	switch {
	case typ == DebuggerType:
		model := g.Agent.Model
		if model == "" && len(p.Agent.Command) > 0 && filepath.Base(p.Agent.Command[0]) == routing.Claude {
			model = p.Agent.Model
		}
		return Agent{Command: slices.Clone(debuggerCommand), Model: model}
	case len(g.Agent.Command) > 0:
		return Agent{Command: slices.Clone(g.Agent.Command), Model: g.Agent.Model}
	case typ == ReviewType:
		return Agent{Command: slices.Clone(reviewCommand), Model: g.Agent.Model}
	}
	model := g.Agent.Model
	if model == "" {
		model = p.Agent.Model
	}
	return Agent{Command: slices.Clone(p.Agent.Command), Model: model}
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
	paths := make([]string, 0, 1+len(groups))
	paths = append(paths, "agent.model")
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		paths = append(paths, "groups."+name+".agent.model")
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

// checkLaunchFlags refuses a command carrying a flag the workforce sets,
// whether as -m o3, -m=o3 or -mo3: a short flag's value can be attached. A
// codex command is also refused a -c or --config that sets one of
// codexConfigKeys, in any of its forms. The error names the flag or key,
// never its value.
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
	}
	return nil
}

// codexConfigKey is the key argv[i] sets when it is Codex's -c or --config,
// with its key=value separate (-c k=v), joined (--config=k=v, -c=k=v) or
// attached (-ck=v); "" when it is not one. A key may be quoted.
func codexConfigKey(argv []string, i int) string {
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
		return ""
	}
	key, _, _ := strings.Cut(kv, "=")
	return strings.Trim(strings.TrimSpace(key), `"'`)
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

// groupAgent reads a group's own agent table.
func groupAgent(g map[string]any) Agent {
	a, _ := g[agentKey].(map[string]any)
	var out Agent
	if cmd, ok := a["command"].([]string); ok {
		out.Command = slices.Clone(cmd)
	}
	out.Model, _ = a["model"].(string)
	return out
}

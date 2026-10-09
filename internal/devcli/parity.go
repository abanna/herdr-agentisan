package devcli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Three places describe how to run this repository's gates: the AGENTS.md
// command table, Taskfile.yml, and .github/workflows/ci.yml. If they drift,
// AGENTS.md does not merely go stale — it actively misleads, because an agent
// that runs the documented command and sees it pass concludes the gate passes.
// ParityReport checks every direction so that cannot happen silently.

// localOnly lists documented commands that legitimately have no CI counterpart,
// each with the reason. CI never starts a server or installs local hooks.
var localOnly = map[string]string{
	"task run:server":        "CI never starts the server",
	"task run:cli":           "CI never runs the interactive CLI",
	"task install":           "installs into the developer's GOBIN",
	"task tools":             "local toolchain bootstrap",
	"uvx pre-commit install": "writes .git/hooks/pre-commit in the developer's clone",
	"task tdaddy:index":      "tdaddy is machine-local; go-tdad is a private repo CI cannot `go install`",
	"task tdaddy:impact":     "same — and it needs a warm local graph store",
}

// nonGateTasks are Taskfile tasks that are not gates and need no CI
// counterpart, each with the reason it is exempt.
//
// This map is an escape hatch from the parity check: a task named here is not
// compared against CI at all. Adding an entry weakens the gate, so every one
// carries a justification and adding a new one is a reviewable decision, not a
// way to silence a failure. If a parity failure tempts you to add a name here,
// the right fix is almost always to make CI and the docs agree instead.
var nonGateTasks = map[string]string{
	"default":        "lists tasks",
	"build":          "aggregate build target",
	"build:cli":      "produces a binary; CI builds via `go test`/`go vet`",
	"build:devctl":   "produces a binary",
	"build:server":   "produces a binary",
	"install":        "installs into the developer's GOBIN",
	"run:server":     "CI never starts the server",
	"run:cli":        "CI never runs the interactive CLI",
	"check":          "aggregate of the gates below it",
	"clean":          "deletes build output",
	"tools":          "prints pinned tool versions",
	"ci":             "runs the workflow locally via act",
	"lint:fix":       "mutates source; CI must never auto-fix",
	"openapi:write":  "regenerates in place; the `openapi` task is the gate",
	"cover:html":     "opens a report for a human",
	"hooks:install":  "writes into the developer's .git/, not a gate",
	"secrets:staged": "reads the git index, which is empty in CI; the security job scans history instead",
	"tdaddy:index":   "machine-local dependency graph; not a CI gate",
	"tdaddy:impact":  "machine-local; advisory test selection, not a gate",
	// The kind tasks create, mutate and destroy a local Kubernetes cluster.
	// CI has no cluster and must not acquire one to satisfy a docs check: the
	// gates that DO run in CI over this material are `devctl manifests` and
	// the promtool rule tests, both of which are static and both of which are
	// compared normally.
	"kind:up":     "creates a local kind cluster",
	"kind:down":   "deletes a local kind cluster",
	"kind:deploy": "builds an image and rolls it out into a local cluster",
	"kind:secret": "generates a local Secret from /dev/urandom",
	"kind:smoke":  "exercises a running local cluster",
	// The k8s:* family reaches INTO a running cluster: it lists, follows,
	// port-forwards, execs and pulls profiles. None of it asserts anything, so
	// none of it is a gate, and CI has no cluster to point it at. The gates
	// that DO cover this material are `devctl manifests` and the promtool rule
	// tests, both static.
	"k8s:status":    "lists what is deployed",
	"k8s:resources": "lists every object this repo owns",
	"k8s:ui":        "port-forwards the UIs for a human",
	"k8s:logs":      "follows logs from a running cluster",
	"k8s:events":    "reads cluster events",
	"k8s:shell":     "execs a shell into a pod",
	"k8s:alerts":    "reads live alert state out of Prometheus",
	"k8s:profile":   "pulls a pprof profile from a running pod",
}

// ParityReport is the outcome of the three-way comparison.
type ParityReport struct {
	// UndocumentedCIGates are commands CI runs that AGENTS.md does not list.
	UndocumentedCIGates []string
	// DocumentedNotInCI are table commands CI never runs and that are not
	// declared local-only.
	DocumentedNotInCI []string
	// TaskGatesNotInCI are Taskfile gate tasks whose command CI never runs.
	TaskGatesNotInCI []string
	// CIGatesNotInTask are CI commands no Taskfile task runs, so `task check`
	// would quietly run fewer gates than CI.
	CIGatesNotInTask []string
}

// OK reports whether the three sources agree.
func (r ParityReport) OK() bool {
	return len(r.UndocumentedCIGates) == 0 && len(r.DocumentedNotInCI) == 0 &&
		len(r.TaskGatesNotInCI) == 0 && len(r.CIGatesNotInTask) == 0
}

// String renders the report as the human-readable failure message.
func (r ParityReport) String() string {
	if r.OK() {
		return "docs parity: AGENTS.md, Taskfile.yml and ci.yml agree"
	}
	var b strings.Builder
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "  - %s\n", it)
		}
	}
	b.WriteString("docs parity FAILED")
	section("CI runs these, but AGENTS.md does not document them", r.UndocumentedCIGates)
	section("AGENTS.md documents these, but CI does not run them", r.DocumentedNotInCI)
	section("Taskfile gates whose command CI does not run", r.TaskGatesNotInCI)
	section("CI gates no Taskfile task runs (so `task check` is weaker than CI)", r.CIGatesNotInTask)
	return b.String()
}

// tableRow matches a row of the AGENTS.md command table: | Task | `cmd` |.
var tableRow = regexp.MustCompile("^\\|[^|]+\\|\\s*`([^`]+)`\\s*\\|")

// readUnderRoot reads name from within root, refusing to follow a path that
// escapes it. os.Root confines the traversal, which is what gosec's G304 asks
// for and what keeps a caller-supplied root from reaching arbitrary files.
func readUnderRoot(root, name string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open repository root %s: %w", root, err)
	}
	defer r.Close() //nolint:errcheck // read-only root; close error is not actionable

	f, err := r.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return raw, nil
}

// parseDocCommands pulls every command out of the AGENTS.md table.
func parseDocCommands(root string) ([]string, error) {
	raw, err := readUnderRoot(root, "AGENTS.md")
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if m := tableRow.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no command table rows found in AGENTS.md")
	}
	return out, nil
}

// taskfile is the subset of Taskfile.yml this check reads.
type taskfile struct {
	Vars  map[string]yaml.Node `yaml:"vars"`
	Tasks map[string]struct {
		Cmds []yaml.Node `yaml:"cmds"`
		Deps []yaml.Node `yaml:"deps"`
	} `yaml:"tasks"`
}

// templateVar matches a Taskfile variable reference such as {{.COVERAGE_MIN}}.
var templateVar = regexp.MustCompile(`\{\{\s*\.([A-Za-z0-9_]+)\s*\}\}`)

// expandVars substitutes scalar Taskfile vars into a command string. A command
// left templated would never equal the literal CI runs, so every comparison
// would report drift that is not there. Vars this cannot resolve (computed
// `sh:` vars, CLI_ARGS) are left alone and filtered out by the caller.
func expandVars(cmd string, vars map[string]string) string {
	return templateVar.ReplaceAllStringFunc(cmd, func(m string) string {
		name := templateVar.FindStringSubmatch(m)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

// scalarVars pulls the plain string vars out of the Taskfile vars block.
func scalarVars(nodes map[string]yaml.Node) map[string]string {
	out := make(map[string]string, len(nodes))
	for k, n := range nodes {
		if n.Kind == yaml.ScalarNode {
			out[k] = n.Value
		}
	}
	return out
}

// parseTaskCommands returns the shell commands of every gate task, keyed by task.
func parseTaskCommands(root string) (map[string][]string, error) {
	raw, err := readUnderRoot(root, "Taskfile.yml")
	if err != nil {
		return nil, err
	}
	var tf taskfile
	if err := yaml.Unmarshal(raw, &tf); err != nil {
		return nil, fmt.Errorf("parse Taskfile.yml: %w", err)
	}

	vars := scalarVars(tf.Vars)
	out := map[string][]string{}
	for name, task := range tf.Tasks {
		if _, exempt := nonGateTasks[name]; exempt {
			continue
		}
		for _, node := range task.Cmds {
			// A cmd is either a plain string or a mapping (e.g. `task:`).
			if node.Kind != yaml.ScalarNode {
				continue
			}
			cmd := normalise(expandVars(node.Value, vars))
			// A command still carrying an unresolved template (a computed
			// `sh:` var, or CLI_ARGS) cannot be compared literally.
			if templateVar.MatchString(cmd) {
				continue
			}
			out[name] = append(out[name], cmd)
		}
	}
	return out, nil
}

// workflow is the subset of ci.yml this check reads.
type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// parseCICommands returns every `run:` command in the workflow.
func parseCICommands(root string) ([]string, error) {
	raw, err := readUnderRoot(root, filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		return nil, err
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("parse ci.yml: %w", err)
	}
	var out []string
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			for line := range strings.SplitSeq(step.Run, "\n") {
				if c := normalise(line); c != "" && isGateCommand(c) {
					out = append(out, c)
				}
			}
		}
	}
	return out, nil
}

// isGateCommand filters out setup steps that are not gates.
func isGateCommand(c string) bool {
	for _, prefix := range []string{
		"go mod download", "go version", "echo ", "mkdir ", "cd ",
		"sudo ", "curl ", "sh -c", "git config",
	} {
		if strings.HasPrefix(c, prefix) {
			return false
		}
	}
	// Gate runners this repo actually uses. A gate invoked through a runner
	// absent from this list is invisible to the parity check, which is worse
	// than a false positive: the check would pass while the docs drifted.
	// Add the runner here rather than routing around it.
	//
	// `docker ` earns its place because promtool has no usable Go entrypoint:
	// `go run github.com/prometheus/prometheus/cmd/promtool@vX` compiles the
	// whole of Prometheus, and there is no tool-directive form that does not
	// drag that into this module. Running the pinned prom/prometheus image is
	// the same binary CI and a laptop both get. Without this prefix the
	// alert-rule gate would be dropped from the CI side of the comparison and
	// then reported as drift in two directions at once — which is exactly the
	// pressure to weaken `localOnly` that this check exists to resist.
	for _, prefix := range []string{"task ", "go ", "git ", "uvx ", "docker "} {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// normalise collapses whitespace so formatting differences are not drift.
func normalise(s string) string { return strings.Join(strings.Fields(s), " ") }

// CheckParity compares the three sources rooted at root.
func CheckParity(root string) (ParityReport, error) {
	doc, err := parseDocCommands(root)
	if err != nil {
		return ParityReport{}, err
	}
	tasks, err := parseTaskCommands(root)
	if err != nil {
		return ParityReport{}, err
	}
	ci, err := parseCICommands(root)
	if err != nil {
		return ParityReport{}, err
	}

	docSet := toSet(doc)
	ciSet := toSet(ci)

	taskCmdSet := map[string]bool{}
	taskByCmd := map[string]string{}
	for name, cmds := range tasks {
		for _, c := range cmds {
			taskCmdSet[c] = true
			taskByCmd[c] = name
		}
	}

	var rep ParityReport
	for c := range ciSet {
		if !docSet[c] {
			rep.UndocumentedCIGates = append(rep.UndocumentedCIGates, c)
		}
		// A CI command satisfied either by a task running it directly, or by
		// the documented table entry being the task that wraps it.
		if !taskCmdSet[c] && !strings.HasPrefix(c, "task ") {
			rep.CIGatesNotInTask = append(rep.CIGatesNotInTask, c)
		}
	}
	for c := range docSet {
		if !ciSet[c] {
			if _, ok := localOnly[c]; !ok {
				rep.DocumentedNotInCI = append(rep.DocumentedNotInCI, c)
			}
		}
	}
	for c, name := range taskByCmd {
		if !ciSet[c] && !strings.HasPrefix(c, "task ") {
			rep.TaskGatesNotInCI = append(rep.TaskGatesNotInCI, fmt.Sprintf("%s: %s", name, c))
		}
	}

	sort.Strings(rep.UndocumentedCIGates)
	sort.Strings(rep.DocumentedNotInCI)
	sort.Strings(rep.TaskGatesNotInCI)
	sort.Strings(rep.CIGatesNotInTask)
	return rep, nil
}

func toSet(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s] = true
	}
	return out
}

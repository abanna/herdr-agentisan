package settings

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	colorPattern  = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	githubPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
)

// Rotation thresholds are context percentages: exactly thresholdCount of
// them, strictly ascending, within [minThreshold, maxThreshold].
const (
	thresholdCount = 3
	minThreshold   = 1
	maxThreshold   = 100
	// rotationPath is where a profile, or a group of one, sets them.
	rotationPath = "rotation.thresholds"
)

// merged is the outcome of merging sources, lowest first.
type merged struct {
	values map[string]any
	// sources maps every leaf path to its layer name.
	sources map[string]string
	// origins maps every leaf path to the source that set it, so an error
	// names the file as well as the layer.
	origins map[string]*source
	// tables maps every table path to the source that first set it, the
	// lowest layer that has it, so an error about a table names where it
	// came from even when it holds no leaf.
	tables map[string]*source
}

// merge deep-merges tables and replaces everything else whole: scalars,
// lists and secret references are leaves. nil sources are skipped.
func merge(srcs ...*source) merged {
	m := merged{values: map[string]any{}, sources: map[string]string{}, origins: map[string]*source{}, tables: map[string]*source{}}
	for _, s := range srcs {
		if s != nil {
			m.mergeInto(m.values, s.values, "", s)
		}
	}
	return m
}

func (m merged) mergeInto(dst, src map[string]any, prefix string, s *source) {
	for k, v := range src {
		p := join(prefix, k)
		if table, ok := v.(map[string]any); ok {
			sub, ok := dst[k].(map[string]any)
			if !ok {
				sub = map[string]any{}
				dst[k] = sub
				m.tables[p] = s
			}
			m.mergeInto(sub, table, p, s)
			continue
		}
		dst[k] = v
		m.sources[p] = s.layer
		m.origins[p] = s
	}
}

// at names a resolved key path with the layer and file that set it.
func (m merged) at(path string) string {
	if s, ok := m.origins[path]; ok {
		return s.at(path)
	}
	return path + " (not set in any layer)"
}

// tableAt names a table's path with the layer and file that first set it.
func (m merged) tableAt(path string) string {
	if s, ok := m.tables[path]; ok {
		return s.at(path)
	}
	return m.at(path)
}

func (m merged) invalidf(path, format string, args ...any) error {
	return fmt.Errorf("%s: %w: %s", m.at(path), ErrInvalid, fmt.Sprintf(format, args...))
}

// needsCount refuses a group whose count key is missing. A key no layer sets
// has no source of its own, so the refusal names the group's table, with the
// layer and file that added it, and then the key.
func (m merged) needsCount(group, key string) error {
	return fmt.Errorf("%s: %w: every group needs %s: %s", m.tableAt("groups."+group), ErrInvalid, key, m.at("groups."+group+"."+key))
}

func (m merged) get(path string) (any, bool) {
	return lookup(m.values, path)
}

// lookup walks a dotted path through a tree of tables. Names never contain
// dots (namePattern), so the path is unambiguous.
func lookup(tree map[string]any, path string) (any, bool) {
	var cur any = tree
	for seg := range strings.SplitSeq(path, ".") {
		table, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = table[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// checkFields runs the field rules and the D12 command checks on whatever
// the merged layers set. None of them needs a selected project.
func (m merged) checkFields() error {
	for _, check := range []func() error{m.checkColor, m.checkGitHub, m.checkAgent, m.checkGroups, m.checkGroupAgents, m.checkProviderAgents, m.checkModels, m.checkSpawn, m.checkVerify, m.checkRotation, m.checkNotices, m.checkTelemetry, m.checkTests, m.checkDebugger, m.checkRouting} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (m merged) checkColor() error {
	if v, ok := m.get("color"); ok {
		if s, _ := v.(string); !colorPattern.MatchString(s) {
			return m.invalidf("color", "%q is not a #rrggbb colour", s)
		}
	}
	return nil
}

func (m merged) checkGitHub() error {
	if v, ok := m.get("github"); ok {
		if s, _ := v.(string); !githubPattern.MatchString(s) {
			return m.invalidf("github", "%q is not owner/name", s)
		}
	}
	return nil
}

func (m merged) checkAgent() error {
	v, _ := m.get("agent.command")
	argv, _ := v.([]string)
	if err := CheckAgentCommand(argv); err != nil {
		return fmt.Errorf("%s: %w", m.at("agent.command"), err)
	}
	if err := checkLaunchFlags(argv); err != nil {
		return fmt.Errorf("%s: %w", m.at("agent.command"), err)
	}
	return nil
}

// MaxSlots bounds a group's slots. Every slot can be a tab the workforce
// opens (A23), so the bound keeps a mistyped cap from opening thousands, and
// keeps every count the workforce sizes from one small.
const MaxSlots = 64

// maxGroupName bounds a group's name: a worker's id, <short>-<group>-<n>, is
// its agent's name, at most 32 characters in herdr, and with a short code of
// up to 8 and n up to 99, a group of up to 20 fits (A53). Spawn still checks
// the whole id.
const maxGroupName = 20

func (m merged) checkGroups() error {
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		g, _ := groups[name].(map[string]any)
		workersPath, slotsPath := "groups."+name+".workers", "groups."+name+".slots"
		if name == BossType {
			return m.invalidf(workersPath, "%s is the boss's tab, <short>-%s, not a worker type", name, name)
		}
		if len(name) > maxGroupName {
			return fmt.Errorf("%s: %w: a group's name is at most %d characters, so its workers' ids, <short>-<group>-<n>, fit herdr's 32-character agent names", m.tableAt("groups."+name), ErrInvalid, maxGroupName)
		}
		description, described := g["description"].(string)
		if described {
			if err := checkDescription(description); err != nil {
				return m.invalidf("groups."+name+".description", "%v", err)
			}
		}
		if describesOnly(g) {
			continue // it describes a group and starts no worker (A36)
		}
		workers, ok := g["workers"].(int)
		if !ok {
			return m.needsCount(name, "workers")
		}
		if workers < 0 {
			return m.invalidf(workersPath, "workers %d is below 0", workers)
		}
		slots, ok := g["slots"].(int)
		if !ok {
			return m.needsCount(name, "slots")
		}
		if slots < workers {
			return m.invalidf(slotsPath, "slots %d is below workers %d", slots, workers)
		}
		if slots > MaxSlots {
			return m.invalidf(slotsPath, "slots %d is above %d", slots, MaxSlots)
		}
	}
	return nil
}

// describesOnly reports whether a merged group table only describes: it sets
// a description, even "", and neither count (A36).
func describesOnly(g map[string]any) bool {
	_, hasWorkers := g["workers"].(int)
	_, hasSlots := g["slots"].(int)
	_, described := g["description"].(string)
	return described && !hasWorkers && !hasSlots
}

// checkDescribers refuses an agent on every group that only describes, which
// never launches (A36): any key of its agent table, the command first, but a
// model of "", which is none, from whichever layer sets it. Only a project's
// whole merge can tell that a group only describes, since a later layer, the
// repo file included, may give it counts; so Load runs this on that merge
// alone, never on the partial ones checkFields also lints.
func (m merged) checkDescribers() error {
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		g, _ := groups[name].(map[string]any)
		if !describesOnly(g) {
			continue
		}
		agent, _ := g[agentKey].(map[string]any)
		for _, key := range slices.Sorted(maps.Keys(agent)) {
			if agent[key] != "" {
				return m.invalidf("groups."+name+"."+agentKey+"."+key, "the group only describes, so it starts no worker and takes no agent; give it workers and slots, or leave the agent out")
			}
		}
	}
	return nil
}

// checkDescription reports whether d can be a group's description: one line
// every client draws as written, at most MaxDescriptionChars characters, and
// printable throughout (letters, marks, numbers, punctuation, symbols and the
// ASCII space), so no control character, format character or other space.
// "" is no description. The error names the reason, never the value, which
// may carry terminal escapes.
func checkDescription(d string) error {
	if !utf8.ValidString(d) {
		return errors.New("the description is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(d); n > MaxDescriptionChars {
		return fmt.Errorf("the description is %d characters, more than %d", n, MaxDescriptionChars)
	}
	for i, r := range d {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("the description has the unprintable character %U at byte %d", r, i)
		}
	}
	return nil
}

func (m merged) checkVerify() error {
	v, _ := m.get("verify")
	commands, _ := v.([][]string)
	for i, argv := range commands {
		if len(argv) == 0 || argv[0] == "" {
			return m.invalidf("verify", "command %d is empty", i+1)
		}
	}
	return nil
}

// checkRotation checks the project's thresholds, which the defaults always
// set, and each group's own, where one sets them (A33).
func (m merged) checkRotation() error {
	if err := m.checkThresholds(rotationPath); err != nil {
		return err
	}
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		path := "groups." + name + "." + rotationPath
		if _, ok := m.get(path); ok {
			if err := m.checkThresholds(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkThresholds checks the thresholds at path.
func (m merged) checkThresholds(path string) error {
	v, _ := m.get(path)
	ts, _ := v.([]int)
	if len(ts) != thresholdCount {
		return m.invalidf(path, "want exactly %d thresholds, got %d", thresholdCount, len(ts))
	}
	for i, t := range ts {
		if t < minThreshold || t > maxThreshold {
			return m.invalidf(path, "%d is outside %d..%d", t, minThreshold, maxThreshold)
		}
		if i > 0 && t <= ts[i-1] {
			return m.invalidf(path, "%v is not strictly ascending", ts)
		}
	}
	return nil
}

// repo resolves the project's repo: set, ~ expanded, absolute, and a
// directory that exists.
func (m merged) repo(lookupEnv func(string) (string, bool), stat func(string) (fs.FileInfo, error)) (string, error) {
	v, ok := m.get("repo")
	if !ok {
		return "", m.invalidf("repo", "the project has no repo")
	}
	raw, _ := v.(string)
	path, err := ExpandHome(raw, lookupEnv)
	if err != nil {
		return "", fmt.Errorf("%s: %w", m.at("repo"), err)
	}
	if !filepath.IsAbs(path) {
		return "", m.invalidf("repo", "%q is not an absolute path", raw)
	}
	path = filepath.Clean(path)
	info, err := stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w: %w", m.at("repo"), ErrInvalid, err)
	}
	if !info.IsDir() {
		return "", m.invalidf("repo", "%s is not a directory", path)
	}
	return path, nil
}

// checkBundle is the D12 check that the Agentisan bundle is installed in the
// repo, so every session launched there loads its workflow skills.
func checkBundle(repo string, stat func(string) (fs.FileInfo, error), at string) error {
	marker := filepath.Join(repo, filepath.FromSlash(BundleMarker))
	info, err := stat(marker)
	if err != nil {
		return fmt.Errorf("%s: %w: no %s, so the Agentisan bundle is not installed there: %w", at, ErrBypassesAgentisan, BundleMarker, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: %w: %s is not a regular file, so it is not the Agentisan bundle's version file", at, ErrBypassesAgentisan, BundleMarker)
	}
	return nil
}

// buildProfile reads the merged, checked tree into a Profile. Every type
// was checked when its layer was loaded.
func buildProfile(name string, values map[string]any) *Profile {
	str := func(path string) string {
		v, _ := lookup(values, path)
		s, _ := v.(string)
		return s
	}
	p := &Profile{Name: name, Repo: str("repo"), Color: str("color"), GitHub: str("github")}
	p.Agent.Model = str("agent.model")
	if v, ok := lookup(values, "agent.command"); ok {
		p.Agent.Command, _ = v.([]string)
	}
	if v, ok := lookup(values, agentKey); ok {
		a, _ := v.(map[string]any)
		p.Providers = providerAgents(a)
	}
	if v, ok := lookup(values, "verify"); ok {
		p.Verify, _ = v.([][]string)
	}
	if v, ok := lookup(values, rotationPath); ok {
		p.Rotation.Thresholds, _ = v.([]int)
	}
	p.Spawn = buildSpawn(values)
	p.Routing = deploymentOf(values)
	if v, ok := lookup(values, "groups"); ok {
		groups, _ := v.(map[string]any)
		p.Groups = make(map[string]Group, len(groups))
		for n, raw := range groups {
			g, _ := raw.(map[string]any)
			workers, _ := g["workers"].(int)
			slots, _ := g["slots"].(int)
			description, _ := g["description"].(string)
			rotation, _ := lookup(g, rotationPath)
			thresholds, _ := rotation.([]int)
			agent, provider, byProvider := groupAgent(g)
			p.Groups[n] = Group{Workers: workers, Slots: slots, Agent: agent, Provider: provider, Providers: byProvider, Description: description, DescribesOnly: describesOnly(g), Rotation: Rotation{Thresholds: thresholds}}
		}
	}
	if v, ok := lookup(values, "integrations"); ok {
		integrations, _ := v.(map[string]any)
		p.Integrations = make(map[string]Integration, len(integrations))
		for n, raw := range integrations {
			in, _ := raw.(map[string]any)
			var out Integration
			if ref, ok := in["token"].(SecretRef); ok {
				out.Token = &ref
			}
			out.URL, _ = in["url"].(string)
			p.Integrations[n] = out
		}
	}
	return p
}

// Leaves returns every resolved leaf, sorted by path.
func (r Resolved) Leaves() []Leaf {
	out := make([]Leaf, 0, len(r.Sources))
	for _, path := range slices.Sorted(maps.Keys(r.Sources)) {
		v, _ := lookup(r.Values, path)
		out = append(out, Leaf{Path: path, Value: v, Layer: r.Sources[path]})
	}
	return out
}

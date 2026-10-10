package settings

import (
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
)

// merged is the outcome of merging sources, lowest first.
type merged struct {
	values map[string]any
	// sources maps every leaf path to its layer name.
	sources map[string]string
	// origins maps every leaf path to the source that set it, so an error
	// names the file as well as the layer.
	origins map[string]*source
}

// merge deep-merges tables and replaces everything else whole: scalars,
// lists and secret references are leaves. nil sources are skipped.
func merge(srcs ...*source) merged {
	m := merged{values: map[string]any{}, sources: map[string]string{}, origins: map[string]*source{}}
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

func (m merged) invalidf(path, format string, args ...any) error {
	return fmt.Errorf("%s: %w: %s", m.at(path), ErrInvalid, fmt.Sprintf(format, args...))
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
	for _, check := range []func() error{m.checkColor, m.checkGitHub, m.checkAgent, m.checkGroups, m.checkVerify, m.checkRotation} {
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
	return nil
}

func (m merged) checkGroups() error {
	v, _ := m.get("groups")
	groups, _ := v.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		g, _ := groups[name].(map[string]any)
		workersPath, slotsPath := "groups."+name+".workers", "groups."+name+".slots"
		workers, ok := g["workers"].(int)
		if !ok {
			return m.invalidf(workersPath, "every group needs workers")
		}
		if workers < 0 {
			return m.invalidf(workersPath, "workers %d is below 0", workers)
		}
		slots, ok := g["slots"].(int)
		if !ok {
			return m.invalidf(slotsPath, "every group needs slots")
		}
		if slots < workers {
			return m.invalidf(slotsPath, "slots %d is below workers %d", slots, workers)
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

func (m merged) checkRotation() error {
	v, _ := m.get("rotation.thresholds")
	ts, _ := v.([]int)
	const path = "rotation.thresholds"
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
	if v, ok := lookup(values, "verify"); ok {
		p.Verify, _ = v.([][]string)
	}
	if v, ok := lookup(values, "rotation.thresholds"); ok {
		p.Rotation.Thresholds, _ = v.([]int)
	}
	if v, ok := lookup(values, "groups"); ok {
		groups, _ := v.(map[string]any)
		p.Groups = make(map[string]Group, len(groups))
		for n, raw := range groups {
			g, _ := raw.(map[string]any)
			workers, _ := g["workers"].(int)
			slots, _ := g["slots"].(int)
			p.Groups[n] = Group{Workers: workers, Slots: slots}
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

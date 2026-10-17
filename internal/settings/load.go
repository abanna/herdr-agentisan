package settings

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// flagOrigin is how errors name the --set layer.
const flagOrigin = "--set"

// bareKey is a TOML bare key: the alphabet of one --set path segment.
var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// defaults is layer 1: the built-in values every profile starts from.
func defaults() *source {
	return &source{layer: LayerDefault, origin: "built-in", values: map[string]any{
		"agent":    map[string]any{"command": []string{"claude"}},
		"rotation": map[string]any{"thresholds": []int{55, 65, 75}},
	}}
}

// Load reads the shared file and, when a project is selected, its repo file,
// and resolves them with the --set overrides. Every layer is checked against
// the schema before anything merges, so an error names the layer it is in;
// the field rules and D12 then run on the merged values.
//
// With no project selected it resolves the defaults, the shared tables and
// the overrides, and lists the projects; every project still has to pass the
// field rules and the D12 command checks, so a bare resolve lints the whole
// file. A missing shared or repo file is not an error.
func Load(opts LoadOptions) (Resolved, error) {
	if opts.ConfigDir == "" {
		return Resolved{}, ErrNoConfigDir
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}

	sharedPath := filepath.Join(opts.ConfigDir, FileName)
	shared, projects, err := loadShared(sharedPath)
	if err != nil {
		return Resolved{}, err
	}
	flags, err := parseFlags(opts.Set)
	if err != nil {
		return Resolved{}, err
	}
	names := slices.Sorted(maps.Keys(projects))
	if names == nil {
		names = []string{}
	}

	if opts.Project == "" {
		m := merge(slices.Concat([]*source{defaults(), shared}, flags)...)
		if err := m.checkFields(); err != nil {
			return Resolved{}, err
		}
		if err := m.spaces(names); err != nil {
			return Resolved{}, err
		}
		// Lint every project too, short of the repo checks, which touch
		// the filesystem and wait for a selection.
		for _, name := range names {
			if err := merge(slices.Concat([]*source{defaults(), shared, projects[name]}, flags)...).checkFields(); err != nil {
				return Resolved{}, err
			}
		}
		return Resolved{Values: m.values, Sources: m.sources, Projects: names}, nil
	}

	project, ok := projects[opts.Project]
	if !ok {
		known := "none"
		if len(names) > 0 {
			known = strings.Join(names, ", ")
		}
		return Resolved{}, fmt.Errorf("%w %q in %s; known projects: %s", ErrNoProject, opts.Project, sharedPath, known)
	}
	layers := []*source{defaults(), shared, project}
	// The repo file is found through repo, so repo resolves from every
	// layer but that one first.
	repo, err := merge(slices.Concat(layers, flags)...).repo(opts.LookupEnv, opts.Stat)
	if err != nil {
		return Resolved{}, err
	}
	repoFile, err := loadRepoFile(filepath.Join(repo, RepoFileName))
	if err != nil {
		return Resolved{}, err
	}
	m := merge(slices.Concat(layers, []*source{repoFile}, flags)...)
	m.values["repo"] = repo
	if err := m.checkFields(); err != nil {
		return Resolved{}, err
	}
	if err := m.spaces(names); err != nil {
		return Resolved{}, err
	}
	if err := checkBundle(repo, opts.Stat, m.at("repo")); err != nil {
		return Resolved{}, err
	}
	return Resolved{
		Project:  opts.Project,
		Profile:  buildProfile(opts.Project, m.values),
		Values:   m.values,
		Sources:  m.sources,
		Projects: names,
	}, nil
}

// readTOML reads and decodes path and checks its schema_version, which it
// then drops. found is false when the file does not exist.
func readTOML(path string) (doc map[string]any, found bool, err error) {
	raw, err := readRegular(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, false, parseError(path, err)
	}
	if err := checkSchemaVersion(path, doc); err != nil {
		return nil, false, err
	}
	delete(doc, "schema_version")
	return doc, true, nil
}

// readRegular reads path, which must be a regular file of at most
// MaxFileBytes. The type is checked on the opened descriptor, so a path
// swapped after the check cannot be read in its place, and a FIFO or device
// is refused without being read.
func readRegular(path string) ([]byte, error) {
	f, err := openNonBlocking(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w: not a regular file", path, ErrInvalid)
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(raw) > MaxFileBytes {
		return nil, fmt.Errorf("%s: %w: larger than %d bytes", path, ErrInvalid, MaxFileBytes)
	}
	return raw, nil
}

// parseError reports where a file fails to parse. It uses the decoder's
// message only, never the contextual excerpt, which would echo the line.
func parseError(path string, err error) error {
	if de, ok := errors.AsType[*toml.DecodeError](err); ok {
		row, col := de.Position()
		return fmt.Errorf("%s: %w: line %d, column %d: %s", path, ErrInvalid, row, col, de.Error())
	}
	return fmt.Errorf("%s: %w: %w", path, ErrInvalid, err)
}

func checkSchemaVersion(path string, doc map[string]any) error {
	v, ok := doc["schema_version"]
	if !ok {
		return fmt.Errorf("%s: %w: schema_version is missing; this binary reads %d", path, ErrUnsupportedSchema, SchemaVersion)
	}
	if n, ok := v.(int64); !ok || n != SchemaVersion {
		return fmt.Errorf("%s: %w: schema_version is %s; this binary reads %d", path, ErrUnsupportedSchema, FormatValue(v), SchemaVersion)
	}
	return nil
}

// loadShared reads the shared file into its shared layer (every top-level
// table but projects) and one layer for each project. Every project is
// checked, selected or not, so a bare `config resolve` lints the whole file.
func loadShared(path string) (*source, map[string]*source, error) {
	projects := map[string]*source{}
	doc, found, err := readTOML(path)
	if err != nil || !found {
		return nil, projects, err
	}
	shared := &source{layer: LayerShared, origin: path, schema: sharedSchema}
	raw, hasProjects := doc["projects"]
	delete(doc, "projects")
	if err := shared.load(doc); err != nil {
		return nil, nil, err
	}
	if !hasProjects {
		return shared, projects, nil
	}
	table, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("projects (%s): %w: want a table, got %s", path, ErrInvalid, tomlType(raw))
	}
	for _, name := range slices.Sorted(maps.Keys(table)) {
		p := &source{layer: LayerProject, origin: path, prefix: "projects." + name + "."}
		if !namePattern.MatchString(name) {
			return nil, nil, fmt.Errorf("%s: %w: a project name must match %s", p.at(""), ErrInvalid, namePattern)
		}
		body, ok := table[name].(map[string]any)
		if !ok {
			return nil, nil, p.wrongType("", "a table", table[name])
		}
		if err := p.load(body); err != nil {
			return nil, nil, err
		}
		projects[name] = p
	}
	return shared, projects, nil
}

// loadRepoFile reads a project's overlay; nil when there is none.
func loadRepoFile(path string) (*source, error) {
	doc, found, err := readTOML(path)
	if err != nil || !found {
		return nil, err
	}
	s := &source{layer: LayerRepo, origin: path}
	if _, ok := doc["repo"]; ok {
		return nil, fmt.Errorf("%s: %w: the repo file is found through repo, so it cannot set it", s.at("repo"), ErrUnknownKey)
	}
	if err := s.load(doc); err != nil {
		return nil, err
	}
	return s, nil
}

// parseFlags turns each --set into a layer of its own, applied in order.
func parseFlags(sets []string) ([]*source, error) {
	out := make([]*source, 0, len(sets))
	for _, arg := range sets {
		path, value, err := ParseSet(arg)
		if err != nil {
			return nil, err
		}
		segs := strings.Split(path, ".")
		raw := value
		for _, seg := range slices.Backward(segs) {
			raw = map[string]any{seg: raw}
		}
		s := &source{layer: LayerFlag, origin: flagOrigin, schema: sharedSchema}
		tree, _ := raw.(map[string]any)
		if err := s.load(tree); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ParseSet splits a --set argument at its first = into a dotted key path and
// a value. The value is decoded as TOML (3, "opus", [50, 60, 70],
// { env = "X" }); anything that does not decode as exactly one TOML value is
// taken as a plain string, so --set agent.model=opus works unquoted. Errors
// name the key path, never the value.
func ParseSet(arg string) (string, any, error) {
	path, text, ok := strings.Cut(arg, "=")
	path = strings.TrimSpace(path)
	if !ok || path == "" {
		return "", nil, fmt.Errorf("%s: %w: want key.path=value", flagOrigin, ErrInvalid)
	}
	for seg := range strings.SplitSeq(path, ".") {
		if !bareKey.MatchString(seg) {
			return "", nil, fmt.Errorf("%s %s: %w: each part of a key path must match %s", flagOrigin, path, ErrInvalid, bareKey)
		}
	}
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) {
		// A file cannot hold one either: TOML is UTF-8.
		return "", nil, fmt.Errorf("%s %s: %w: the value is not valid UTF-8", flagOrigin, path, ErrInvalid)
	}
	var doc map[string]any
	if err := toml.Unmarshal([]byte("v = "+text), &doc); err != nil || len(doc) != 1 {
		// Not one TOML value: a plain string, by design.
		return path, text, nil
	}
	v, ok := doc["v"]
	if !ok {
		return path, text, nil
	}
	return path, v, nil
}

// ExpandHome expands a leading ~ or ~/ in path with HOME, read through
// lookupEnv (os.LookupEnv when nil). Any other path, ~user included, is
// returned unchanged. HOME must be set and absolute.
func ExpandHome(path string, lookupEnv func(string) (string, bool)) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	home, _ := lookupEnv("HOME")
	if home == "" {
		return "", fmt.Errorf("%w: %s starts with ~ but HOME is not set", ErrInvalid, path)
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("%w: HOME %q is not an absolute path", ErrInvalid, home)
	}
	return filepath.Join(home, strings.TrimPrefix(path[1:], "/")), nil
}

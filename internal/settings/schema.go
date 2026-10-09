package settings

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// kind is the shape a schema node accepts.
type kind int

const (
	// kindTable has fixed keys, each with a node of its own.
	kindTable kind = iota
	// kindNames has any key matching namePattern, every value of elem.
	kindNames
	kindString
	kindInt
	kindStrings
	kindInts
	kindArgvs
	kindSecret
)

// node describes one key path of the profile. Tables and name maps are the
// only interior nodes; everything else, a secret reference included, is a
// leaf that a higher layer replaces whole.
type node struct {
	kind   kind
	fields map[string]*node
	elem   *node
	// secrets marks a subtree in which every key named in secretNames is a
	// secret field, wherever it sits.
	secrets bool
}

// secretNames are the keys that hold a secret anywhere under integrations.
var secretNames = map[string]bool{"token": true, "api_key": true, "password": true, "secret": true}

func leafOf(k kind) *node { return &node{kind: k} }

// profileSchema is the body of a profile: the shared tables, a
// [projects.<name>] table, a repo file and --set all share it.
var profileSchema = &node{kind: kindTable, fields: map[string]*node{
	"repo":   leafOf(kindString),
	"color":  leafOf(kindString),
	"github": leafOf(kindString),
	"agent": {kind: kindTable, fields: map[string]*node{
		"command": leafOf(kindStrings),
		"model":   leafOf(kindString),
	}},
	"groups": {kind: kindNames, elem: &node{kind: kindTable, fields: map[string]*node{
		"workers": leafOf(kindInt),
		"slots":   leafOf(kindInt),
	}}},
	"verify": leafOf(kindArgvs),
	"integrations": {kind: kindNames, secrets: true, elem: &node{kind: kindTable, fields: map[string]*node{
		"token": leafOf(kindSecret),
		"url":   leafOf(kindString),
	}}},
	"rotation": {kind: kindTable, fields: map[string]*node{
		"thresholds": leafOf(kindInts),
	}},
}}

// source is one layer's values and where they were read from.
type source struct {
	// layer is the provenance recorded for every leaf this source sets.
	layer string
	// origin is the file the values came from, or "--set".
	origin string
	// prefix is the key path of the values inside origin, such as
	// "projects.app." for a project.
	prefix string
	// values is the normalised tree: tables are map[string]any, leaves are
	// string, int, []string, []int, [][]string or SecretRef.
	values map[string]any
}

// at names a key path the way errors report it: as written in the origin,
// followed by the layer and the origin.
func (s *source) at(path string) string {
	return fmt.Sprintf("%s (%s, %s)", strings.TrimSuffix(s.prefix+path, "."), s.layer, s.origin)
}

// load checks raw against the profile schema and stores its normalised form.
func (s *source) load(raw map[string]any) error {
	v, err := s.normalize(profileSchema, raw, "")
	if err != nil {
		return err
	}
	s.values, _ = v.(map[string]any)
	return nil
}

// normalize checks raw against n and converts it to the leaf types the
// resolver works with. Errors name the key path in s.
func (s *source) normalize(n *node, raw any, path string) (any, error) {
	if n.secrets {
		// Before the schema walk, so a pasted secret is reported as one
		// even under a key the schema does not know.
		if err := s.scanSecrets(raw, path); err != nil {
			return nil, err
		}
	}
	switch n.kind {
	case kindTable:
		return s.table(n, raw, path)
	case kindNames:
		return s.names(n, raw, path)
	case kindSecret:
		ref, err := ParseSecretRef(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.at(path), err)
		}
		return ref, nil
	case kindString:
		return scalarOf(s, raw, path, "a string", asString)
	case kindInt:
		return scalarOf(s, raw, path, "an integer", asInt)
	case kindStrings:
		return listOf(s, raw, path, "a list of strings", asString)
	case kindInts:
		return listOf(s, raw, path, "a list of integers", asInt)
	default: // kindArgvs
		return listOf(s, raw, path, "a list of argv lists", asStrings)
	}
}

func (s *source) table(n *node, raw any, path string) (map[string]any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, s.wrongType(path, "a table", raw)
	}
	out := make(map[string]any, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		p := join(path, k)
		child, known := n.fields[k]
		if !known {
			return nil, fmt.Errorf("%s: %w", s.at(p), ErrUnknownKey)
		}
		v, err := s.normalize(child, m[k], p)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func (s *source) names(n *node, raw any, path string) (map[string]any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, s.wrongType(path, "a table", raw)
	}
	out := make(map[string]any, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		p := join(path, k)
		if !namePattern.MatchString(k) {
			return nil, fmt.Errorf("%s: %w: a name must match %s", s.at(p), ErrInvalid, namePattern)
		}
		v, err := s.normalize(n.elem, m[k], p)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// scanFrame is one value scanSecrets has yet to visit.
type scanFrame struct {
	v      any
	key    string // empty for an array item
	parent *scanFrame
	secret bool
}

// scanSecrets checks every key named in secretNames, at any depth under raw,
// holds a reference. It walks with a stack instead of recursion and joins a
// key path only to report it, so a deep file costs time and memory in its
// size, not its depth squared.
func (s *source) scanSecrets(raw any, path string) error {
	stack := []*scanFrame{{v: raw}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.secret {
			if _, err := ParseSecretRef(f.v); err != nil {
				return fmt.Errorf("%s: %w", s.at(f.path(path)), err)
			}
			continue
		}
		switch v := f.v.(type) {
		case map[string]any:
			// Pushed in reverse, so keys are visited in sorted order.
			for _, k := range slices.Backward(slices.Sorted(maps.Keys(v))) {
				stack = append(stack, &scanFrame{v: v[k], key: k, parent: f, secret: secretNames[k]})
			}
		case []any:
			for _, item := range slices.Backward(v) {
				stack = append(stack, &scanFrame{v: item, parent: f})
			}
		}
	}
	return nil
}

// path joins the keys from the scan's root down to f under base.
func (f *scanFrame) path(base string) string {
	var keys []string
	for ; f != nil; f = f.parent {
		if f.key != "" {
			keys = append(keys, f.key)
		}
	}
	slices.Reverse(keys)
	return join(base, strings.Join(keys, "."))
}

// scalarOf converts a TOML value conv accepts.
func scalarOf[T any](s *source, raw any, path, want string, conv func(any) (T, bool)) (T, error) {
	v, ok := conv(raw)
	if !ok {
		var zero T
		return zero, s.wrongType(path, want, raw)
	}
	return v, nil
}

// listOf converts a TOML array whose every item conv accepts.
func listOf[T any](s *source, raw any, path, want string, conv func(any) (T, bool)) ([]T, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, s.wrongType(path, want, raw)
	}
	out := make([]T, 0, len(items))
	for i, item := range items {
		v, ok := conv(item)
		if !ok {
			return nil, fmt.Errorf("%s: %w: want %s, but item %d is %s", s.at(path), ErrInvalid, want, i+1, tomlType(item))
		}
		out = append(out, v)
	}
	return out, nil
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// asInt refuses a value int cannot hold, which only a 32-bit target has.
func asInt(v any) (int, bool) {
	i, ok := v.(int64)
	if !ok || int64(int(i)) != i {
		return 0, false
	}
	return int(i), true
}

func asStrings(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// wrongType reports a value of the wrong TOML type. It names the type, never
// the value, which may be a secret.
func (s *source) wrongType(path, want string, raw any) error {
	return fmt.Errorf("%s: %w: want %s, got %s", s.at(path), ErrInvalid, want, tomlType(raw))
}

// tomlType names the TOML type of a decoded value.
func tomlType(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case int64, int:
		return "an integer"
	case float64:
		return "a float"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "a table"
	default:
		return "a date or time"
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

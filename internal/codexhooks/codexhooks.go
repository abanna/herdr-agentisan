// Package codexhooks installs the Codex hooks that report each Codex
// agent's context use to its herdr pane (ADR-001 A22, A27, A43): async
// PostToolUse (every tool), Stop and PostCompact hooks that run
// `herdr-agentisan report codex`, in Codex's user hooks file,
// $CODEX_HOME/hooks.json.
//
// Codex only reads that file, so writing it never competes with Codex's own
// writes to config.toml, where it records the hooks the user trusts. Other
// tools share it: herdr's own Codex integration keeps a SessionStart hook
// there. Every entry that is not ours is kept as it was, and ours go last in
// each event, because Codex keys its trust by a hook's file and position.
//
// Nothing here trusts a hook. Codex runs a hook only once the user has
// trusted it, at startup or in /hooks, and asks again when it changes.
package codexhooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

const (
	// FileName is Codex's user hooks file, in its home directory.
	FileName = "hooks.json"
	// MaxFileSize caps the hooks file and config.toml this reads.
	MaxFileSize = 1 << 20

	configName = "config.toml"
	// command is what each hook runs; the docs' sample runs the same.
	command = "herdr-agentisan report codex"
	// timeout is each hook's timeout in seconds.
	timeout = 10
	// toolEvent is the one event whose group has a matcher: every tool.
	toolEvent = "PostToolUse"
	// commandType is a command hook's type.
	commandType = "command"
)

// events are the Codex events `report codex` runs from, in the order a
// result lists them. Never SessionEnd: Codex runs it whenever it unloads a
// thread (A27).
var events = []string{toolEvent, "Stop", "PostCompact"}

// topKeys are the keys Codex allows at the top of a hooks file: it ignores
// a file with any other.
var topKeys = []string{"description", "hooks"}

var (
	// ErrNoHome means Codex's home directory cannot be found: CODEX_HOME
	// names no directory, or neither it nor an absolute HOME is set.
	ErrNoHome = errors.New("no Codex home directory")
	// ErrInvalid means a file is not one this can read or update: not a
	// regular file, too large, not valid JSON or TOML, or in a shape Codex
	// itself refuses.
	ErrInvalid = errors.New("not a Codex file this can update")
	// ErrAmbiguous means an event runs `report codex` beside another hook in
	// one group, or twice: which one is ours is the user's to say.
	ErrAmbiguous = errors.New("ambiguous report codex hook")
	// ErrInConfigTOML means config.toml already runs `report codex`. A copy
	// in hooks.json as well would run every report twice.
	ErrInConfigTOML = errors.New("report codex hooks are already in config.toml")
	// ErrUnwritable means the hooks file cannot be replaced safely: it has
	// other hard links, which a rename would leave behind, or the user may
	// not write it, which a rename in a writable directory would bypass.
	ErrUnwritable = errors.New("hooks file cannot be replaced safely")
	// ErrNotInstalled means not every hook is present as this writes it.
	ErrNotInstalled = errors.New("report codex hooks are not all installed")
)

// State is what an install did to one event's hook, or what a check found.
type State string

const (
	// Added means the event had no `report codex` hook, and now has one.
	Added State = "added"
	// Updated means its hook was replaced in place, keeping its command.
	Updated State = "updated"
	// Unchanged means its hook was already as this writes it.
	Unchanged State = "unchanged"
	// Present means a check found the hook as this writes it.
	Present State = "present"
	// Differs means a check found a `report codex` hook that is not as this
	// writes it: not async, another timeout or matcher, or a field more.
	Differs State = "differs"
	// Missing means a check found no `report codex` hook for the event.
	Missing State = "missing"
)

// Hook is one event's outcome.
type Hook struct {
	Event string `json:"event"`
	State State  `json:"state"`
	// Source is the file that holds the hook; "" when there is none.
	Source string `json:"source,omitempty"`
}

// Result is an install's or a check's outcome.
type Result struct {
	// Path is the hooks file.
	Path string `json:"path"`
	// Changed reports that the install wrote it.
	Changed bool   `json:"changed"`
	Hooks   []Hook `json:"hooks"`
	// HooksOff reports features.hooks = false in config.toml: Codex then
	// runs no hook at all, ours included.
	HooksOff bool `json:"hooks_off"`
}

// Installed reports whether every event has its hook as this writes it.
func (r Result) Installed() bool {
	if len(r.Hooks) != len(events) {
		return false
	}
	for _, h := range r.Hooks {
		switch h.State {
		case Added, Updated, Unchanged, Present:
		default:
			return false
		}
	}
	return true
}

// Group is one of Codex's matcher groups: the hooks an event runs, for the
// tools its matcher names.
type Group struct {
	Matcher string    `json:"matcher,omitempty" toml:"matcher"`
	Hooks   []Handler `json:"hooks"             toml:"hooks"`
}

// Handler is one command hook.
type Handler struct {
	Type    string `json:"type"    toml:"type"`
	Command string `json:"command" toml:"command"`
	Timeout int    `json:"timeout" toml:"timeout"`
	Async   bool   `json:"async"   toml:"async"`
}

// Hooks returns the group each event runs `report codex` from, as an
// install writes it.
func Hooks() map[string]Group {
	out := make(map[string]Group, len(events))
	for _, e := range events {
		out[e] = group(e, command)
	}
	return out
}

// group is event's hook, running cmd: an install keeps the command an
// existing hook already runs, which may name the binary by its path.
func group(event, cmd string) Group {
	g := Group{Hooks: []Handler{{Type: commandType, Command: cmd, Timeout: timeout, Async: true}}}
	if event == toolEvent {
		g.Matcher = "*"
	}
	return g
}

// Home is Codex's home directory, as Codex finds it: CODEX_HOME, which must
// be a directory, else ~/.codex, which need not exist yet.
func Home(lookup func(string) (string, bool)) (string, error) {
	if v, ok := lookup("CODEX_HOME"); ok && v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("%w: CODEX_HOME %s: %w", ErrNoHome, v, err)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("%w: CODEX_HOME: %w", ErrNoHome, err)
		}
		if !st.IsDir() {
			return "", fmt.Errorf("%w: CODEX_HOME %s is not a directory", ErrNoHome, abs)
		}
		return abs, nil
	}
	home, ok := lookup("HOME")
	if !ok || !filepath.IsAbs(home) {
		return "", fmt.Errorf("%w: neither CODEX_HOME nor an absolute HOME is set", ErrNoHome)
	}
	return filepath.Join(home, ".codex"), nil
}

// Install adds each event's hook to home's hooks file, or brings it in line
// in place, and writes the file only when that changed something. It
// refuses, changing nothing, when config.toml already runs `report codex`
// or the file is one it cannot update safely. It creates home if needed.
func Install(home string) (Result, error) {
	path := filepath.Join(home, FileName)
	res := Result{Path: path}
	cfg, err := readConfig(home)
	if err != nil {
		return res, err
	}
	res.HooksOff = cfg.hooksOff
	if len(cfg.found) > 0 {
		res.Hooks = cfg.hooks()
		return res, fmt.Errorf("%w: %s runs `%s` from its %s hooks; remove them there and install again, or keep them there and leave %s out",
			ErrInConfigTOML, cfg.path, command, strings.Join(cfg.found, ", "), FileName)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return res, fmt.Errorf("create %s: %w", home, err)
	}
	f, err := load(path)
	if err != nil {
		return res, err
	}
	doc, err := parse(path, f.data)
	if err != nil {
		return res, err
	}
	changed := false
	for _, e := range events {
		st, err := doc.install(path, e)
		if err != nil {
			return res, err
		}
		res.Hooks = append(res.Hooks, Hook{Event: e, State: st, Source: path})
		changed = changed || st != Unchanged
	}
	if !changed {
		return res, nil
	}
	data, err := doc.encode()
	if err != nil {
		return res, err
	}
	if err := f.replace(data); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// InstalledIn reports whether Codex, in the home lookup names (Home), runs
// every hook as Install writes it: each one installed, and hooks not turned
// off in config.toml. It writes nothing.
func InstalledIn(lookup func(string) (string, bool)) (bool, error) {
	home, err := Home(lookup)
	if err != nil {
		return false, err
	}
	res, err := Check(home)
	if err != nil {
		return false, err
	}
	return res.Installed() && !res.HooksOff, nil
}

// Check reports each event's hook, in the hooks file or else in
// config.toml, and writes nothing. A home that does not exist has none.
func Check(home string) (Result, error) {
	path := filepath.Join(home, FileName)
	res := Result{Path: path}
	cfg, err := readConfig(home)
	if err != nil {
		return res, err
	}
	res.HooksOff = cfg.hooksOff
	f, err := load(path)
	if err != nil {
		return res, err
	}
	doc, err := parse(path, f.data)
	if err != nil {
		return res, err
	}
	for _, e := range events {
		st, err := doc.check(path, e)
		if err != nil {
			return res, err
		}
		h := Hook{Event: e, State: st, Source: path}
		if st == Missing {
			h.Source = ""
			if s, ok := cfg.ours[e]; ok {
				h.State, h.Source = s, cfg.path
			}
		}
		res.Hooks = append(res.Hooks, h)
	}
	return res, nil
}

// isReportCodex reports whether a hook command runs `herdr-agentisan report
// codex`, by any path and with anything after it, such as a redirect.
func isReportCodex(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) >= 3 && filepath.Base(f[0]) == "herdr-agentisan" && f[1] == "report" && f[2] == "codex"
}

// file is the hooks file as read.
type file struct {
	// path is the file as Codex names it; target is where it is written,
	// the file its symlinks lead to.
	path, target string
	data         []byte
	mode         fs.FileMode
	exists       bool
	// linked reports more than one hard link.
	linked bool
}

// load reads the hooks file, following its symlinks. A file that does not
// exist is empty; a dangling symlink is an error, so nothing is created
// where it points.
func load(path string) (file, error) {
	f := file{path: path, target: path, mode: 0o600}
	lst, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("read %s: %w", path, err)
	}
	if lst.Mode().Type() == fs.ModeSymlink {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return f, fmt.Errorf("follow %s: %w", path, err)
		}
		f.target = target
	}
	st, err := os.Stat(f.target)
	if err != nil {
		return f, fmt.Errorf("read %s: %w", path, err)
	}
	// Checked before it is opened: opening a FIFO waits for a writer.
	if !st.Mode().IsRegular() {
		return f, fmt.Errorf("%w: %s is not a regular file", ErrInvalid, path)
	}
	data, err := readCapped(path, f.target)
	if err != nil {
		return f, err
	}
	f.data, f.mode, f.exists = data, st.Mode().Perm(), true
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		f.linked = s.Nlink > 1
	}
	return f, nil
}

// readCapped reads the regular file at target, which path names, refusing
// one over MaxFileSize.
func readCapped(path, target string) ([]byte, error) {
	// #nosec G304 -- the path is a file of the user's own Codex home.
	r, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrInvalid, path, MaxFileSize)
	}
	return data, nil
}

// replace writes data over the file through a temporary file in its
// directory and a rename, so Codex never reads half a file, with the file's
// own mode, or 0600 for a new one.
func (f file) replace(data []byte) (err error) {
	if f.exists {
		if f.linked {
			return fmt.Errorf("%w: %s has other hard links, which a replaced file would leave behind; make it a single file or a symlink", ErrUnwritable, f.path)
		}
		// A rename replaces any file its directory lets the user write,
		// whatever the file's own mode, so the file is opened for writing
		// first, as herdr's own installer does.
		// #nosec G304 -- the path is Codex's own hooks file in the user's Codex home.
		w, err := os.OpenFile(f.target, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnwritable, f.path, err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("close %s: %w", f.path, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.target), "."+filepath.Base(f.target)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := tmp.Chmod(f.mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	if err := os.Rename(tmp.Name(), f.target); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	return nil
}

// document is a hooks file, decoded only as deep as an install needs: every
// value it does not change is kept as written.
type document struct {
	top   map[string]json.RawMessage
	hooks map[string]json.RawMessage
	// changed holds the events an install rewrote.
	changed map[string][]json.RawMessage
}

func invalidf(path, format string, args ...any) error {
	return fmt.Errorf("%w: %s %s", ErrInvalid, path, fmt.Sprintf(format, args...))
}

// parse decodes a hooks file. An empty one, or one of only white space, is
// a file with no hooks.
func parse(path string, data []byte) (*document, error) {
	d := &document{top: map[string]json.RawMessage{}, hooks: map[string]json.RawMessage{}, changed: map[string][]json.RawMessage{}}
	if !utf8.Valid(data) {
		return nil, invalidf(path, "is not UTF-8")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return d, nil
	}
	if !decodeObject(data, &d.top) {
		return nil, invalidf(path, "is not a JSON object")
	}
	for k := range d.top {
		if !slices.Contains(topKeys, k) {
			return nil, invalidf(path, "has the key %q; Codex reads only %s, and ignores a file with any other", k, strings.Join(topKeys, " and "))
		}
	}
	if raw, ok := d.top["hooks"]; ok && !decodeObject(raw, &d.hooks) {
		return nil, invalidf(path, "has hooks that are not an object")
	}
	return d, nil
}

// decodeObject decodes raw into out when it is a JSON object, and not null.
func decodeObject(raw []byte, out *map[string]json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Unmarshal(trimmed, out) == nil
}

// groups returns event's groups; none when it has no entry or a null one.
func (d *document) groups(path, event string) ([]json.RawMessage, error) {
	raw, ok := d.hooks[event]
	trimmed := bytes.TrimSpace(raw)
	if !ok || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var out []json.RawMessage
	if len(trimmed) == 0 || trimmed[0] != '[' || json.Unmarshal(trimmed, &out) != nil {
		return nil, invalidf(path, "has %s hooks that are not a list", event)
	}
	return out, nil
}

// handler is the part of a hook handler that says whether it is ours.
type handler struct {
	Type    *string `json:"type"`
	Command *string `json:"command"`
}

// findOurs returns the index of event's one group that runs `report codex`
// and nothing else, and its command; -1 when there is none.
func findOurs(path, event string, groups []json.RawMessage) (int, string, error) {
	at, cmd := -1, ""
	for i, raw := range groups {
		var g struct {
			Matcher *string           `json:"matcher"`
			Hooks   []json.RawMessage `json:"hooks"`
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &g) != nil {
			return -1, "", invalidf(path, "has a %s group that is not an object with a string matcher and a list of hooks", event)
		}
		mine := ""
		for _, h := range g.Hooks {
			var hd handler
			th := bytes.TrimSpace(h)
			if len(th) == 0 || th[0] != '{' || json.Unmarshal(th, &hd) != nil {
				return -1, "", invalidf(path, "has a %s hook that is not an object with a string type and command", event)
			}
			if hd.Type != nil && *hd.Type == commandType && hd.Command != nil && isReportCodex(*hd.Command) {
				mine = *hd.Command
			}
		}
		switch {
		case mine == "":
			continue
		case len(g.Hooks) > 1:
			return -1, "", fmt.Errorf("%w: %s runs `%s` beside other hooks in one %s group; give it a group of its own", ErrAmbiguous, path, command, event)
		case at >= 0:
			return -1, "", fmt.Errorf("%w: %s runs `%s` from %s twice; keep one", ErrAmbiguous, path, command, event)
		}
		at, cmd = i, mine
	}
	return at, cmd, nil
}

// install adds or updates event's hook.
func (d *document) install(path, event string) (State, error) {
	groups, err := d.groups(path, event)
	if err != nil {
		return "", err
	}
	at, cmd, err := findOurs(path, event, groups)
	if err != nil {
		return "", err
	}
	if at < 0 {
		raw, err := marshal(group(event, command))
		if err != nil {
			return "", err
		}
		d.changed[event] = append(groups, raw)
		return Added, nil
	}
	want, err := marshal(group(event, cmd))
	if err != nil {
		return "", err
	}
	if sameJSON(groups[at], want) {
		return Unchanged, nil
	}
	groups[at] = want
	d.changed[event] = groups
	return Updated, nil
}

// check reports event's hook.
func (d *document) check(path, event string) (State, error) {
	groups, err := d.groups(path, event)
	if err != nil {
		return "", err
	}
	at, cmd, err := findOurs(path, event, groups)
	if err != nil {
		return "", err
	}
	if at < 0 {
		return Missing, nil
	}
	want, err := marshal(group(event, cmd))
	if err != nil {
		return "", err
	}
	if sameJSON(groups[at], want) {
		return Present, nil
	}
	return Differs, nil
}

// encode writes the document back: two-space indents, keys sorted, values
// it did not change as written, and no HTML escaping, which Codex would read
// the same but would change the user's own text.
func (d *document) encode() ([]byte, error) {
	hooks := make(map[string]any, len(d.hooks)+len(d.changed))
	for k, v := range d.hooks {
		hooks[k] = v
	}
	for k, v := range d.changed {
		hooks[k] = v
	}
	top := make(map[string]any, len(d.top)+1)
	for k, v := range d.top {
		top[k] = v
	}
	top["hooks"] = hooks
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(top); err != nil {
		return nil, fmt.Errorf("encode %s: %w", FileName, err)
	}
	return buf.Bytes(), nil
}

// marshal encodes v compactly, without HTML escaping.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode a hook: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sameJSON reports whether a and b decode to the same value: layout, key
// order and number spelling aside.
func sameJSON(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// config is what config.toml says about our hooks.
type config struct {
	path string
	// found lists the events, any of Codex's, that run `report codex`.
	found []string
	// ours is the state of each of our events that runs it there.
	ours     map[string]State
	hooksOff bool
}

// hooks lists our events' states in config.toml.
func (c config) hooks() []Hook {
	out := make([]Hook, 0, len(events))
	for _, e := range events {
		h := Hook{Event: e, State: Missing}
		if s, ok := c.ours[e]; ok {
			h.State, h.Source = s, c.path
		}
		out = append(out, h)
	}
	return out
}

// readConfig reads home's config.toml, which it never writes. A file that
// does not exist says nothing.
func readConfig(home string) (config, error) {
	c := config{path: filepath.Join(home, configName), ours: map[string]State{}}
	st, err := os.Stat(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read %s: %w", c.path, err)
	}
	if !st.Mode().IsRegular() {
		return c, fmt.Errorf("%w: %s is not a regular file", ErrInvalid, c.path)
	}
	data, err := readCapped(c.path, c.path)
	if err != nil {
		return c, err
	}
	var v map[string]any
	if err := toml.Unmarshal(data, &v); err != nil {
		return c, fmt.Errorf("%w: %s: %w", ErrInvalid, c.path, err)
	}
	if features, ok := v["features"].(map[string]any); ok {
		if on, ok := features["hooks"].(bool); ok && !on {
			c.hooksOff = true
		}
	}
	hooks, _ := v["hooks"].(map[string]any)
	for _, event := range slices.Sorted(maps.Keys(hooks)) {
		groups, _ := hooks[event].([]any)
		for _, g := range groups {
			gm, ok := g.(map[string]any)
			if !ok {
				continue
			}
			handlers, _ := gm["hooks"].([]any)
			cmd := ""
			for _, h := range handlers {
				hm, _ := h.(map[string]any)
				if t, _ := hm["type"].(string); t == commandType {
					if s, _ := hm["command"].(string); isReportCodex(s) {
						cmd = s
					}
				}
			}
			if cmd == "" {
				continue
			}
			if !slices.Contains(c.found, event) {
				c.found = append(c.found, event)
			}
			if !slices.Contains(events, event) {
				continue
			}
			st := Differs
			if len(handlers) == 1 && sameTOML(gm, group(event, cmd)) {
				st = Present
			}
			if c.ours[event] != Present {
				c.ours[event] = st
			}
		}
	}
	return c, nil
}

// sameTOML reports whether a decoded TOML group is want.
func sameTOML(g map[string]any, want Group) bool {
	a, err := json.Marshal(g)
	if err != nil {
		return false
	}
	b, err := marshal(want)
	if err != nil {
		return false
	}
	return sameJSON(a, b)
}

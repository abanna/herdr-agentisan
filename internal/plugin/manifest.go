package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ErrInvalidManifest means herdr-plugin.toml parsed but breaks herdr's rules,
// or did not parse at all. A missing file is reported separately.
var ErrInvalidManifest = errors.New("invalid herdr-plugin.toml")

// ManifestFile is the manifest's name at the plugin root.
const ManifestFile = "herdr-plugin.toml"

// ID is the plugin id herdr-plugin.toml declares. Code that names this plugin
// to herdr (plugin.pane.open) uses it rather than HERDR_PLUGIN_ID, which is
// unset when the binary runs from a shell rather than from herdr.
const ID = "nerdsrun.agentisan"

// BtopPane is the [[panes]] entrypoint that runs btop in a herdr popup.
const BtopPane = "btop"

// Manifest is the subset of herdr-plugin.toml this plugin declares. Decoding
// is strict: a key this struct does not know is a typo until proven
// otherwise, so adding a section (events, panes, ...) means adding it here.
type Manifest struct {
	ID              string    `toml:"id"`
	Name            string    `toml:"name"`
	Version         string    `toml:"version"`
	MinHerdrVersion string    `toml:"min_herdr_version"`
	Description     string    `toml:"description"`
	Platforms       []string  `toml:"platforms"`
	Build           []Command `toml:"build"`
	// Startup runs on every herdr server start and live handoff, never on
	// link, enable or config reload.
	Startup []Command `toml:"startup"`
	Actions []Action  `toml:"actions"`
	// Panes are terminal UIs herdr runs on plugin.pane.open.
	Panes []Pane `toml:"panes"`
}

// Command is a [[build]] or [[startup]] step: one exec of an argv array, no
// shell.
type Command struct {
	Command   []string `toml:"command"`
	Platforms []string `toml:"platforms"`
}

// Action is an [[actions]] entry.
type Action struct {
	ID          string   `toml:"id"`
	Title       string   `toml:"title"`
	Description string   `toml:"description"`
	Contexts    []string `toml:"contexts"`
	Command     []string `toml:"command"`
	Platforms   []string `toml:"platforms"`
}

// Pane is a [[panes]] entry: an argv herdr runs as a terminal pane. Popup
// sizes (width, height) are not declared here: the plugin sends them with each
// plugin.pane.open, which overrides the manifest's.
type Pane struct {
	ID          string `toml:"id"`
	Title       string `toml:"title"`
	Description string `toml:"description"`
	// Placement is one of knownPlacements; empty is herdr's default, overlay.
	Placement string   `toml:"placement"`
	Command   []string `toml:"command"`
	Platforms []string `toml:"platforms"`
}

var (
	// herdr's documented id alphabets: plugin ids may contain dots, local
	// action ids may not (the qualified id is "<plugin>.<action>").
	pluginIDPattern = regexp.MustCompile(`^[A-Za-z0-9.:_-]+$`)
	actionIDPattern = regexp.MustCompile(`^[A-Za-z0-9:_-]+$`)

	knownPlatforms = []string{"linux", "macos", "windows"}
	knownContexts  = []string{"global", "workspace", "tab", "pane", "selection"}
	// knownPlacements are herdr 0.9.3's PluginPanePlacement values.
	knownPlacements = []string{"overlay", "popup", "split", "tab", "zoomed"}
)

// maxPaneIDChars is herdr's PLUGIN_ACTION_ID_MAX_CHARS, which bounds a pane
// id as it does an action id.
const maxPaneIDChars = 120

// LoadManifest reads, strictly decodes and validates the manifest at path.
func LoadManifest(path string) (Manifest, error) {
	// #nosec G304 -- path is the plugin's own manifest, chosen by the caller, never by herdr or a user at runtime.
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", path, err)
	}
	var m Manifest
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %s: %w", ErrInvalidManifest, path, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Validate checks the rules herdr enforces, plus the ones it only warns about.
func (m Manifest) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidManifest}, args...)...)
	}
	if !pluginIDPattern.MatchString(m.ID) {
		return bad("id %q must match %s", m.ID, pluginIDPattern)
	}
	for field, v := range map[string]string{"name": m.Name, "version": m.Version, "min_herdr_version": m.MinHerdrVersion} {
		if v == "" {
			return bad("%s is required", field)
		}
	}
	if err := checkPlatforms(m.Platforms); err != nil {
		return bad("%w", err)
	}
	if m.BuildOutput() == "" {
		return bad("no [[build]] step names an output with -o")
	}
	for i, st := range m.Startup {
		if len(st.Command) == 0 {
			return bad("startup hook %d needs a non-empty command", i)
		}
		if err := checkPlatforms(st.Platforms); err != nil {
			return bad("startup hook %d: %w", i, err)
		}
	}
	seen := map[string]bool{}
	for _, a := range m.Actions {
		if !actionIDPattern.MatchString(a.ID) {
			return bad("action id %q must match %s", a.ID, actionIDPattern)
		}
		if seen[a.ID] {
			return bad("action id %q is declared twice", a.ID)
		}
		seen[a.ID] = true
		if a.Title == "" || len(a.Command) == 0 {
			return bad("action %q needs a title and a non-empty command", a.ID)
		}
		for _, c := range a.Contexts {
			if !slices.Contains(knownContexts, c) {
				return bad("action %q has unknown context %q", a.ID, c)
			}
		}
		if err := checkPlatforms(a.Platforms); err != nil {
			return bad("action %q: %w", a.ID, err)
		}
	}
	return m.validatePanes()
}

// validatePanes mirrors herdr 0.9.3's normalize_manifest_pane, so a pane
// herdr would refuse at link time fails here first: an id in the action-id
// alphabet, at most 120 characters and unique (compared as written, as
// herdr does); a title that is not blank; a command with no empty argument;
// and a placement herdr knows.
func (m Manifest) validatePanes() error {
	seen := map[string]bool{}
	for _, p := range m.Panes {
		if !actionIDPattern.MatchString(p.ID) || len(p.ID) > maxPaneIDChars {
			return fmt.Errorf("%w: pane id %q must match %s, in at most %d characters", ErrInvalidManifest, p.ID, actionIDPattern, maxPaneIDChars)
		}
		if seen[p.ID] {
			return fmt.Errorf("%w: pane id %q is declared twice", ErrInvalidManifest, p.ID)
		}
		seen[p.ID] = true
		if strings.TrimSpace(p.Title) == "" || len(p.Command) == 0 || slices.Contains(p.Command, "") {
			return fmt.Errorf("%w: pane %q needs a title and a command with no empty argument", ErrInvalidManifest, p.ID)
		}
		if p.Placement != "" && !slices.Contains(knownPlacements, p.Placement) {
			return fmt.Errorf("%w: pane %q has unknown placement %q", ErrInvalidManifest, p.ID, p.Placement)
		}
		if err := checkPlatforms(p.Platforms); err != nil {
			return fmt.Errorf("%w: pane %q: %w", ErrInvalidManifest, p.ID, err)
		}
	}
	return nil
}

func checkPlatforms(ps []string) error {
	for _, p := range ps {
		if !slices.Contains(knownPlatforms, p) {
			return fmt.Errorf("unknown platform %q", p)
		}
	}
	return nil
}

// BuildOutput is the path the first [[build]] step writes with -o, relative
// to the plugin root. Actions must invoke exactly this path: `herdr plugin
// link` never runs the build, so a mismatch only shows up in a live herdr.
func (m Manifest) BuildOutput() string {
	for _, b := range m.Build {
		if i := slices.Index(b.Command, "-o"); i >= 0 && i+1 < len(b.Command) {
			return b.Command[i+1]
		}
	}
	return ""
}

// QualifiedActionID is the id herdr exposes for an action: "<plugin>.<action>".
func (m Manifest) QualifiedActionID(a Action) string {
	return m.ID + "." + a.ID
}

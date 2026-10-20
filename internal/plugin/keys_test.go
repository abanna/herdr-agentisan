package plugin_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// keySnippet is docs/config/herdr-keys.example.toml as herdr 0.9.3 reads it:
// the [keys] action binding it sets (goto, src/config/model.rs:359) and its
// [[keys.command]] entries, with CommandKeybindConfig's fields only
// (src/config/keybinds.rs:122-137). herdr ignores an unknown key, so the
// sample is decoded strictly: a misspelt one fails here instead.
type keySnippet struct {
	Keys struct {
		Goto    []string     `toml:"goto"`
		Command []keyBinding `toml:"command"`
	} `toml:"keys"`
}

// keyBinding is one [[keys.command]] entry.
type keyBinding struct {
	Key         string `toml:"key"`
	Command     string `toml:"command"`
	Type        string `toml:"type"`
	Description string `toml:"description"`
	Width       any    `toml:"width"`
	Height      any    `toml:"height"`
}

// readKeySnippet is the sample's text and its strict decoding.
func readKeySnippet(t *testing.T) (string, keySnippet) {
	t.Helper()
	sample, err := os.ReadFile(filepath.Join("..", "..", "docs", "config", "herdr-keys.example.toml"))
	require.NoError(t, err)
	var cfg keySnippet
	dec := toml.NewDecoder(bytes.NewReader(sample))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&cfg))
	return string(sample), cfg
}

// readmeKeys is the README's TOML blocks that bind keys, in the README's
// order: the ones holding a [keys] table or a [[keys.command]].
func readmeKeys(t *testing.T) string {
	t.Helper()
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	require.NoError(t, err)
	var blocks []string
	for _, b := range regexp.MustCompile("(?s)```toml\n(.*?)```").FindAllStringSubmatch(string(readme), -1) {
		for line := range strings.Lines(b[1]) {
			if l := strings.TrimSpace(line); l == "[keys]" || l == "[[keys.command]]" {
				blocks = append(blocks, b[1])
				break
			}
		}
	}
	return strings.Join(blocks, "\n")
}

// TestKeySnippetBindsGoTo keeps the documented search key from rotting
// (ADR-001 A20): prefix+/ opens herdr's own Go To, the action herdr names
// keys.goto, beside its default prefix+g (src/config/model.rs:1130), which a
// list keeps (BindingConfig::Many, src/config/keybinds.rs:47). Help is
// prefix+?, so the slash is bare. TestKeySnippetBindsBack holds the README
// to the same sample.
func TestKeySnippetBindsGoTo(t *testing.T) {
	t.Parallel()
	_, cfg := readKeySnippet(t)
	assert.Equal(t, []string{"prefix+g", "prefix+/"}, cfg.Keys.Goto)
	assert.Contains(t, readmeKeys(t), `goto = ["prefix+g", "prefix+/"]`)
}

// keyTypes are herdr 0.9.3's CommandKeybindType values
// (src/config/keybinds.rs:113-118). There is no plugin_pane: a key opens a
// plugin pane through a shell command or a plugin action.
var keyTypes = []string{"shell", "pane", "popup", "plugin_action"}

// TestKeySnippetOpensTheDashboard: the sample's prefix+d, which replaces
// dashboard-tab.sh, opens this plugin's dashboard pane, the split the
// manifest declares, focused, through herdr's own CLI (`herdr plugin pane
// open`). herdr runs a shell key command as `/bin/sh -lc <command>`
// (src/platform/linux.rs:299-308, src/platform/macos.rs:187-192), with
// HERDR_SOCKET_PATH set to its own socket (src/app/custom_commands.rs:244-248).
// TestKeySnippetBindsBack holds the README to the same sample.
func TestKeySnippetOpensTheDashboard(t *testing.T) {
	t.Parallel()
	_, cfg := readKeySnippet(t)
	i := slices.IndexFunc(cfg.Keys.Command, func(k keyBinding) bool { return k.Key == "prefix+d" })
	require.GreaterOrEqual(t, i, 0, "the sample binds no prefix+d")
	k := cfg.Keys.Command[i]

	assert.Contains(t, keyTypes, k.Type)
	assert.Equal(t, "shell", k.Type)
	assert.Equal(t, "herdr plugin pane open --plugin "+plugin.ID+" --entrypoint "+plugin.DashboardPane+" --focus", k.Command)
	assert.NotEmpty(t, strings.TrimSpace(k.Description))
	assert.Nil(t, k.Width, "popup sizes apply to type popup only")
	assert.Nil(t, k.Height, "popup sizes apply to type popup only")

	m, err := plugin.LoadManifest(filepath.Join("..", "..", plugin.ManifestFile))
	require.NoError(t, err)
	assert.Equal(t, plugin.ID, m.ID)
	j := slices.IndexFunc(m.Panes, func(p plugin.Pane) bool { return p.ID == plugin.DashboardPane })
	require.GreaterOrEqual(t, j, 0, "the snippet opens an entrypoint the manifest declares")
	assert.Equal(t, "split", m.Panes[j].Placement, "the CLI leaves the placement to the manifest")
}

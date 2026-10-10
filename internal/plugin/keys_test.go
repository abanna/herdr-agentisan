package plugin_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keySnippet is docs/config/herdr-keys.example.toml as herdr 0.9.3 reads it:
// the [keys] action binding it sets (goto, src/config/model.rs:359) and its
// [[keys.command]] entries, with CommandKeybindConfig's fields only
// (src/config/keybinds.rs:122-137). herdr ignores an unknown key, so the
// sample is decoded strictly: a misspelt one fails here instead.
type keySnippet struct {
	Keys struct {
		Goto    []string `toml:"goto"`
		Command []struct {
			Key         string `toml:"key"`
			Command     string `toml:"command"`
			Type        string `toml:"type"`
			Description string `toml:"description"`
			Width       any    `toml:"width"`
			Height      any    `toml:"height"`
		} `toml:"command"`
	} `toml:"keys"`
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

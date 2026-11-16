package settings_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestLoadRefusesCodexHookState covers the Q9 trust-state route at every
// command table, with routing on and off. The inline forms preserve a real
// hook key: Codex splits override paths literally at every dot, so quoting
// that key in a dotted override would not address the same map entry.
func TestLoadRefusesCodexHookState(t *testing.T) {
	t.Parallel()
	const hook = `"file:/tmp/qa145/hooks.json:stop:0:0"={trusted_hash="sha256:planted-secret"}`
	for name, kv := range map[string]string{
		"leaf":                 `hooks.state.simple.trusted_hash="sha256:planted-secret"`,
		"entry":                `hooks.state.simple={trusted_hash="sha256:planted-secret"}`,
		"state inline":         `hooks.state={` + hook + `}`,
		"hooks inline":         `hooks={state={` + hook + `}}`,
		"dotted inline":        `hooks={state.` + hook + `}`,
		"escaped inline key":   `hooks={"\u0073tate"={` + hook + `}}`,
		"empty state":          `hooks.state={}`,
		"multiline inline":     "hooks={\n# a comment\nstate={" + hook + "},\n}",
		"literal inline key":   `hooks={'state'={` + hook + `}}`,
		"empty ancestor state": `hooks={state={}}`,
		"spaced key":           ` hooks.state = {` + hook + `}`,
	} {
		for form, args := range map[string][]string{
			"short": {"-c", kv}, "long": {"--config", kv},
			"short equals": {"-c=" + kv}, "long equals": {"--config=" + kv},
			"attached": {"-c" + kv},
		} {
			for path, body := range codexCommandAt(args...) {
				for _, enabled := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/routing=%t", name, form, path, enabled), func(t *testing.T) {
						t.Parallel()
						_, err := loadRouting(t, body+fmt.Sprintf("[routing]\nenabled = %t\n", enabled), "", nil, true)
						require.ErrorIs(t, err, settings.ErrInvalid)
						assert.Contains(t, err.Error(), path+" ("+settings.LayerShared+", ")
						assert.Contains(t, err.Error(), "codex's hooks.state key runs enabled hooks without the trust a person gives Codex once (ADR-001 A43): remove it")
						assert.NotContains(t, err.Error(), "planted-secret")
						assert.NotContains(t, err.Error(), "file:/tmp/qa145")
					})
				}
			}
		}
	}
	for _, program := range []string{"/opt/bin/codex", "../bin/codex"} {
		_, err := loadRouting(t, `[agent]
command = ["`+program+`", "-c", 'hooks.state={`+hook+`}']
`, "", nil, true)
		require.ErrorIs(t, err, settings.ErrInvalid)
	}
}

// TestLoadKeepsOtherHookConfig pins the matching boundary: the state
// namespace is refused, while hook definitions and unrelated fields stay
// the user's. Invalid TOML becomes a string in Codex, not effective state.
func TestLoadKeepsOtherHookConfig(t *testing.T) {
	t.Parallel()
	for name, kv := range map[string]string{
		"bare hooks":              `hooks={}`,
		"definitions":             `hooks={stop=[{command="echo trusted_hash=hello"}]}`,
		"state look-alike":        `hooks={stateful={trusted_hash="x"}}`,
		"nested state":            `hooks={other={state={trusted_hash="x"}}}`,
		"state dotted look-alike": `hooks.states.simple.trusted_hash="x"`,
		"case":                    `Hooks.state.simple.trusted_hash="x"`,
		"non ASCII":               `hooks.état={trusted_hash="x"}`,
		"string":                  `hooks="state={trusted_hash=hello}"`,
		"array":                   `hooks=[{state={trusted_hash="x"}}]`,
		"malformed":               `hooks={state={`,
		"non config":              `other={hooks={state={trusted_hash="x"}}}`,
	} {
		for path, body := range codexCommandAt("-c", kv) {
			t.Run(name+"/"+path, func(t *testing.T) {
				t.Parallel()
				_, err := loadRouting(t, body+"[routing]\nenabled = true\n", "", nil, true)
				require.NoError(t, err)
			})
		}
	}
	for _, kv := range []string{`hooks.state.simple.trusted_hash="x"`, `hooks={state={simple={trusted_hash="x"}}}`} {
		for _, program := range []string{"claude", "/opt/bin/claude"} {
			// The provider-specific codex table necessarily requires codex; the
			// top/group tables exercise the executable guard with Claude.
			for path, body := range codexCommandAt("-c", kv) {
				if strings.Contains(path, ".codex.") {
					continue
				}
				_, err := loadRouting(t, strings.Replace(body, `"codex"`, `"`+program+`"`, 1), "", nil, true)
				require.NoError(t, err)
			}
		}
	}
}

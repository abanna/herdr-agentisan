package plugin_test

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/plugin"
)

// TestStateDirFindsHerdrsDirectory: --state-dir, then HERDR_PLUGIN_STATE_DIR,
// then the directory herdr 0.9.5 gives this plugin, from XDG_STATE_HOME when
// it is set at all (even empty, or relative, which daemon.PathsFor refuses),
// else HOME (ADR-001 A51). With neither there is no directory, as before. A
// value that is not UTF-8 counts as unset, as std::env::var has it.
func TestStateDirFindsHerdrsDirectory(t *testing.T) {
	t.Parallel()

	const own = "herdr/plugins/nerdsrun.agentisan"
	tests := map[string]struct {
		flag    string
		env     map[string]string
		dir     string
		derived bool
	}{
		"the flag, over everything": {
			flag: "/flag",
			env:  map[string]string{"HERDR_PLUGIN_STATE_DIR": "/var", "XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			dir:  "/flag",
		},
		"the variable, over a derivation": {
			env: map[string]string{"HERDR_PLUGIN_STATE_DIR": "/var", "XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			dir: "/var",
		},
		"XDG_STATE_HOME, over HOME": {
			env: map[string]string{"HERDR_PLUGIN_STATE_DIR": "", "XDG_STATE_HOME": "/xdg", "HOME": "/home/u"},
			dir: "/xdg/" + own, derived: true,
		},
		"HOME alone": {
			env: map[string]string{"HOME": "/home/u"},
			dir: "/home/u/.local/state/" + own, derived: true,
		},
		"an empty XDG_STATE_HOME is set, as herdr has it": {
			env: map[string]string{"XDG_STATE_HOME": "", "HOME": "/home/u"},
			dir: own, derived: true,
		},
		"an XDG_STATE_HOME with .. is cleaned, as daemon.PathsFor cleans the daemon's own": {
			env: map[string]string{"XDG_STATE_HOME": "/x/../xdg"},
			dir: "/xdg/" + own, derived: true,
		},
		"a HOME with .. is cleaned too": {
			env: map[string]string{"HOME": "/home/u/../v"},
			dir: "/home/v/.local/state/" + own, derived: true,
		},
		"an XDG_STATE_HOME with a space and a non-ASCII letter": {
			env: map[string]string{"XDG_STATE_HOME": "/st ü"},
			dir: "/st ü/" + own, derived: true,
		},
		"a HOME with a space and a non-ASCII letter": {
			env: map[string]string{"HOME": "/home/ü ser"},
			dir: "/home/ü ser/.local/state/" + own, derived: true,
		},
		"a relative XDG_STATE_HOME stays relative": {
			env: map[string]string{"XDG_STATE_HOME": "state", "HOME": "/home/u"},
			dir: "state/" + own, derived: true,
		},
		"an empty HOME is set, as herdr has it": {
			env: map[string]string{"HOME": ""},
			dir: ".local/state/" + own, derived: true,
		},
		"an XDG_STATE_HOME that is not UTF-8 is unset": {
			env: map[string]string{"XDG_STATE_HOME": "/x\xff", "HOME": "/home/u"},
			dir: "/home/u/.local/state/" + own, derived: true,
		},
		"a HOME that is not UTF-8 is unset": {
			env: map[string]string{"HOME": "/home/\xff"},
		},
		"neither": {
			env: map[string]string{"HERDR_PLUGIN_STATE_DIR": "", "PATH": "/bin"},
		},
		"nothing at all": {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, derived := plugin.StateDir(tc.flag, lookupFrom(tc.env))
			assert.Equal(t, tc.dir, dir)
			assert.Equal(t, tc.derived, derived)
			if tc.derived {
				assert.Equal(t, dir, plugin.HerdrStateDir(lookupFrom(tc.env)))
			}
		})
	}
}

// TestIDIsItsOwnHerdrPathComponent: herdr escapes a plugin id into its
// state directory's last component (src/plugin_paths.rs:96-124): every
// byte but [a-z0-9._-] becomes %XX, a trailing "." becomes %2E, an id whose
// stem before the first "." is a Windows device name gets a leading %
// (src/api/schema/plugins.rs:162-189), and one over 120 characters is cut
// and hashed. HerdrStateDir uses ID as it is, which is right only while
// none of that applies.
func TestIDIsItsOwnHerdrPathComponent(t *testing.T) {
	t.Parallel()

	assert.Regexp(t, regexp.MustCompile(`^[a-z0-9._-]+$`), plugin.ID)
	assert.NotRegexp(t, regexp.MustCompile(`\.$`), plugin.ID)
	assert.LessOrEqual(t, utf8.RuneCountInString(plugin.ID), 120)
	stem, _, _ := strings.Cut(plugin.ID, ".")
	assert.NotRegexp(t, regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`), stem, "a Windows device name")
}

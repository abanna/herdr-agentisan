package plugin

import (
	"path/filepath"
	"unicode/utf8"
)

// StateDir is the plugin state directory a command finds the daemon under
// (ADR-001 A51): flag when given, else HERDR_PLUGIN_STATE_DIR, which herdr
// sets only for the plugin's own processes and panes, else herdr's
// directory for this plugin (HerdrStateDir), with derived true. It is ""
// when none of them gives one, and the caller refuses as before. lookup is
// os.LookupEnv in production.
//
// Only a command that reads or signals a running daemon may take the
// derived directory. One that starts a daemon must not: under a wrong guess
// it would be a second daemon for the same herdr server, which no lock sees.
func StateDir(flag string, lookup func(string) (string, bool)) (dir string, derived bool) {
	if flag != "" {
		return flag, false
	}
	if dir, _ := lookup("HERDR_PLUGIN_STATE_DIR"); dir != "" {
		return dir, false
	}
	if dir := HerdrStateDir(lookup); dir != "" {
		return dir, true
	}
	return "", false
}

// HerdrStateDir is the directory herdr 0.9.5 gives this plugin as
// HERDR_PLUGIN_STATE_DIR, worked out from the environment as herdr does
// (ADR-001 A51). herdr's plugin_state_dir is state_dir()/plugins/<ID>
// (src/plugin_paths.rs:50-54), and state_dir is $XDG_STATE_HOME/herdr when
// XDG_STATE_HOME is set at all, else $HOME/.local/state/herdr
// (src/config/io.rs:37-42,88-94). herdr reads each with std::env::var, so a
// set but empty value counts, and gives a relative directory that
// daemon.PathsFor refuses, while a value that is not UTF-8 does not count.
//
// ID needs none of herdr's escaping (TestIDIsItsOwnHerdrPathComponent). A
// release herdr is assumed: a debug build uses herdr-dev
// (src/config/io.rs:22-27). With neither variable it is "": herdr would
// fall back to its platform's temporary directory, which a pane shell, one
// herdr always gives HOME, never reaches.
func HerdrStateDir(lookup func(string) (string, bool)) string {
	set := func(k string) (string, bool) {
		v, ok := lookup(k)
		return v, ok && utf8.ValidString(v)
	}
	if xdg, ok := set("XDG_STATE_HOME"); ok {
		return filepath.Join(xdg, "herdr", "plugins", ID)
	}
	if home, ok := set("HOME"); ok {
		return filepath.Join(home, ".local", "state", "herdr", "plugins", ID)
	}
	return ""
}

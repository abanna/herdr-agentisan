package debugger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// The launch's inputs that `debugger args` and `worker spawn --group
// debugger` both look up the same way (A32).

// RootEnv is where herdr gives the plugin its checkout.
const RootEnv = "HERDR_PLUGIN_ROOT"

// Root is the plugin's checkout: RootEnv when herdr set it, else the
// checkout the binary was built into (<root>/bin/herdr-agentisan).
func Root(lookup func(string) (string, bool), bin string) string {
	if r, _ := lookup(RootEnv); r != "" {
		return r
	}
	return filepath.Dir(filepath.Dir(bin))
}

// CheckRoot refuses a root with no role under it, which claude could not
// append: none, or one that is no regular file, such as a directory or a
// FIFO claude would wait on. A link to a regular file is one.
func CheckRoot(root string) error {
	fi, err := os.Stat(filepath.Join(root, RoleFile))
	if err == nil && !fi.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		return fmt.Errorf("%w: no %s under %q, the plugin's checkout: %w", ErrInvalid, RoleFile, root, err)
	}
	return nil
}

// KeyFiles is the env file the shared config in configDir names, which
// holds the trail's read key, and its target when it is a link: the launch
// denies both to Read. The file itself is never opened. lookup gives HOME,
// for an env file under ~/.
func KeyFiles(configDir string, lookup func(string) (string, bool)) ([]string, error) {
	envFile, err := settings.DebuggerEnvFile(configDir, lookup)
	if err != nil {
		return nil, fmt.Errorf("%w: the shared config: %w", ErrInvalid, err)
	}
	if envFile == "" {
		return nil, fmt.Errorf("%w: set [debugger] env_file in %s: the debugger's events read the trail's key from it", ErrInvalid, filepath.Join(configDir, settings.FileName))
	}
	keys := []string{envFile}
	if target, err := filepath.EvalSymlinks(envFile); err == nil && target != envFile {
		keys = append(keys, target)
	}
	return keys, nil
}

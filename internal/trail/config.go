package trail

import (
	"context"
	"fmt"
	"time"

	"github.com/abanna/herdr-agentisan/internal/settings"
)

// ConfigDirEnv is how the events commands find the shared config, whose
// debugger.env_file names the env file they read: herdr's plugin config dir,
// which the workforce sets in the debugger's environment.
const ConfigDirEnv = "HERDR_PLUGIN_CONFIG_DIR"

// FromConfig builds the client from the env file the shared config in
// configDir (HERDR_PLUGIN_CONFIG_DIR) names as debugger.env_file, read now,
// within timeout, for URLVar and KeyVar only. lookupEnv is read for HOME,
// to expand a ~/ path, and nothing else: the URL and the key are never
// taken from the environment, so they never have to be in the agent's.
func FromConfig(ctx context.Context, configDir string, lookupEnv func(string) (string, bool), timeout time.Duration) (*Client, error) {
	if configDir == "" {
		return nil, fmt.Errorf("%w: %s is not set; the workforce sets it for the debugger, and a shell can export it", ErrNotConfigured, ConfigDirEnv)
	}
	path, err := settings.DebuggerEnvFile(configDir, lookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotConfigured, err)
	}
	if path == "" {
		return nil, fmt.Errorf("%w: [debugger] env_file is not set in %s's %s", ErrNotConfigured, configDir, settings.FileName)
	}
	vals, err := ReadEnvFile(ctx, path, []string{URLVar, KeyVar}, timeout)
	if err != nil {
		return nil, err
	}
	return NewClient(vals[URLVar], vals[KeyVar], nil)
}

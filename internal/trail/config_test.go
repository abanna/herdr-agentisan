package trail_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/trail"
)

// configDir writes a shared config whose [debugger] env_file is envFile
// ("" for none) and returns its directory.
func configDir(t *testing.T, envFile string) string {
	t.Helper()
	dir := t.TempDir()
	body := "schema_version = 1\n"
	if envFile != "" {
		body += "[debugger]\nenv_file = \"" + envFile + "\"\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(body), 0o600))
	return dir
}

// TestFromConfig: the client comes from the env file the shared config
// names, and from nowhere else. ELASTIC_URL and ELASTIC_API_KEY in the
// environment are never read, so a debugger whose config names no file has
// no trail, whatever its environment holds.
func TestFromConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	good := filepath.Join(home, "local.env")
	require.NoError(t, os.WriteFile(good, []byte("ELASTIC_URL=http://es:9200\n"+keyEq+probeKey+"\n"), 0o600))
	badURL := filepath.Join(home, "bad.env")
	require.NoError(t, os.WriteFile(badURL, []byte("ELASTIC_URL=http://u:p@es:9200\n"+keyEq+probeKey+"\n"), 0o600))
	env := map[string]string{"HOME": home, trail.URLVar: "http://elsewhere:9200", trail.KeyVar: "env-key-never-read"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	tests := map[string]struct {
		dir     string
		wantErr error
		wantIn  string
	}{
		"the env file":           {dir: configDir(t, good)},
		"a path under ~/":        {dir: configDir(t, "~/local.env")},
		"no config dir":          {dir: "", wantErr: trail.ErrNotConfigured, wantIn: "HERDR_PLUGIN_CONFIG_DIR"},
		"no env_file, env set":   {dir: configDir(t, ""), wantErr: trail.ErrNotConfigured, wantIn: "[debugger] env_file is not set"},
		"the file missing":       {dir: configDir(t, filepath.Join(home, "gone.env")), wantErr: trail.ErrEnvFile, wantIn: "does not exist"},
		"a URL with a user":      {dir: configDir(t, badURL), wantErr: trail.ErrInvalid, wantIn: trail.URLVar},
		"a config that is wrong": {dir: configDir(t, "relative.env"), wantErr: trail.ErrNotConfigured, wantIn: "debugger.env_file"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := trail.FromConfig(t.Context(), tc.dir, lookup, time.Second)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.wantIn)
				assert.NotContains(t, err.Error(), probeKey)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "trail.Client(http://es:9200)", c.String(), "the file's URL, not the environment's")
		})
	}
}

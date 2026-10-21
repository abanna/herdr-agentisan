package trail_test

import (
	"net/http"
	"net/http/httptest"
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

// TestFromConfigReadsNoEnvironmentButHOME: ELASTIC_URL and ELASTIC_API_KEY
// in the process environment, or in the lookup, never reach the client: the
// key sent is the env file's, to the env file's URL, and a file without the
// key is not configured, whatever the environment holds. Not parallel: it
// sets the process environment.
func TestFromConfigReadsNoEnvironmentButHOME(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the environment's URL was dialled")
	}))
	t.Cleanup(elsewhere.Close)
	srv, req, _ := esStandIn(t, http.StatusOK, recorded(t))
	t.Setenv(trail.URLVar, elsewhere.URL)
	t.Setenv(trail.KeyVar, "env-key-never-read")
	home := t.TempDir()
	env := map[string]string{"HOME": home, trail.URLVar: elsewhere.URL, trail.KeyVar: "lookup-key-never-read"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	good := filepath.Join(home, "local.env")
	require.NoError(t, os.WriteFile(good, []byte("ELASTIC_URL="+srv.URL+"\n"+keyEq+probeKey+"\n"), 0o600))
	c, err := trail.FromConfig(t.Context(), configDir(t, good), lookup, time.Second)
	require.NoError(t, err)
	_, err = c.Query(t.Context(), "FROM logs-agentisan.otel-*")
	require.NoError(t, err)
	assert.Equal(t, "ApiKey "+probeKey, req.Header.Get("Authorization"), "the file's key, not the environment's")

	noKey := filepath.Join(home, "nokey.env")
	require.NoError(t, os.WriteFile(noKey, []byte("ELASTIC_URL="+srv.URL+"\n"), 0o600))
	_, err = trail.FromConfig(t.Context(), configDir(t, noKey), lookup, time.Second)
	require.ErrorIs(t, err, trail.ErrNotConfigured, "the environment's key is no fallback")

	_, err = trail.FromConfig(t.Context(), configDir(t, ""), lookup, time.Second)
	require.ErrorIs(t, err, trail.ErrNotConfigured, "no env_file, whatever the environment holds")
}

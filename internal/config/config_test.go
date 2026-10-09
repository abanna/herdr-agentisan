package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	// Not parallel: Load reads process environment.
	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "development", cfg.Env)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "console", cfg.LogFormat)
	assert.False(t, cfg.IsProduction())

	assert.Equal(t, "herdr-agentisan", cfg.ServiceName)
	assert.Empty(t, cfg.OTLPEndpoint, "tracing export must be off until a collector is named")
	assert.InDelta(t, 1.0, cfg.TraceSampleRatio, 0)
	assert.Equal(t, 15*time.Second, cfg.DaemonLockWait)
	assert.False(t, cfg.AllowUnverified, "the protocol pin is on unless overridden")
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("HERDR_AGENTISAN_ENV", "production")
	t.Setenv("HERDR_AGENTISAN_LOG_LEVEL", "debug")
	t.Setenv("HERDR_AGENTISAN_LOG_FORMAT", "json")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.True(t, cfg.IsProduction())
}

func TestLoadReadsTelemetryEnvironment(t *testing.T) {
	t.Setenv("HERDR_AGENTISAN_OTLP_ENDPOINT", "otel-collector:4318")
	t.Setenv("HERDR_AGENTISAN_TRACE_SAMPLE_RATIO", "0.25")
	t.Setenv("HERDR_AGENTISAN_SERVICE_NAME", "herdr-agentisan-canary")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "otel-collector:4318", cfg.OTLPEndpoint)
	assert.InDelta(t, 0.25, cfg.TraceSampleRatio, 0)
	assert.Equal(t, "herdr-agentisan-canary", cfg.ServiceName)
}

func TestValidate(t *testing.T) {
	t.Parallel()

	base := config.Config{
		Env: "development", LogLevel: "info", LogFormat: "console",
		ServiceName: "herdr-agentisan", TraceSampleRatio: 1,
		DaemonLockWait: 15 * time.Second,
	}

	tests := map[string]struct {
		mutate  func(c *config.Config)
		wantErr bool
	}{
		"valid":              {mutate: func(*config.Config) {}},
		"unknown env":        {mutate: func(c *config.Config) { c.Env = "qa" }, wantErr: true},
		"unknown log format": {mutate: func(c *config.Config) { c.LogFormat = "xml" }, wantErr: true},
		"empty service name": {mutate: func(c *config.Config) { c.ServiceName = "" }, wantErr: true},
		"negative sample ratio": {
			mutate:  func(c *config.Config) { c.TraceSampleRatio = -0.1 },
			wantErr: true,
		},
		"sample ratio above one": {
			mutate:  func(c *config.Config) { c.TraceSampleRatio = 1.5 },
			wantErr: true,
		},
		"zero sample ratio is valid": {mutate: func(c *config.Config) { c.TraceSampleRatio = 0 }},
		"staging is valid":           {mutate: func(c *config.Config) { c.Env = "staging" }},
		"production is valid":        {mutate: func(c *config.Config) { c.Env = "production" }},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := base
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestProductionNeedsNoAPIToken: the plugin serves no API, so a deployed
// environment has no token to demand. Requiring one would refuse to start a
// binary that has nothing to authenticate.
func TestProductionNeedsNoAPIToken(t *testing.T) {
	t.Setenv("HERDR_AGENTISAN_ENV", "production")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.True(t, cfg.IsProduction())
}

// TestLoadEnvironmentClasses walks what Load observes besides explicit
// values: an empty variable, an unprefixed look-alike inherited from the
// parent, malformed values, and the .env file in the working directory.
// Not parallel: Load reads process environment and the working directory.
func TestLoadEnvironmentClasses(t *testing.T) {
	tests := map[string]struct {
		env       map[string]string
		dotenv    string // written to ./.env when non-empty; "<dir>" makes it a directory
		wantErr   bool
		wantEnv   string
		wantLevel string
	}{
		"empty value falls back to the default": {
			env: map[string]string{"HERDR_AGENTISAN_ENV": ""}, wantEnv: "development", wantLevel: "info",
		},
		"unprefixed look-alike is ignored": {
			env: map[string]string{"ENV": "production", "LOG_LEVEL": "debug"}, wantEnv: "development", wantLevel: "info",
		},
		"value with whitespace is rejected": {
			env: map[string]string{"HERDR_AGENTISAN_ENV": " production"}, wantErr: true,
		},
		"value with invalid UTF-8 is rejected": {
			env: map[string]string{"HERDR_AGENTISAN_LOG_FORMAT": "js\xffon"}, wantErr: true,
		},
		".env in the working directory is read": {
			dotenv: "HERDR_AGENTISAN_LOG_LEVEL=debug\n", wantEnv: "development", wantLevel: "debug",
		},
		"real environment wins over .env": {
			env:    map[string]string{"HERDR_AGENTISAN_LOG_LEVEL": "warn"},
			dotenv: "HERDR_AGENTISAN_LOG_LEVEL=debug\n", wantEnv: "development", wantLevel: "warn",
		},
		"a .env directory is ignored, not fatal": {
			dotenv: "<dir>", wantEnv: "development", wantLevel: "info",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// godotenv.Load writes into the process environment. Registering
			// the key with t.Setenv first and then unsetting it lets the
			// cleanup restore the original state whatever .env sets.
			t.Setenv("HERDR_AGENTISAN_LOG_LEVEL", "")
			require.NoError(t, os.Unsetenv("HERDR_AGENTISAN_LOG_LEVEL"))
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			dir := t.TempDir()
			switch tc.dotenv {
			case "":
			case "<dir>":
				require.NoError(t, os.Mkdir(filepath.Join(dir, ".env"), 0o750))
			default:
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(tc.dotenv), 0o600))
			}
			t.Chdir(dir)

			cfg, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantEnv, cfg.Env)
			assert.Equal(t, tc.wantLevel, cfg.LogLevel)
		})
	}
}

// TestEnvPrefixIsTheRepositoryName: variables are namespaced by the binary
// they configure. Any other prefix is ignored — see the "unprefixed look-alike"
// case in TestLoadEnvironmentClasses.
func TestEnvPrefixIsTheRepositoryName(t *testing.T) {
	t.Setenv("HERDR_AGENTISAN_LOG_LEVEL", "debug")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "HERDR_AGENTISAN", config.EnvPrefix)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "herdr-agentisan", cfg.ServiceName)
}

func TestInvalidEnvIsRejectedByLoad(t *testing.T) {
	t.Setenv("HERDR_AGENTISAN_ENV", "qa")

	_, err := config.Load()
	require.Error(t, err)
}

// TestDaemonLockWaitClasses: how long a new daemon waits for a lock still held
// by a daemon bound to a herdr socket that has since been replaced (ADR-001
// A3). It is a Go duration, more than zero and at most five minutes: a wait
// that long means the old daemon is wedged, and waiting longer only delays the
// error that says so.
func TestDaemonLockWaitClasses(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		"a Go duration":          {value: "30s", want: 30 * time.Second},
		"sub-second":             {value: "250ms", want: 250 * time.Millisecond},
		"exactly five minutes":   {value: "5m", want: 5 * time.Minute},
		"empty uses the default": {value: "", want: 15 * time.Second},
		"just past five minutes": {value: "5m1s", wantErr: true},
		"zero":                   {value: "0s", wantErr: true},
		"negative":               {value: "-1s", wantErr: true},
		"a bare number":          {value: "15", wantErr: true},
		"not a duration":         {value: "soon", wantErr: true},
		"overflows a duration":   {value: "9999999999h", wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HERDR_AGENTISAN_DAEMON_LOCK_WAIT", tc.value)

			cfg, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.DaemonLockWait)
		})
	}
}

// TestConfigTravelsInTheContext: main loads the config once and the commands
// read it from the context, so a command never re-reads the environment.
func TestConfigTravelsInTheContext(t *testing.T) {
	t.Parallel()

	_, ok := config.From(context.Background())
	assert.False(t, ok, "a context without a config says so")

	want := config.Config{Env: "staging", DaemonLockWait: time.Minute}
	got, ok := config.From(config.Into(context.Background(), want))
	require.True(t, ok)
	assert.Equal(t, want, got)
}

// TestAllowUnverifiedClasses: HERDR_AGENTISAN_ALLOW_UNVERIFIED lets the
// daemon run against a herdr protocol this build was not verified against
// (ADR-001 A4). Only a boolean is accepted.
func TestAllowUnverifiedClasses(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    bool
		wantErr bool
	}{
		"1":                      {value: "1", want: true},
		"true":                   {value: "true", want: true},
		"0":                      {value: "0"},
		"false":                  {value: "false"},
		"empty":                  {value: ""},
		"not a bool":             {value: "maybe", wantErr: true},
		"surrounding whitespace": {value: " 1", wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HERDR_AGENTISAN_ALLOW_UNVERIFIED", tc.value)
			cfg, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.AllowUnverified)
		})
	}
}

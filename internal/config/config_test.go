package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nerds-run/go-agents/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	// Not parallel: Load reads process environment.
	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, "development", cfg.Env)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, 15*time.Second, cfg.ReadTimeout)
	assert.False(t, cfg.IsProduction())

	assert.Equal(t, ":9090", cfg.AdminAddr)
	assert.Equal(t, "go-agents", cfg.ServiceName)
	assert.Empty(t, cfg.OTLPEndpoint, "tracing export must be off until a collector is named")
	assert.InDelta(t, 1.0, cfg.TraceSampleRatio, 0)
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("GO_AGENTS_ENV", "production")
	t.Setenv("GO_AGENTS_HTTP_ADDR", ":9999")
	t.Setenv("GO_AGENTS_LOG_FORMAT", "json")
	t.Setenv("GO_AGENTS_API_TOKEN", "tok")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, ":9999", cfg.HTTPAddr)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.True(t, cfg.IsProduction())
}

func TestLoadReadsTelemetryEnvironment(t *testing.T) {
	t.Setenv("GO_AGENTS_ADMIN_ADDR", ":9101")
	t.Setenv("GO_AGENTS_OTLP_ENDPOINT", "otel-collector:4318")
	t.Setenv("GO_AGENTS_TRACE_SAMPLE_RATIO", "0.25")
	t.Setenv("GO_AGENTS_SERVICE_NAME", "go-agents-canary")

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, ":9101", cfg.AdminAddr)
	assert.Equal(t, "otel-collector:4318", cfg.OTLPEndpoint)
	assert.InDelta(t, 0.25, cfg.TraceSampleRatio, 0)
	assert.Equal(t, "go-agents-canary", cfg.ServiceName)
}

// TestProductionRequiresAToken is the point of the whole package: a deployed
// environment must not silently fall back to an unauthenticated service.
func TestProductionRequiresAToken(t *testing.T) {
	t.Setenv("GO_AGENTS_ENV", "production")
	t.Setenv("GO_AGENTS_API_TOKEN", "")

	_, err := config.Load()
	require.ErrorIs(t, err, config.ErrMissingSecret)
}

func TestStagingRequiresATokenToo(t *testing.T) {
	t.Setenv("GO_AGENTS_ENV", "staging")
	t.Setenv("GO_AGENTS_API_TOKEN", "")

	_, err := config.Load()
	require.ErrorIs(t, err, config.ErrMissingSecret)
}

func TestDevelopmentMayRunUnauthenticated(t *testing.T) {
	t.Setenv("GO_AGENTS_ENV", "development")
	t.Setenv("GO_AGENTS_API_TOKEN", "")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.APIToken)
}

func TestValidate(t *testing.T) {
	t.Parallel()

	base := config.Config{
		Env: "development", LogLevel: "info", LogFormat: "console", HTTPAddr: ":8080",
		AdminAddr: ":9090", ServiceName: "go-agents", TraceSampleRatio: 1,
	}

	tests := map[string]struct {
		mutate  func(c *config.Config)
		wantErr bool
	}{
		"valid":              {mutate: func(*config.Config) {}},
		"unknown env":        {mutate: func(c *config.Config) { c.Env = "qa" }, wantErr: true},
		"unknown log format": {mutate: func(c *config.Config) { c.LogFormat = "xml" }, wantErr: true},
		"empty addr":         {mutate: func(c *config.Config) { c.HTTPAddr = "" }, wantErr: true},
		"empty admin addr":   {mutate: func(c *config.Config) { c.AdminAddr = "" }, wantErr: true},
		// The invariant worth a test of its own: one listener for both would
		// publish /debug/pprof on the port users reach.
		"admin addr shared with the api": {
			mutate:  func(c *config.Config) { c.AdminAddr = c.HTTPAddr },
			wantErr: true,
		},
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
		"production without a token": {
			mutate:  func(c *config.Config) { c.Env = "production" },
			wantErr: true,
		},
		"production with a token": {
			mutate: func(c *config.Config) { c.Env = "production"; c.APIToken = "t" },
		},
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

func TestInvalidEnvIsRejectedByLoad(t *testing.T) {
	t.Setenv("GO_AGENTS_ENV", "qa")

	_, err := config.Load()
	require.Error(t, err)
}

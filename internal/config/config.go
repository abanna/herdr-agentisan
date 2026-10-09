// Package config loads runtime configuration from the environment.
//
// Precedence is flags > environment > .env file > defaults. No secret carries
// a default: when one is added, Load must return an error rather than silently
// fall back, so a missing credential fails at startup instead of at first use.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

// EnvPrefix namespaces every environment variable this service reads.
const EnvPrefix = "GO_AGENTS"

// dotenvFile is the optional developer file Load reads from the working
// directory.
const dotenvFile = ".env"

// Config is the fully resolved runtime configuration.
type Config struct {
	// Env is the deployment environment: development, staging or production.
	Env string `mapstructure:"env"`
	// LogLevel is a zerolog level name: trace, debug, info, warn, error.
	LogLevel string `mapstructure:"log_level"`
	// LogFormat is "json" for machine-readable output or "console" for local use.
	LogFormat string `mapstructure:"log_format"`

	// ServiceName labels traces.
	ServiceName string `mapstructure:"service_name"`
	// OTLPEndpoint is the host:port of the OTLP/HTTP collector. Empty disables
	// span export, which is what a plain `go run` on a laptop wants.
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
	// TraceSampleRatio is the head-sampling ratio in [0,1].
	TraceSampleRatio float64 `mapstructure:"trace_sample_ratio"`
}

// IsProduction reports whether the process is running in a deployed environment.
func (c Config) IsProduction() bool { return c.Env == "production" }

// Load resolves configuration from .env (if present), the environment and
// defaults, then validates it.
func Load() (Config, error) {
	// A missing .env is not an error: most environments inject real
	// variables and never ship the file. Anything but a regular file is
	// skipped before godotenv opens it — opening a FIFO blocks until a writer
	// appears, which would hang startup on a developer convenience. Stat
	// follows symlinks, so a link to a regular file still loads.
	if info, err := os.Stat(dotenvFile); err == nil && info.Mode().IsRegular() {
		_ = godotenv.Load(dotenvFile)
	}

	v := viper.New()
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetDefault("env", "development")
	v.SetDefault("log_level", "info")
	v.SetDefault("log_format", "console")
	v.SetDefault("service_name", "go-agents")
	v.SetDefault("otlp_endpoint", "")
	v.SetDefault("trace_sample_ratio", 1.0)

	// AutomaticEnv only resolves keys viper already knows about, and
	// SetDefault is what registers them — so every key above must have a
	// default, even an empty one.
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks invariants that must hold before the process starts.
func (c Config) Validate() error {
	switch c.Env {
	case "development", "staging", "production":
	default:
		return fmt.Errorf("invalid env %q: want development, staging or production", c.Env)
	}
	switch c.LogFormat {
	case "json", "console":
	default:
		return fmt.Errorf("invalid log_format %q: want json or console", c.LogFormat)
	}
	if c.ServiceName == "" {
		return errors.New("service_name must not be empty")
	}
	if c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		return fmt.Errorf("invalid trace_sample_ratio %v: want a ratio in [0,1]", c.TraceSampleRatio)
	}
	return nil
}

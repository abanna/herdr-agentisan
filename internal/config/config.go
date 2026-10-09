// Package config loads runtime configuration from the environment.
//
// Precedence is flags > environment > .env file > defaults. No secret carries
// a default: Load returns an error rather than silently falling back, so a
// missing credential fails at startup instead of at the first request.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

// EnvPrefix namespaces every environment variable this service reads.
const EnvPrefix = "GO_AGENTS"

// ErrMissingSecret is returned when a required secret is absent.
var ErrMissingSecret = errors.New("required secret is not set")

// Listen-address defaults. They are constants rather than literals inside
// Load because the Kubernetes manifests must agree with them: `devctl
// manifests` reads these and fails when a containerPort drifts from the port
// the binary actually binds. A probe pointed at the wrong port is a readiness
// check that never passes, and nothing else in the repo would catch it.
const (
	// DefaultHTTPAddr is the published API listener.
	DefaultHTTPAddr = ":8080"
	// DefaultAdminAddr is the unpublished /metrics and /debug/pprof listener.
	DefaultAdminAddr = ":9090"
)

// DefaultShutdownTimeout bounds connection draining. The Kubernetes manifest's
// terminationGracePeriodSeconds must exceed it, or the kubelet SIGKILLs the
// process partway through the drain it was told to perform — `devctl
// manifests` checks exactly that.
const DefaultShutdownTimeout = 10 * time.Second

// Config is the fully resolved runtime configuration.
type Config struct {
	// Env is the deployment environment: development, staging or production.
	Env string `mapstructure:"env"`
	// LogLevel is a zerolog level name: trace, debug, info, warn, error.
	LogLevel string `mapstructure:"log_level"`
	// LogFormat is "json" for machine-readable output or "console" for local use.
	LogFormat string `mapstructure:"log_format"`

	// HTTPAddr is the listen address for the REST API.
	HTTPAddr string `mapstructure:"http_addr"`
	// ReadTimeout bounds how long reading a request may take.
	ReadTimeout time.Duration `mapstructure:"read_timeout"`
	// WriteTimeout bounds how long writing a response may take.
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	// IdleTimeout bounds how long an idle keep-alive connection is held.
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
	// ShutdownTimeout bounds graceful shutdown before connections are dropped.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`

	// APIToken guards mutating API routes. Required outside development.
	APIToken string `mapstructure:"api_token"`

	// AdminAddr is the listen address for /metrics and /debug/pprof. It is a
	// SEPARATE listener from HTTPAddr on purpose: pprof on a public port hands
	// an attacker heap dumps and goroutine stacks, and a Service that never
	// routes this port cannot be asked for them.
	AdminAddr string `mapstructure:"admin_addr"`
	// ServiceName labels traces and the build_info metric.
	ServiceName string `mapstructure:"service_name"`
	// OTLPEndpoint is the host:port of the OTLP/HTTP collector. Empty disables
	// span export, which is what a plain `go run` on a laptop wants.
	OTLPEndpoint string `mapstructure:"otlp_endpoint"`
	// TraceSampleRatio is the head-sampling ratio in [0,1]. 1 keeps every
	// trace, which is right for a local cluster and wrong for production load.
	TraceSampleRatio float64 `mapstructure:"trace_sample_ratio"`
}

// IsProduction reports whether the service is running in a deployed environment.
func (c Config) IsProduction() bool { return c.Env == "production" }

// Load resolves configuration from .env (if present), the environment and
// defaults, then validates it.
func Load() (Config, error) {
	// A missing .env is not an error: deployed environments inject real
	// variables and never ship the file.
	_ = godotenv.Load()

	v := viper.New()
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetDefault("env", "development")
	v.SetDefault("log_level", "info")
	v.SetDefault("log_format", "console")
	v.SetDefault("http_addr", DefaultHTTPAddr)
	v.SetDefault("read_timeout", 15*time.Second)
	v.SetDefault("write_timeout", 15*time.Second)
	v.SetDefault("idle_timeout", 60*time.Second)
	v.SetDefault("shutdown_timeout", DefaultShutdownTimeout)
	v.SetDefault("api_token", "")
	v.SetDefault("admin_addr", DefaultAdminAddr)
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

// Validate checks invariants that must hold before the service starts.
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
	if c.HTTPAddr == "" {
		return errors.New("http_addr must not be empty")
	}
	if c.AdminAddr == "" {
		return errors.New("admin_addr must not be empty")
	}
	// Collapsing the two onto one listener would publish /debug/pprof on the
	// port users reach. Refuse rather than quietly serve heap dumps.
	if c.AdminAddr == c.HTTPAddr {
		return fmt.Errorf("admin_addr %q must differ from http_addr: pprof would be publicly reachable", c.AdminAddr)
	}
	if c.ServiceName == "" {
		return errors.New("service_name must not be empty")
	}
	if c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		return fmt.Errorf("invalid trace_sample_ratio %v: want a ratio in [0,1]", c.TraceSampleRatio)
	}
	// Development may run unauthenticated for convenience; a deployed
	// environment may not. Failing here is the point — see package doc.
	if c.Env != "development" && c.APIToken == "" {
		return fmt.Errorf("%w: %s_API_TOKEN", ErrMissingSecret, EnvPrefix)
	}
	return nil
}

// Package main is the entrypoint for the REST API server.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/nerds-run/go-agents/internal/api"
	"github.com/nerds-run/go-agents/internal/config"
	"github.com/nerds-run/go-agents/internal/logging"
	"github.com/nerds-run/go-agents/internal/notes"
	"github.com/nerds-run/go-agents/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, err := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}

	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	// SIGINT/SIGTERM cancels the context, which triggers graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telCfg := telemetry.Config{
		ServiceName:  cfg.ServiceName,
		Version:      config.Version,
		Commit:       config.Commit,
		Env:          cfg.Env,
		OTLPEndpoint: cfg.OTLPEndpoint,
		SampleRatio:  cfg.TraceSampleRatio,
	}
	// This does NOT verify the collector is reachable, and deliberately so.
	//
	// `otlptracehttp.New` is lazy: it builds a client and returns, and nothing
	// dials until the first batch export. An earlier comment here claimed a
	// dead collector would fail startup — it does not. Spans are queued,
	// retried and eventually dropped by the batch processor while the service
	// keeps serving.
	//
	// That is the behaviour we want, so the fix is the comment rather than the
	// code. Failing startup would make the collector a hard dependency of the
	// API: a telemetry outage would become an application outage, and pods
	// would CrashLoop on a problem that costs nothing but visibility. What
	// notices instead is the other side — no traces arriving is what
	// `task kind:smoke` asserts on, and the collector exports its own metrics
	// to Prometheus.
	flushTraces, err := telemetry.SetupTracing(ctx, telCfg)
	if err != nil {
		return fmt.Errorf("setup tracing: %w", err)
	}
	metrics := telemetry.NewRegistry(telCfg)

	srv := api.NewServer(notes.NewMemStore(), log, cfg.APIToken, api.WithObservers(
		telemetry.TracingMiddleware(cfg.ServiceName),
		metrics.Middleware(),
	))
	httpSrv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      srv.Router(),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		// Bound the header read separately so a slowloris client cannot
		// hold a connection open through the read timeout alone.
		ReadHeaderTimeout: cfg.ReadTimeout,
	}
	adminSrv := metrics.AdminServer(cfg.AdminAddr)

	errCh := make(chan error, 2)
	go func() { errCh <- serve(adminSrv, "admin") }()
	go func() {
		log.Info().
			Str("addr", cfg.HTTPAddr).
			Str("admin_addr", cfg.AdminAddr).
			Str("env", cfg.Env).
			Str("version", config.Version).
			Str("commit", config.Commit).
			Str("otlp_endpoint", cfg.OTLPEndpoint).
			Bool("auth_enabled", cfg.APIToken != "").
			Msg("server listening")
		errCh <- serve(httpSrv, "api")
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
		// A listener returning ErrServerClosed without a shutdown signal
		// means the other half is already draining; fall through and drain
		// this one too rather than exiting with a half-served process.
	case <-ctx.Done():
		log.Info().Msg("shutdown signal received, draining connections")
	}

	// The shutdown context is deliberately not derived from ctx: ctx is
	// already cancelled by the signal, so a child would expire immediately
	// and every in-flight request would be cut rather than drained.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	var shutdownErr error
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		shutdownErr = fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
		shutdownErr = fmt.Errorf("admin shutdown: %w", err)
	}
	// Flush last: spans recorded by requests drained above would be lost if
	// the exporter were closed first.
	if err := flushTraces(shutdownCtx); err != nil && shutdownErr == nil {
		shutdownErr = fmt.Errorf("flush traces: %w", err)
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	log.Info().Msg("server stopped")
	return nil
}

// serve runs one listener, treating a deliberate shutdown as success.
func serve(s *http.Server, name string) error {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s on %s: %w", name, s.Addr, err)
	}
	return nil
}

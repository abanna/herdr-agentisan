// Package main is the entrypoint for the `herdr-agentisan` plugin binary.
//
// It stays a thin shim: config, logging, tracing, signal handling and fang
// styling only. The command tree lives in internal/cli so it is testable
// without spawning a process.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
)

// flushTimeout bounds the tracer flush on exit. A plugin process is
// short-lived and holds one of herdr's in-flight slots until it exits, so an
// unreachable collector must not stall it.
const flushTimeout = 3 * time.Second

func main() {
	os.Exit(run())
}

// run holds the body so deferred cleanup executes before the process exits;
// calling os.Exit directly from main would skip every defer.
func run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\ninterrupted (Ctrl+C again to force quit)")
		cancel()
		// Restore the default disposition so a second signal kills the
		// process immediately, matching docker, kubectl and git.
		signal.Reset(os.Interrupt, syscall.SIGTERM)
	}()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-agentisan:", err)
		return 1
	}
	// Logs go to stderr only: stdout carries command output and the --json
	// contract herdr and scripts parse.
	logger, err := logging.New(os.Stderr, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-agentisan:", err)
		return 1
	}
	shutdown, err := telemetry.SetupTracing(ctx, telemetry.Config{
		ServiceName:  cfg.ServiceName,
		Version:      config.Version,
		Commit:       config.Commit,
		Env:          cfg.Env,
		OTLPEndpoint: cfg.OTLPEndpoint,
		SampleRatio:  cfg.TraceSampleRatio,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-agentisan:", err)
		return 1
	}
	defer func() {
		flushCtx, cancelFlush := context.WithTimeout(context.Background(), flushTimeout)
		defer cancelFlush()
		if err := shutdown(flushCtx); err != nil {
			logger.Warn().Err(err).Msg("flush traces")
		}
	}()
	ctx = logging.Into(ctx, logger)
	ctx = config.Into(ctx, cfg)

	if err := fang.Execute(ctx, cli.Root(),
		fang.WithVersion(config.Version),
		fang.WithCommit(config.Commit),
	); err != nil {
		return 1
	}
	return 0
}

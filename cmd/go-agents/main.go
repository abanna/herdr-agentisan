// Package main is the entrypoint for the `go-agents` product CLI.
//
// It stays a thin shim: signal handling and fang styling only. The command
// tree lives in internal/cli so it is testable without spawning a process.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/fang"

	"github.com/nerds-run/go-agents/internal/cli"
	"github.com/nerds-run/go-agents/internal/config"
)

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

	if err := fang.Execute(ctx, cli.Root(),
		fang.WithVersion(config.Version),
		fang.WithCommit(config.Commit),
	); err != nil {
		return 1
	}
	return 0
}

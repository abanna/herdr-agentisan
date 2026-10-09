// Package main is the entrypoint for `devctl`, the development operations CLI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/fang"

	"github.com/abanna/herdr-agentisan/internal/config"
	"github.com/abanna/herdr-agentisan/internal/devctl"
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
		fmt.Fprintln(os.Stderr, "\ninterrupted")
		cancel()
		signal.Reset(os.Interrupt, syscall.SIGTERM)
	}()

	if err := fang.Execute(ctx, devctl.Root(),
		fang.WithVersion(config.Version),
		fang.WithCommit(config.Commit),
	); err != nil {
		return 1
	}
	return 0
}

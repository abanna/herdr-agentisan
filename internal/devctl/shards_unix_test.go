//go:build unix

package devctl_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/devctl"
)

// TestModulePackagesStopsAHungGoList: a go list that never ends, and whose
// child keeps its output open after it is killed, returns once the caller's
// context ends, the way the CLI's own deadline and an interrupt end it.
func TestModulePackagesStopsAHungGoList(t *testing.T) {
	bin := t.TempDir()
	// sh waits on a sleep it started: killing sh leaves the sleep holding
	// the pipe go list's output goes to.
	script := "#!/bin/sh\nsleep 30\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700)) //nolint:gosec // an executable stand-in
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := devctl.ModulePackages(ctx, "../..", false)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 10*time.Second, "neither go list nor its child holds the call")
}

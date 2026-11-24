package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
	"github.com/abanna/herdr-agentisan/internal/testtmp"
)

// stubDir holds the stand-in claude that runLine puts first on PATH. TestMain
// writes it, once, before any test starts.
var stubDir string

// claudeStub prints its working directory, HERDR_PLUGIN_CONFIG_DIR and each
// argument, one per line.
const claudeStub = "#!/bin/sh\npwd\nprintf '%s\\n' \"$HERDR_PLUGIN_CONFIG_DIR\"\nfor a; do printf '%s\\n' \"$a\"; done\n"

// TestMain puts the tests' temporary directories on tmpfs: each daemon these
// tests start opens a fresh state.db there, whose fsyncs then never wait on a
// busy disk's journal (NERD-5503). It also writes the stand-in claude before
// any test can fork: a file written while another goroutine forks is
// inherited, still open for writing, by that fork's child until the child
// execs, and a shell that execs it in that window fails with "Text file
// busy" (ETXTBSY, NERD-5645). Nothing forks before m.Run, so no child ever
// holds the stub's write descriptor.
func TestMain(m *testing.M) {
	if bundletest.Active() {
		bundletest.Main() // a stand-in bundle server: stageread_test.go
	}
	testtmp.Use()
	dir, err := os.MkdirTemp("", "claude-stub-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cli tests: the stand-in claude's directory:", err)
		os.Exit(1)
	}
	stubDir = dir
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claudeStub), 0o700); err != nil { //nolint:gosec // an executable stand-in
		fmt.Fprintln(os.Stderr, "cli tests: the stand-in claude:", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

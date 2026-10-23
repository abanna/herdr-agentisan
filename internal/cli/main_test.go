package cli_test

import (
	"os"
	"testing"

	"github.com/abanna/herdr-agentisan/internal/testtmp"
)

// TestMain puts the tests' temporary directories on tmpfs: each daemon these
// tests start opens a fresh state.db there, whose fsyncs then never wait on a
// busy disk's journal (NERD-5503).
func TestMain(m *testing.M) {
	testtmp.Use()
	os.Exit(m.Run())
}

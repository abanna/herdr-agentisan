package bundle_test

import (
	"os"
	"testing"

	"github.com/abanna/herdr-agentisan/internal/bundle/bundletest"
	"github.com/abanna/herdr-agentisan/internal/testtmp"
)

// TestMain is the stand-in server when a test re-runs this binary as one
// (bundletest), and otherwise puts the tests' state on tmpfs (NERD-5503).
func TestMain(m *testing.M) {
	if bundletest.Active() {
		bundletest.Main()
	}
	testtmp.Use()
	os.Exit(m.Run())
}

func TestTheTestsRunOnTmpfs(t *testing.T) {
	t.Parallel()
	testtmp.CheckUsed(t)
}

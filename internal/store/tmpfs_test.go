package store_test

import (
	"testing"

	"github.com/abanna/herdr-agentisan/internal/testtmp"
)

// TestTheTestsRunOnTmpfs: this package's TestMain calls testtmp.Use, so its
// tests' state is on tmpfs wherever tmpfs is available (NERD-5503).
func TestTheTestsRunOnTmpfs(t *testing.T) {
	t.Parallel()
	testtmp.CheckUsed(t)
}

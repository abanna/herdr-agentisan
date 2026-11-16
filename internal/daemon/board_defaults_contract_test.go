package daemon_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestBoardDefaultsMatchA60 pins the documented policy independently of
// the constants used by the existing board behavior tests.
func TestBoardDefaultsMatchA60(t *testing.T) {
	t.Parallel()

	t.Run("wait", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 30*time.Minute, daemon.DefaultBoardWait, "A60 default wait")
	})
	t.Run("limit", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 50, daemon.DefaultBoardLimit, "A60 default read limit")
	})
	t.Run("retention", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, 30*24*time.Hour, store.BoardRetention, "A60 retention")
	})
}

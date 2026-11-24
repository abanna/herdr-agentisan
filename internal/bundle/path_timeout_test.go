package bundle

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWorkDirResolutionHonorsDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ResolveWithin(ctx, func() (string, error) {
		time.Sleep(2 * time.Second)
		return "/late", nil
	})
	require.ErrorIs(t, err, ErrTimeout)
	require.Less(t, time.Since(start), time.Second)
}

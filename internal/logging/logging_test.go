package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/logging"
)

func TestNewJSONLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg, err := logging.New(&buf, "info", "json")
	require.NoError(t, err)

	lg.Info().Str("key", "value").Msg("hello")

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, "hello", got["message"])
	assert.Equal(t, "value", got["key"])
	assert.Contains(t, got, "time")
}

func TestNewConsoleLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg, err := logging.New(&buf, "debug", "console")
	require.NoError(t, err)

	lg.Debug().Msg("visible")
	assert.Contains(t, buf.String(), "visible")
}

func TestLevelFiltering(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg, err := logging.New(&buf, "error", "json")
	require.NoError(t, err)

	lg.Info().Msg("should be dropped")
	assert.Empty(t, buf.String(), "an info line must not survive an error-level logger")

	lg.Error().Msg("should appear")
	assert.Contains(t, buf.String(), "should appear")
}

func TestNewRejectsBadLevel(t *testing.T) {
	t.Parallel()

	_, err := logging.New(&bytes.Buffer{}, "shouty", "json")
	require.Error(t, err)
}

func TestContextRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg, err := logging.New(&buf, "info", "json")
	require.NoError(t, err)

	ctx := logging.Into(t.Context(), lg)
	got := logging.From(ctx)
	got.Info().Msg("through the context")

	assert.Contains(t, buf.String(), "through the context")
}

// TestFromEmptyContextIsSafe guards the reason From returns a value rather
// than a pointer: callers must not need a nil check at every use site.
func TestFromEmptyContextIsSafe(t *testing.T) {
	t.Parallel()

	assert.NotPanics(t, func() {
		lg := logging.From(context.Background())
		lg.Info().Msg("discarded")
	})
}

package telemetry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"

	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
)

// TestBoardPostedEmitsTheSchema is A74's contract for a board flush, as
// docs/telemetry.md lists it: its name, exactly its five attributes, the
// body line and the severity, spelled out by hand. A nil *Events emits
// nothing.
func TestBoardPostedEmitsTheSchema(t *testing.T) {
	t.Parallel()
	events, rec := telemetrytest.NewEvents()
	events.BoardPosted(t.Context(), telemetry.BoardPost{Project: "ha", Channel: "daemon", Posts: 3, FirstSeq: 7, LastSeq: 9})

	got := rec.Events()
	require.Len(t, got, 1)
	assert.Equal(t, "agentisan.board.posted", got[0].Name)
	assert.Equal(t, map[string]any{
		"agentisan.board.project": "ha", "agentisan.board.channel": "daemon",
		"agentisan.board.posts": int64(3), "agentisan.board.first_seq": int64(7), "agentisan.board.last_seq": int64(9),
	}, got[0].Attrs, "exactly the schema's attributes, no more")
	records := rec.Records()
	assert.Equal(t, log.SeverityInfo, records[0].Severity())
	assert.Equal(t, "INFO", records[0].SeverityText())
	assert.Equal(t, "3 posts on ha's board", records[0].Body().AsString())
	assert.False(t, records[0].Timestamp().IsZero())

	var none *telemetry.Events
	assert.NotPanics(t, func() { none.BoardPosted(t.Context(), telemetry.BoardPost{Project: "ha", Posts: 1}) })
}

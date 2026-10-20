package trail_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/trail"
)

// TestWorkerQuery: the item-12 recipe, exactly. It reads the trail's
// stream, matches the id as the worker id or the pane id (a worker's events
// carry its pane id until it is bound), and names every field through
// telemetry's constants, so renaming an attribute changes the query too.
func TestWorkerQuery(t *testing.T) {
	t.Parallel()
	q, err := trail.WorkerQuery("coders-1", time.Hour)
	require.NoError(t, err)
	assert.Equal(t, `FROM logs-agentisan.otel-*
| WHERE event_name == "agentisan.worker.status_changed"
    AND (attributes.agentisan.worker.id == "coders-1" OR attributes.herdr.pane.id == "coders-1")
    AND @timestamp > NOW() - 60 minutes
| SORT @timestamp
| KEEP @timestamp, attributes.agentisan.worker.id, attributes.herdr.pane.id, attributes.agentisan.status.from, attributes.agentisan.status.to, attributes.agentisan.group, attributes.agentisan.worker.bound
| LIMIT 1000`, q)
	for _, attr := range []string{telemetry.AttrWorkerID, telemetry.AttrPaneID, telemetry.AttrFrom, telemetry.AttrTo, telemetry.AttrGroup, telemetry.AttrWorkerBound} {
		assert.Contains(t, q, "attributes."+attr)
	}
	require.NoError(t, trail.CheckQuery(q), "the recipe passes the trail check")
}

// TestWorkerQueryInputs: an id becomes an ES|QL string literal, escaped,
// never spliced; the window is whole minutes within its bounds.
func TestWorkerQueryInputs(t *testing.T) {
	t.Parallel()
	ids := map[string]struct {
		id, lit string
	}{
		"a pane id":       {id: "w14:p8", lit: `"w14:p8"`},
		"a quote":         {id: `x" OR true OR "`, lit: `"x\" OR true OR \""`},
		"a backslash":     {id: `a\b`, lit: `"a\\b"`},
		"line breaks/tab": {id: "a\nb\rc\td", lit: `"a\nb\rc\td"`},
		"non-ASCII":       {id: "kódér-1", lit: `"kódér-1"`},
	}
	for name, tc := range ids {
		q, err := trail.WorkerQuery(tc.id, time.Hour)
		require.NoError(t, err, name)
		assert.Contains(t, q, "== "+tc.lit+" OR", name)
	}
	for name, id := range map[string]string{
		"empty": "", "too long": strings.Repeat("x", 129), "a NUL": "a\x00b", "an escape": "a\x1bb", "invalid UTF-8": "a\xffb",
	} {
		_, err := trail.WorkerQuery(id, time.Hour)
		require.ErrorIs(t, err, trail.ErrInvalid, name)
	}
	for since, ok := range map[time.Duration]bool{
		time.Minute: true, 7 * 24 * time.Hour: true, 0: false, 30 * time.Second: false,
		90 * time.Second: false, 7*24*time.Hour + time.Minute: false, -time.Hour: false,
	} {
		_, err := trail.WorkerQuery("w1:p1", since)
		if ok {
			require.NoError(t, err, since)
		} else {
			require.ErrorIs(t, err, trail.ErrInvalid, since)
		}
	}
}

// TestCheckQuery: an ad-hoc query reads the trail's stream and nothing else.
func TestCheckQuery(t *testing.T) {
	t.Parallel()
	for q, ok := range map[string]bool{
		"FROM logs-agentisan.otel-* | LIMIT 5":               true,
		"  from logs-agentisan.otel-*\n| STATS n = COUNT(*)": true,
		"FROM logs-agentisan.otel-*":                         true,
		"FROM logs-* | LIMIT 5":                              false,
		"FROM logs-agentisan.otel-*,.security | LIMIT 1":     false,
		"FROM logs-agentisan.otel-default | LIMIT 1":         false,
		"ROW a = 1": false,
		"SHOW INFO": false,
		"":          false,
	} {
		err := trail.CheckQuery(q)
		if ok {
			require.NoError(t, err, q)
		} else {
			require.ErrorIs(t, err, trail.ErrInvalid, q)
		}
	}
}

// TestChangesNeedsTheRecipeColumns: an answer missing one of the recipe's
// columns is refused by name, not read as empty values.
func TestChangesNeedsTheRecipeColumns(t *testing.T) {
	t.Parallel()
	_, err := trail.Changes(trail.Table{Columns: []trail.Column{{Name: "@timestamp", Type: "date"}}})
	require.ErrorIs(t, err, trail.ErrUnavailable)
	assert.Contains(t, err.Error(), "attributes.agentisan.worker.id")
}

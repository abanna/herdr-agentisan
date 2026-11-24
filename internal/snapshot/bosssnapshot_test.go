package snapshot_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

const bossJSON = `{"v":1,"team":"alpha","at":"2026-11-24T09:00:03Z","team_poll":{"as_of":"2026-11-24T09:00:00Z","state":"stale"},"boss":null,"missing_boss_name":null,"groups":[{"name":"coder","workers":[{"id":null,"generation":null,"pane_id":"p1","name":"w","agent":null,"status":"unknown","item":null,"stage":null,"stage_at":null,"stage_stale":false,"idle_since":null,"ctx":0}]}],"workers_omitted":0,"board":{"as_of":null,"state":"unavailable","mark":null,"newest":null,"unread":null}}`

// NERD-5748: exact keys, bounded metadata and a distinction between null and zero.
func TestBossSnapshotJSONStrict(t *testing.T) {
	t.Parallel()
	got, err := snapshot.DecodeBoss(strings.NewReader(bossJSON))
	require.NoError(t, err)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, bossJSON, string(raw))
	_, err = snapshot.DecodeBoss(strings.NewReader(strings.Replace(bossJSON, `"stage":null`, `"stage":"`+strings.Repeat("調", 80)+`"`, 1)))
	require.NoError(t, err, "240 bytes, but 80 characters as the reporter bounds a stage")
	worker := bossJSON[strings.Index(bossJSON, `{"id":`) : strings.Index(bossJSON, `}]}`)+1]
	for _, tc := range []struct{ name, from, to string }{
		{"invalid missing boss name", `"missing_boss_name":null`, `"missing_boss_name":"al-boss\n"`},
		{"version", `"v":1`, `"v":2`},
		{"case", `"team":`, `"Team":`},
		{"duplicate", `"ctx":0`, `"ctx":null,"ctx":0`},
		{"missing", `,"ctx":0`, ``},
		{"unknown field", `"ctx":0`, `"ctx":0,"argv":[]`},
		{"null count", `"workers_omitted":0`, `"workers_omitted":null`},
		{"null array", `"workers":[` + worker + "]", `"workers":null`},
		{"null false", `"stage_stale":false`, `"stage_stale":null`},
		{"status", `"unknown"`, `"running"`},
		{"freshness", `"stale"`, `"old"`},
		{"board fresher than the reply", `"board":{"as_of":null,"state":"unavailable","mark":null,"newest":null,"unread":null}`, `"board":{"as_of":"2026-11-24T09:00:04Z","state":"fresh","mark":0,"newest":0,"unread":0}`},
		{"team poll fresher than the reply", `"team_poll":{"as_of":"2026-11-24T09:00:00Z"`, `"team_poll":{"as_of":"2026-11-24T09:00:04Z"`},
		{"unavailable count", `"mark":null`, `"mark":0`},
		{"time spelling", `T09:00:00Z`, `T09:00:00,0Z`},
		{"time value", `T09:00:03Z`, `T09:60:03Z`},
		{"UTF-8", `"w"`, "\"\xff\""},
		{"field length", `"w"`, "\"" + strings.Repeat("a", 257) + "\""},
		{"item length", `"item":null`, `"item":"` + strings.Repeat("a", 81) + `"`},
		{"stage length", `"stage":null`, `"stage":"` + strings.Repeat("a", 81) + `"`},
		{"context", `"ctx":0`, `"ctx":101`},
		{"binding", `"generation":null`, `"generation":1`},
		{"worker cap", worker, strings.Repeat(worker+",", 128) + worker},
		{"size", bossJSON, bossJSON + strings.Repeat(" ", 1<<20)},
		{"trailing value", bossJSON, bossJSON + " {}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(bossJSON, tc.from, tc.to, 1)
			require.NotEqual(t, bossJSON, input)
			_, err := snapshot.DecodeBoss(strings.NewReader(input))
			require.Error(t, err)
		})
	}
	t.Run("text cap", func(t *testing.T) {
		extra := got.Groups[0].Workers[0]
		extra.Name = strings.Repeat("w", 32)
		for i := 1; i < snapshot.MaxBossWorkers; i++ {
			extra.PaneID = fmt.Sprintf("p%d", i+1)
			got.Groups[0].Workers = append(got.Groups[0].Workers, extra)
		}
		got.WorkersOmitted = 2
		var out strings.Builder
		require.NoError(t, got.WriteText(&out))
		assert.LessOrEqual(t, out.Len(), snapshot.MaxBossTextBytes)
		visible := strings.Count(out.String(), "\n  ")
		require.Positive(t, visible)
		require.Less(t, visible, snapshot.MaxBossWorkers)
		assert.Contains(t, out.String(), fmt.Sprintf("%d workers omitted", snapshot.MaxBossWorkers+2-visible))
	})
}

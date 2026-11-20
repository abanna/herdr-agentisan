package workforce_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/routing"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// replayKey is the replay tests' retry key; begun and answered are when
// its request began and was first answered.
var (
	replayKey = strings.Repeat("3f9c0d5e", 4)
	begun     = time.Date(2026, 11, 17, 20, 0, 41, 0, time.UTC)
	answered  = time.Date(2026, 11, 17, 20, 1, 2, 987654321, time.UTC)
)

// replayPrefix opens every replayed refusal's message.
const replayPrefix = "replayed (first answered 2026-11-17T20:01:02Z); its ids and pane ids are as they were then and may since have been reused; act on none of them: "

// linked is a link of id bound at gen whose row is now status on pane, with
// briefs; a status of "" is a link superseded since, with no row.
func linked(id string, gen int64, status, pane string, briefs int) store.SpawnLink {
	l := store.SpawnLink{LogicalID: id, Bound: true, Generation: gen, Briefs: briefs}
	if status == "" {
		return l
	}
	row := store.Row{Worker: store.Worker{LogicalID: id, Project: "ha", Group: "coder", PaneID: pane, Generation: gen, Status: status}, CreatedAt: begun}
	if status == store.StatusRetired || status == store.StatusFailed || status == store.StatusGone {
		row.RetiredAt = answered.Add(time.Hour)
	}
	l.Row = &row
	return l
}

// done is a record of replayKey answered a, planning links.
func done(a store.SpawnAnswer, links ...store.SpawnLink) store.SpawnRecord {
	return store.SpawnRecord{
		SpawnKey: store.SpawnKey{Key: replayKey, Project: "ha", Group: "coder", Count: len(links)},
		State:    "done", Answer: a, CreatedAt: begun, FinishedAt: answered, Planned: links,
	}
}

// briefs is n, as a replayed worker's briefs.
func briefs(n int) *int { return &n }

// TestReplayBuilder (B11): the replay builder, on records built by hand. An
// ok replay is the stored result, spent, replayed and answered when first
// answered, each worker as first answered but its status and briefs read
// now and its pane id only while live; a superseded one has neither pane id
// nor briefs. A refusal replays its code and message between the replay's
// prefix and what each planned worker is now, by generation, with a
// planned id never bound under the key counted and never named, even once
// another spawn has bound it.
func TestReplayBuilder(t *testing.T) {
	t.Parallel()
	first := workforce.SpawnResult{Project: "ha", Warnings: []string{"ha-coder-2 will report no ctx"}, Workers: []workforce.Spawned{
		{ID: "ha-coder-1", Group: "coder", PaneID: "w2:p1", Generation: 0, Agent: "claude", Binding: "claude_mid", Model: "claude-sonnet-5-5", Effort: "high", FallbackReason: routing.Reason("no_task")},
		{ID: "ha-coder-2", Group: "coder", PaneID: "w2:p2", Generation: 1, Agent: "codex"},
		{ID: "ha-coder-3", Group: "coder", PaneID: "w2:p3", Generation: 0, Agent: "claude"},
		{ID: "ha-coder-4", Group: "coder", PaneID: "w2:p4", Generation: 0, Agent: "claude"},
		{ID: "ha-coder-5", Group: "coder", PaneID: "w2:p5", Generation: 0, Agent: "claude"},
		{ID: "ha-coder-6", Group: "coder", PaneID: "w2:p6", Generation: 2, Agent: "claude"},
		{ID: "ha-coder-7", Group: "coder", PaneID: "w2:p7", Generation: 0, Agent: "claude"},
	}}
	result, err := json.Marshal(first)
	require.NoError(t, err)
	links := []store.SpawnLink{
		linked("ha-coder-1", 0, store.StatusActive, "w9:p1", 2),
		linked("ha-coder-2", 1, store.StatusStarting, "w9:p2", 0),
		linked("ha-coder-3", 0, store.StatusRetired, "w9:p3", 1),
		linked("ha-coder-4", 0, store.StatusFailed, "w9:p4", 0),
		linked("ha-coder-5", 0, store.StatusLost, "w9:p5", 1),
		linked("ha-coder-6", 2, "", "", 3),
		linked("ha-coder-7", 0, store.StatusGone, "w9:p7", 0),
	}

	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		got, err := workforce.Replay(done(store.SpawnAnswer{Code: "ok", Result: result}, links...))
		require.NoError(t, err)
		want := first
		want.RetryKey, want.KeyState, want.Replayed, want.AnsweredAt = replayKey, "spent", true, "2026-11-17T20:01:02Z"
		want.Workers = append([]workforce.Spawned(nil), first.Workers...)
		now := []struct {
			pane, status string
			briefs       *int
		}{
			{"w9:p1", "active", briefs(2)},
			{"w9:p2", "starting", briefs(0)},
			{"", "retired", briefs(1)},
			{"", "failed", briefs(0)},
			{"", "lost", briefs(1)},
			{"", "superseded", nil},
			{"", "gone", briefs(0)},
		}
		for i, n := range now {
			want.Workers[i].PaneID, want.Workers[i].Status, want.Workers[i].Briefs = n.pane, n.status, n.briefs
		}
		assert.Equal(t, want, got)
		out, err := json.Marshal(got)
		require.NoError(t, err)
		assert.Contains(t, string(out), `{"id":"ha-coder-2","group":"coder","pane_id":"w9:p2","generation":1,"agent":"codex","status":"starting","briefs":0}`, "0 briefs is printed")
		assert.Contains(t, string(out), `{"id":"ha-coder-6","group":"coder","generation":2,"agent":"claude","status":"superseded"}`, "a superseded worker has no pane id and no briefs")
	})

	t.Run("a refusal", func(t *testing.T) {
		t.Parallel()
		rec := done(store.SpawnAnswer{Code: "spawn_failed", Message: "ha-coder-2 did not come up on w2:p2"},
			linked("ha-coder-1", 0, store.StatusActive, "w9:p1", 1),
			linked("ha-coder-2", 3, store.StatusFailed, "w9:p2", 0),
			linked("ha-coder-3", 1, "", "", 2),
			store.SpawnLink{LogicalID: "ha-coder-4"},
			store.SpawnLink{LogicalID: "ha-coder-5"},
		)
		_, err := workforce.Replay(rec)
		var refused *workforce.RefusalError
		require.ErrorAs(t, err, &refused)
		assert.Equal(t, &workforce.RefusalError{
			Code: "spawn_failed",
			Message: replayPrefix + "ha-coder-2 did not come up on w2:p2; now: ha-coder-1 (generation 0) active, 1 brief; " +
				"ha-coder-2 (generation 3) failed, 0 briefs; ha-coder-3 (generation 1) superseded; 2 planned ids never bound",
			RetryKey: replayKey, KeyState: "spent", Err: workforce.ErrReplayed,
		}, refused)
		assert.Equal(t, refused.Message, err.Error())
	})

	t.Run("a planned id never bound under the key, bound since by another spawn", func(t *testing.T) {
		t.Parallel()
		rec := done(store.SpawnAnswer{Code: "herdr_failed", Message: "create a tab: herdr is gone"},
			linked("ha-coder-1", 0, store.StatusActive, "w9:p1", 0),
			store.SpawnLink{LogicalID: "ha-coder-2"},
		)
		_, err := workforce.Replay(rec)
		require.Error(t, err)
		assert.Equal(t, replayPrefix+"create a tab: herdr is gone; now: ha-coder-1 (generation 0) active, 0 briefs; 1 planned id never bound", err.Error())
		assert.NotContains(t, err.Error(), "ha-coder-2")
		assert.NotContains(t, workforce.NowList(rec), "ha-coder-2")
	})

	t.Run("a stored message of any bytes, answered in another zone", func(t *testing.T) {
		t.Parallel()
		rec := done(store.SpawnAnswer{Code: "spawn_failed", Message: "\ufeffa\x00b\xffc\r\nd é\xe2\x82"}, linked("ha-cöder-1", 0, store.StatusActive, "w9:p1", 0))
		rec.FinishedAt = answered.In(time.FixedZone("CST", -6*60*60))
		_, err := workforce.Replay(rec)
		require.Error(t, err)
		assert.Equal(t, replayPrefix+"\ufeffa\x00b\xffc\r\nd é\xe2\x82; now: ha-cöder-1 (generation 0) active, 0 briefs", err.Error(), "byte for byte, the time in UTC")
	})

	t.Run("a stored result holding a byte that is not UTF-8", func(t *testing.T) {
		t.Parallel()
		got, err := workforce.Replay(done(store.SpawnAnswer{Code: "ok", Result: []byte(`{"project":"ha","workers":[{"id":"ha-coder-1","group":"c` + "\xff" + `der"}]}`)},
			linked("ha-coder-1", 0, store.StatusActive, "w9:p1", 0)))
		require.NoError(t, err)
		assert.Equal(t, "c\ufffdder", got.Workers[0].Group, "decoded as encoding/json does, the byte replaced")
	})

	t.Run("the mismatch's now list", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "now: ha-coder-3 (generation 0) active, 1 brief", workforce.NowList(done(store.SpawnAnswer{Code: "ok", Result: result},
			linked("ha-coder-3", 0, store.StatusActive, "w9:p3", 1))))
		assert.Equal(t, "now: ha-coder-3 (generation 2) starting, 2 briefs; 1 planned id never bound",
			workforce.NowList(store.SpawnRecord{State: "running", Planned: []store.SpawnLink{linked("ha-coder-3", 2, store.StatusStarting, "w9:p3", 2), {LogicalID: "ha-coder-4"}}}))
	})

	t.Run("a request still running", func(t *testing.T) {
		t.Parallel()
		rec := done(store.SpawnAnswer{}, linked("ha-coder-1", 0, store.StatusStarting, "w9:p1", 0))
		rec.State, rec.FinishedAt = "running", time.Time{}
		_, err := workforce.Replay(rec)
		require.ErrorIs(t, err, workforce.ErrSpawnUnsettled, "only an answered request replays")
		var refused *workforce.RefusalError
		assert.False(t, errors.As(err, &refused), "with no key state: nothing was read as final")
	})

	t.Run("a stored result that does not decode", func(t *testing.T) {
		t.Parallel()
		for _, result := range []string{`{"workers":3}`, `{"workers":[{"id":"ha-coder-1","generation":99999999999999999999}]}`} {
			_, err := workforce.Replay(done(store.SpawnAnswer{Code: "ok", Result: []byte(result)}, linked("ha-coder-1", 0, store.StatusActive, "w9:p1", 0)))
			require.Error(t, err, result)
			var refused *workforce.RefusalError
			assert.False(t, errors.As(err, &refused), result)
		}
	})
}

// TestARefusalUnwrapsAndAReplayDoesNot (B13): a *RefusalError answers its
// message and unwraps to its cause, so its sentinel still matches; a
// replayed refusal's cause is ErrReplayed, and its old sentinel matches no
// more.
func TestARefusalUnwrapsAndAReplayDoesNot(t *testing.T) {
	t.Parallel()
	fresh := &workforce.RefusalError{
		Code: "pane_changed", Message: "w2:p1 changed", RetryKey: replayKey, KeyState: "spent",
		Err: fmt.Errorf("close w2:p1: %w", workforce.ErrPaneChanged),
	}
	var err error = fresh
	assert.ErrorIs(t, err, workforce.ErrPaneChanged)
	assert.NotErrorIs(t, err, workforce.ErrReplayed)
	assert.Equal(t, "w2:p1 changed", err.Error())

	_, err = workforce.Replay(done(store.SpawnAnswer{Code: "pane_changed", Message: "w2:p1 changed"}, linked("ha-coder-1", 0, store.StatusActive, "w9:p1", 0)))
	assert.ErrorIs(t, err, workforce.ErrReplayed)
	assert.NotErrorIs(t, err, workforce.ErrPaneChanged)
	var refused *workforce.RefusalError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, "pane_changed", refused.Code)
}

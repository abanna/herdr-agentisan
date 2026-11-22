package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// begin records key's request, running, planning ids, at at.
func begin(t *testing.T, s *store.Store, key string, at time.Time, ids ...string) {
	t.Helper()
	require.NoError(t, s.BeginSpawn(t.Context(), spawnKey(func(k *store.SpawnKey) { k.Key, k.Count = key, len(ids) }), ids, at))
}

// noRow asserts id has no workers row.
func noRow(t *testing.T, s *store.Store, id string) {
	t.Helper()
	_, err := s.WorkerRow(t.Context(), id)
	require.ErrorIs(t, err, store.ErrNoWorker, "%s has no workers row", id)
}

// linkOf is key's link for id, as SpawnRecord reads it.
func linkOf(t *testing.T, s *store.Store, key, id string) store.SpawnLink {
	t.Helper()
	rec, err := s.SpawnRecord(t.Context(), key)
	require.NoError(t, err)
	for _, l := range rec.Planned {
		if l.LogicalID == id {
			return l
		}
	}
	require.Failf(t, "not planned", "%s is not planned under the key", id)
	return store.SpawnLink{}
}

// TestBindSpawnedIsOneTransaction (B1): BindSpawned binds a worker and links
// it to its planned id in one transaction, or does neither. An unknown key,
// a done one, an id the key did not plan and one already bound under it
// write no row; a live id writes no link; a running key gets the row and its
// link at its generation, 0 for a new id and 1 for one retired before.
func TestBindSpawnedIsOneTransaction(t *testing.T) {
	t.Parallel()
	other := strings.Repeat("b", 32)
	for name, tc := range map[string]struct {
		setup func(t *testing.T, s *store.Store)
		key   string
		id    string
		want  error
		gen   int64
		// row is the workers row's status and generation after the bind,
		// or "" when id must have none.
		row   string
		check func(t *testing.T, s *store.Store)
	}{
		"an unknown key": {key: other, id: "ha-coder-1", want: store.ErrNoSpawnKey},
		"a done key": {setup: func(t *testing.T, s *store.Store) {
			require.NoError(t, s.FinishSpawn(t.Context(), retryKey, store.SpawnAnswer{Code: "spawn_failed", Message: "m"}, spawnAt))
		}, key: retryKey, id: "ha-coder-1", want: store.ErrSpawnKeyChanged},
		"an id the key did not plan": {key: retryKey, id: "ha-coder-3", want: store.ErrInvalid},
		"a live id": {setup: func(t *testing.T, s *store.Store) {
			_, err := s.BindWorker(t.Context(), fullWorker("ha-coder-1"), spawnAt)
			require.NoError(t, err)
		}, key: retryKey, id: "ha-coder-1", want: store.ErrWorkerLive, row: store.StatusStarting},
		"an id already bound under the key": {setup: func(t *testing.T, s *store.Store) {
			gen, err := s.BindSpawned(t.Context(), fullWorker("ha-coder-1"), retryKey, spawnAt)
			require.NoError(t, err)
			w := fullWorker("ha-coder-1")
			w.Generation = gen
			require.NoError(t, s.SetWorkerStatus(t.Context(), w, store.StatusRetired, spawnAt))
		}, key: retryKey, id: "ha-coder-1", want: store.ErrSpawnKeyChanged, row: store.StatusRetired},
		"an id bound under the key, still starting": {setup: func(t *testing.T, s *store.Store) {
			_, err := s.BindSpawned(t.Context(), fullWorker("ha-coder-1"), retryKey, spawnAt)
			require.NoError(t, err)
		}, key: retryKey, id: "ha-coder-1", want: store.ErrSpawnKeyChanged, row: store.StatusStarting},
		"a running key": {key: retryKey, id: "ha-coder-1", row: store.StatusStarting},
		"an id another running key plans too": {setup: func(t *testing.T, s *store.Store) {
			begin(t, s, other, spawnAt, "ha-coder-1")
		}, key: retryKey, id: "ha-coder-1", row: store.StatusStarting, check: func(t *testing.T, s *store.Store) {
			assert.Equal(t, store.SpawnLink{LogicalID: "ha-coder-1"}, linkOf(t, s, other, "ha-coder-1"), "the other key's link stays empty")
		}},
		"a retired id under a new key": {setup: func(t *testing.T, s *store.Store) {
			gen, err := s.BindWorker(t.Context(), fullWorker("ha-coder-1"), spawnAt)
			require.NoError(t, err)
			w := fullWorker("ha-coder-1")
			w.Generation = gen
			require.NoError(t, s.SetWorkerStatus(t.Context(), w, store.StatusRetired, spawnAt))
		}, key: retryKey, id: "ha-coder-1", gen: 1, row: store.StatusStarting},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			begin(t, s, retryKey, spawnAt, "ha-coder-1", "ha-coder-2")
			if tc.setup != nil {
				tc.setup(t, s)
			}
			before, err := s.SpawnRecord(t.Context(), retryKey)
			require.NoError(t, err)
			gen, err := s.BindSpawned(t.Context(), fullWorker(tc.id), tc.key, spawnAt.Add(time.Minute))
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				after, err := s.SpawnRecord(t.Context(), retryKey)
				require.NoError(t, err)
				assert.Equal(t, before, after, "the record and its links are unchanged")
				if tc.row == "" {
					noRow(t, s, tc.id)
					return
				}
				row, err := s.WorkerRow(t.Context(), tc.id)
				require.NoError(t, err)
				assert.Equal(t, tc.row, row.Status)
				assert.Zero(t, row.Generation, "the row was not bound again")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.gen, gen)
			row, err := s.WorkerRow(t.Context(), tc.id)
			require.NoError(t, err)
			assert.Equal(t, tc.row, row.Status)
			assert.Equal(t, tc.gen, row.Generation)
			assert.Equal(t, store.SpawnLink{LogicalID: tc.id, Bound: true, Generation: tc.gen, Row: &row}, linkOf(t, s, retryKey, tc.id))
			assert.Equal(t, store.SpawnLink{LogicalID: "ha-coder-2"}, linkOf(t, s, retryKey, "ha-coder-2"), "the other planned id is not bound")
			if tc.check != nil {
				tc.check(t, s)
			}
		})
	}
}

// TestBindSpawnedRollsBackAFailedLink (B2): the link step itself fails
// after the upsert ran, by a trigger, with an error (ErrStore) or changing
// no row (ErrSpawnKeyChanged), and nothing of the bind is kept: a fresh id
// has no row, and an id retired at generation 0 is still retired there,
// with its link unbound.
func TestBindSpawnedRollsBackAFailedLink(t *testing.T) {
	t.Parallel()
	// The link fails with an error, or changes no row without one.
	links := map[string]struct {
		raise string
		want  error
	}{"an error": {"RAISE(ABORT, 'injected')", store.ErrStore}, "no row changed": {"RAISE(IGNORE)", store.ErrSpawnKeyChanged}}
	for name, retired := range map[string]bool{"a fresh id": false, "an id retired at generation 0": true} {
		for how, link := range links {
			t.Run(name+", "+how, func(t *testing.T) {
				t.Parallel()
				failedLink(t, retired, link.raise, link.want)
			})
		}
	}
}

func failedLink(t *testing.T, retired bool, raise string, want error) {
	t.Helper()
	s := open(t, dbPath(t))
	ctx := t.Context()
	w := fullWorker("ha-coder-1")
	if retired {
		_, err := s.BindWorker(ctx, w, spawnAt)
		require.NoError(t, err)
		require.NoError(t, s.SetWorkerStatus(ctx, w, store.StatusRetired, spawnAt))
	}
	begin(t, s, retryKey, spawnAt, "ha-coder-1")
	_, err := store.DB(s).ExecContext(ctx, "CREATE TRIGGER fail_link BEFORE UPDATE ON spawn_request_workers BEGIN SELECT "+raise+"; END")
	require.NoError(t, err)

	_, err = s.BindSpawned(ctx, w, retryKey, spawnAt.Add(time.Minute))
	require.ErrorIs(t, err, want)
	assert.Equal(t, store.SpawnLink{LogicalID: "ha-coder-1"}, linkOf(t, s, retryKey, "ha-coder-1"), "the link is unbound")
	if !retired {
		noRow(t, s, "ha-coder-1")
		return
	}
	row, err := s.WorkerRow(ctx, "ha-coder-1")
	require.NoError(t, err)
	assert.Equal(t, store.StatusRetired, row.Status)
	assert.Zero(t, row.Generation, "still at generation 0")
}

// TestFinishSpawnWritesOnce (B4): FinishSpawn turns a running record done
// with its answer, once. A refusal stores its code and message, ok its
// result as JSON (bound as text: the column is STRICT TEXT, N1). A second
// finish is ErrSpawnKeyChanged and keeps the first answer; a finish of a
// record no longer there is ErrNoSpawnKey. A finish timed before the record
// began is stored at its begin (P3), and an answer the schema would refuse
// is ErrInvalid, the record left running.
func TestFinishSpawnWritesOnce(t *testing.T) {
	t.Parallel()
	refused := store.SpawnAnswer{Code: "pane_changed", Message: "w14:p1 changed"}
	ok := store.SpawnAnswer{Code: "ok", Result: []byte(`{"project":"agentisan","workers":[{"id":"ha-coder-1"}]}`)}
	for name, tc := range map[string]struct {
		before func(t *testing.T, s *store.Store)
		answer store.SpawnAnswer
		at     time.Time
		want   error
		// stored is the answer the record holds after, and finished when.
		stored   store.SpawnAnswer
		finished time.Time
	}{
		"a refusal":                {answer: refused, at: spawnAt.Add(time.Minute), stored: refused, finished: spawnAt.Add(time.Minute)},
		"ok":                       {answer: ok, at: spawnAt.Add(time.Minute), stored: ok, finished: spawnAt.Add(time.Minute)},
		"P3: 1 ns before it began": {answer: refused, at: spawnAt.Add(-time.Nanosecond), stored: refused, finished: spawnAt},
		"a second finish": {before: func(t *testing.T, s *store.Store) {
			require.NoError(t, s.FinishSpawn(t.Context(), retryKey, ok, spawnAt.Add(time.Second)))
		}, answer: refused, at: spawnAt.Add(time.Minute), want: store.ErrSpawnKeyChanged, stored: ok, finished: spawnAt.Add(time.Second)},
		"a record dropped since": {before: func(t *testing.T, s *store.Store) {
			dropped, err := s.DropSpawn(t.Context(), retryKey)
			require.NoError(t, err)
			require.True(t, dropped)
		}, answer: refused, at: spawnAt.Add(time.Minute), want: store.ErrNoSpawnKey},
		"a code off the grammar":  {answer: store.SpawnAnswer{Code: "Pane_changed", Message: "m"}, at: spawnAt, want: store.ErrInvalid},
		"an empty code":           {answer: store.SpawnAnswer{Message: "m"}, at: spawnAt, want: store.ErrInvalid},
		"a code of 33 characters": {answer: store.SpawnAnswer{Code: strings.Repeat("a", 33), Message: "m"}, at: spawnAt, want: store.ErrInvalid},
		"a code of 32 characters": {
			answer: store.SpawnAnswer{Code: strings.Repeat("a", 32), Message: "m"}, at: spawnAt,
			stored: store.SpawnAnswer{Code: strings.Repeat("a", 32), Message: "m"}, finished: spawnAt,
		},
		"a refusal with no message": {answer: store.SpawnAnswer{Code: "spawn_failed"}, at: spawnAt, want: store.ErrInvalid},
		"a refusal with a result":   {answer: store.SpawnAnswer{Code: "spawn_failed", Message: "m", Result: []byte(`{}`)}, at: spawnAt, want: store.ErrInvalid},
		"ok with a message":         {answer: store.SpawnAnswer{Code: "ok", Message: "m", Result: []byte(`{}`)}, at: spawnAt, want: store.ErrInvalid},
		"ok with no result":         {answer: store.SpawnAnswer{Code: "ok"}, at: spawnAt, want: store.ErrInvalid},
		"ok with a result not JSON": {answer: store.SpawnAnswer{Code: "ok", Result: []byte(`{`)}, at: spawnAt, want: store.ErrInvalid},
		"ok with a result of CRLF":  {answer: store.SpawnAnswer{Code: "ok", Result: []byte("{\r\n}")}, at: spawnAt, stored: store.SpawnAnswer{Code: "ok", Result: []byte("{\r\n}")}, finished: spawnAt},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			begin(t, s, retryKey, spawnAt, "ha-coder-1")
			if tc.before != nil {
				tc.before(t, s)
			}
			err := s.FinishSpawn(t.Context(), retryKey, tc.answer, tc.at)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
			rec, err := s.SpawnRecord(t.Context(), retryKey)
			if tc.want == store.ErrNoSpawnKey {
				require.ErrorIs(t, err, store.ErrNoSpawnKey)
				return
			}
			require.NoError(t, err)
			if tc.stored.Code == "" {
				assert.Equal(t, "running", rec.State, "nothing was written")
				assert.Zero(t, rec.Answer)
				assert.Zero(t, rec.FinishedAt)
				return
			}
			assert.Equal(t, "done", rec.State)
			assert.Equal(t, tc.stored, rec.Answer)
			assert.Equal(t, tc.finished, rec.FinishedAt)
		})
	}
}

// TestSpawnAnswerBounds (B5): a result over 64 KiB is ErrTooLarge, JSON or
// not, and leaves the record running; one of exactly 64 KiB is stored. A
// message over 16 KiB is stored cut to 16 KiB at a decoded rune boundary,
// one of exactly 16 KiB whole.
func TestSpawnAnswerBounds(t *testing.T) {
	t.Parallel()
	result := func(n int) []byte { return []byte(`"` + strings.Repeat("a", n-2) + `"`) }
	euro := "€" // three bytes
	for name, tc := range map[string]struct {
		answer store.SpawnAnswer
		want   error
		stored string
	}{
		"a 70 KiB result":        {answer: store.SpawnAnswer{Code: "ok", Result: result(70 << 10)}, want: store.ErrTooLarge},
		"a result of 64 KiB + 1": {answer: store.SpawnAnswer{Code: "ok", Result: result(64<<10 + 1)}, want: store.ErrTooLarge},
		"a result of 64 KiB":     {answer: store.SpawnAnswer{Code: "ok", Result: result(64 << 10)}},
		"a 20 KiB message": {
			answer: store.SpawnAnswer{Code: "spawn_failed", Message: strings.Repeat("é", 10<<10)},
			stored: strings.Repeat("é", 8<<10),
		},
		"a message of 16 KiB": {
			answer: store.SpawnAnswer{Code: "spawn_failed", Message: strings.Repeat("m", 16<<10)},
			stored: strings.Repeat("m", 16<<10),
		},
		"a rune across 16 KiB": {
			answer: store.SpawnAnswer{Code: "spawn_failed", Message: strings.Repeat("m", 16<<10-1) + euro + "tail"},
			stored: strings.Repeat("m", 16<<10-1),
		},
		"a message of any bytes": {
			answer: store.SpawnAnswer{Code: "spawn_failed", Message: "\ufeffa\x00b\xffc\r\nd\re é\xe2\x82"},
			stored: "\ufeffa\x00b\xffc\r\nd\re é\xe2\x82",
		},
		// A byte not UTF-8 is one rune wide, so the cut keeps the 🙂 before
		// the two stray bytes whole and one of them.
		"a rune before bytes not UTF-8 across 16 KiB": {
			answer: store.SpawnAnswer{Code: "spawn_failed", Message: strings.Repeat("m", 16<<10-5) + "🙂\x80\x80"},
			stored: strings.Repeat("m", 16<<10-5) + "🙂\x80",
		},
		"a result over 64 KiB, not JSON": {answer: store.SpawnAnswer{Code: "ok", Result: []byte(strings.Repeat("{", 64<<10+1))}, want: store.ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			begin(t, s, retryKey, spawnAt, "ha-coder-1")
			err := s.FinishSpawn(t.Context(), retryKey, tc.answer, spawnAt)
			rec, rerr := s.SpawnRecord(t.Context(), retryKey)
			require.NoError(t, rerr)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				assert.Equal(t, "running", rec.State)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "done", rec.State)
			if tc.answer.Code == "ok" {
				assert.Equal(t, tc.answer.Result, rec.Answer.Result)
				return
			}
			assert.Equal(t, tc.stored, rec.Answer.Message)
			assert.LessOrEqual(t, len(rec.Answer.Message), 16<<10)
		})
	}
}

// TestPruneSpawnKeys (B6): the prune deletes a record created before its
// retention's cutoff, running or done, with its planned rows, and keeps
// one created at the cutoff. The workers rows stay. A running record with a
// bound row still starting at its linked generation is kept, and pruned
// once that row is active, whichever of its planned ids it is; a starting
// row of a later generation keeps nothing. A prune with nothing to delete
// is no error.
func TestPruneSpawnKeys(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	now := spawnAt.Add(20 * 24 * time.Hour)
	cutoff := now.Add(-store.SpawnKeyRetention)
	key := func(c string) string { return strings.Repeat(c, 32) }
	begin(t, s, key("a"), cutoff.Add(-time.Nanosecond), "ha-coder-1")         // running, past
	begin(t, s, key("b"), cutoff, "ha-coder-2")                               // running, at the cutoff
	begin(t, s, key("c"), cutoff.Add(-time.Hour), "ha-coder-3", "ha-coder-4") // done, past, one bound
	begin(t, s, key("d"), cutoff.Add(-time.Hour), "ha-coder-5")               // running, past, its row starting
	begin(t, s, key("e"), cutoff.Add(-time.Hour), "ha-coder-6")               // running, past, rebound since
	begin(t, s, key("f"), cutoff.Add(-time.Hour), "ha-coder-7", "ha-coder-8") // running, past, only its second row starting
	_, err := s.BindSpawned(ctx, fullWorker("ha-coder-3"), key("c"), cutoff)
	require.NoError(t, err)
	require.NoError(t, s.FinishSpawn(ctx, key("c"), store.SpawnAnswer{Code: "spawn_failed", Message: "m"}, cutoff))
	d := fullWorker("ha-coder-5")
	d.Generation, err = s.BindSpawned(ctx, d, key("d"), cutoff)
	require.NoError(t, err)
	e := fullWorker("ha-coder-6")
	e.Generation, err = s.BindSpawned(ctx, e, key("e"), cutoff)
	require.NoError(t, err)
	require.NoError(t, s.SetWorkerStatus(ctx, e, store.StatusRetired, cutoff))
	_, err = s.BindWorker(ctx, fullWorker("ha-coder-6"), cutoff)
	require.NoError(t, err)
	f := fullWorker("ha-coder-8")
	f.Generation, err = s.BindSpawned(ctx, f, key("f"), cutoff)
	require.NoError(t, err)

	n, err := s.PruneSpawnKeys(ctx, now)
	require.NoError(t, err)
	assert.EqualValues(t, 3, n, "a, c and e")
	for c, kept := range map[string]bool{"a": false, "b": true, "c": false, "d": true, "e": false, "f": true} {
		_, err := s.SpawnRecord(ctx, key(c))
		if kept {
			assert.NoError(t, err, c)
		} else {
			assert.ErrorIs(t, err, store.ErrNoSpawnKey, c)
		}
	}
	assert.Equal(t, [2]int{3, 4}, spawnRows(t, s), "the pruned records' planned rows went with them")
	assert.False(t, linkOf(t, s, key("f"), "ha-coder-7").Bound, "f's first row, never bound, kept")
	for _, id := range []string{"ha-coder-3", "ha-coder-5", "ha-coder-6"} {
		_, err := s.WorkerRow(ctx, id)
		assert.NoError(t, err, "%s's workers row stays", id)
	}

	require.NoError(t, s.SetWorkerStatus(ctx, d, store.StatusActive, now))
	n, err = s.PruneSpawnKeys(ctx, now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "d, once its row is active")
	_, err = s.SpawnRecord(ctx, key("d"))
	require.ErrorIs(t, err, store.ErrNoSpawnKey)
	_, err = s.SpawnRecord(ctx, key("b"))
	require.NoError(t, err)

	require.NoError(t, s.SetWorkerStatus(ctx, f, store.StatusActive, now))
	n, err = s.PruneSpawnKeys(ctx, now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "f, once its second row is active")
	n, err = s.PruneSpawnKeys(ctx, now)
	require.NoError(t, err, "a prune with nothing to delete")
	assert.Zero(t, n)
}

// TestSpawnKeyRetentionIsNoLongerThanMessages (PR A's N4): a retry key is
// kept 14 days from its first use, no longer than the finished messages its
// briefs count. A record kept past it for a starting worker can outlive a
// brief (SpawnKeyRetention's comment).
func TestSpawnKeyRetentionIsNoLongerThanMessages(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 14*24*time.Hour, store.SpawnKeyRetention)
	assert.LessOrEqual(t, store.SpawnKeyRetention, store.MessageRetention)
}

// TestKeyedBindErrorsCarryNoRequestValue (revision 5 section 11: no error
// carries the request's text): a keyed bind's error names its sentinel and
// the operation, never the worker's project, group or ids, and errors.Is
// still finds its cause.
func TestKeyedBindErrorsCarryNoRequestValue(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	w := fullWorker("ha-coder-1")
	w.Project, w.Group, w.PaneID = "marker-project", "marker-group", "w14:marker-pane"
	require.NoError(t, s.BeginSpawn(ctx, spawnKey(func(k *store.SpawnKey) { k.Count, k.Project, k.Group = 1, w.Project, w.Group }), []string{w.LogicalID}, spawnAt))
	_, err := s.BindWorker(ctx, w, spawnAt)
	require.NoError(t, err)
	_, err = s.BindSpawned(ctx, w, retryKey, spawnAt)
	require.ErrorIs(t, err, store.ErrWorkerLive, "a live id")
	assert.NotContains(t, err.Error(), "marker")
	assert.NotContains(t, err.Error(), w.LogicalID)

	bad := w
	bad.Provider, bad.TerminalID = "marker-provider", "marker\x00terminal"
	_, err = s.BindSpawned(ctx, bad, retryKey, spawnAt)
	require.ErrorIs(t, err, store.ErrInvalid, "an invalid worker")
	assert.NotContains(t, err.Error(), "marker")
	_, err = s.BindWorker(ctx, bad, spawnAt)
	require.ErrorIs(t, err, store.ErrInvalid)
	assert.Contains(t, err.Error(), "marker", "the keyless bind's diagnostics stay as they were")
}

// TestSpawnWritesRefuseWhatTheyCannotRecord: BindSpawned and FinishSpawn
// refuse a key off the grammar, BindSpawned a worker BindWorker would
// refuse, and all three writes a time nanoseconds since 1970 cannot hold,
// each with ErrInvalid and nothing written.
func TestSpawnWritesRefuseWhatTheyCannotRecord(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	begin(t, s, retryKey, spawnAt, "ha-coder-1")
	for name, key := range map[string]string{
		"empty": "", "31 characters": retryKey[:31], "33 characters": retryKey + "0", "upper case": strings.ToUpper(retryKey),
		"a NUL": retryKey[:31] + "\x00", "invalid UTF-8": retryKey[:31] + "\xff", "non-ASCII": retryKey[:30] + "é",
		"a line break": retryKey[:31] + "\n", "a carriage return": retryKey[:31] + "\r", "a leading dash": "-" + retryKey[1:],
		"a cut sequence": retryKey[:31] + "\xc3", "a byte-order mark": "\ufeff" + retryKey[3:], "Latin-1": retryKey[:31] + "\xe9",
		"a surrogate's bytes": retryKey[:29] + "\xed\xa0\x80", "mixed": retryKey[:29] + "é\xff",
	} {
		_, err := s.BindSpawned(ctx, fullWorker("ha-coder-1"), key, spawnAt)
		require.ErrorIs(t, err, store.ErrInvalid, "bind: %s", name)
		require.ErrorIs(t, s.FinishSpawn(ctx, key, store.SpawnAnswer{Code: "ok", Result: []byte(`{}`)}, spawnAt), store.ErrInvalid, "finish: %s", name)
	}
	for name, change := range map[string]func(*store.Worker){
		"no pane":                       func(w *store.Worker) { w.PaneID = "" },
		"no terminal":                   func(w *store.Worker) { w.TerminalID = "" },
		"another provider":              func(w *store.Worker) { w.Provider = "gemini" },
		"a routing time":                func(w *store.Worker) { w.Routing.RoutedAt = time.Time{} },
		"a pane of invalid UTF-8":       func(w *store.Worker) { w.PaneID = "w14:\xff" },
		"a pane with a byte-order mark": func(w *store.Worker) { w.PaneID = "\ufeffw14:p1" },
		"a terminal with a NUL":         func(w *store.Worker) { w.TerminalID = "term\x00" },
		"a terminal with a line break":  func(w *store.Worker) { w.TerminalID = "term\n" },
	} {
		w := fullWorker("ha-coder-1")
		change(&w)
		_, err := s.BindSpawned(ctx, w, retryKey, spawnAt)
		require.ErrorIs(t, err, store.ErrInvalid, name)
	}
	{ // the worker is checked before the connection, as BindWorker's is
		done, cancel := context.WithCancel(ctx)
		cancel()
		w := fullWorker("ha-coder-1")
		w.Provider = "gemini"
		_, err := s.BindSpawned(done, w, retryKey, spawnAt)
		require.ErrorIs(t, err, store.ErrInvalid, "an invalid provider, the context done")
		require.NotErrorIs(t, err, store.ErrStore)
	}
	{ // the answer is checked before the connection: the schema's CHECKs,
		// which refuse these too as ErrInvalid, are never reached
		done, cancel := context.WithCancel(ctx)
		cancel()
		for name, a := range map[string]store.SpawnAnswer{
			"no code":                   {Message: "m"},
			"a code past 32 bytes":      {Code: strings.Repeat("a", 33), Message: "m"},
			"a code in upper case":      {Code: "Spawn_failed", Message: "m"},
			"a refusal with no message": {Code: "spawn_failed"},
			"a result not JSON":         {Code: "ok", Result: []byte(`{`)},
		} {
			err := s.FinishSpawn(done, retryKey, a, spawnAt)
			require.ErrorIs(t, err, store.ErrInvalid, "%s, the context done", name)
			require.NotErrorIs(t, err, store.ErrStore, name)
		}
	}
	for name, code := range map[string]string{
		"a NUL": "spawn\x00failed", "invalid UTF-8": "spawn\xfffailed", "non-ASCII": "spawn_échec", "a dash": "spawn-failed",
		"a space": "spawn failed", "a leading dash": "-spawn_failed", "a digit": "spawn_failed_2", "a line break": "spawn_failed\n",
	} {
		require.ErrorIs(t, s.FinishSpawn(ctx, retryKey, store.SpawnAnswer{Code: code, Message: "m"}, spawnAt), store.ErrInvalid, "a code with %s", name)
	}
	for name, result := range map[string]string{
		"invalid UTF-8": "\"\xff\"", "a raw NUL": "\"a\x00b\"", "a byte-order mark": "\ufeff{}", "a cut sequence": "\"\xc3\"", "Latin-1": "\"caf\xe9\"",
	} {
		require.ErrorIs(t, s.FinishSpawn(ctx, retryKey, store.SpawnAnswer{Code: "ok", Result: []byte(result)}, spawnAt), store.ErrInvalid, "a result with %s", name)
	}
	deep := []byte(strings.Repeat("[", 2000) + strings.Repeat("]", 2000))
	require.ErrorIs(t, s.FinishSpawn(ctx, retryKey, store.SpawnAnswer{Code: "ok", Result: deep}, spawnAt), store.ErrInvalid, "a result nested past what SQLite's json_valid reads")
	far := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := s.BindSpawned(ctx, fullWorker("ha-coder-1"), retryKey, far)
	require.ErrorIs(t, err, store.ErrInvalid, "a bind time")
	require.ErrorIs(t, s.FinishSpawn(ctx, retryKey, store.SpawnAnswer{Code: "ok", Result: []byte(`{}`)}, far), store.ErrInvalid, "a finish time")
	_, err = s.PruneSpawnKeys(ctx, far)
	require.ErrorIs(t, err, store.ErrInvalid, "a prune time")

	rec, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	assert.Equal(t, "running", rec.State)
	assert.Equal(t, []store.SpawnLink{{LogicalID: "ha-coder-1"}}, rec.Planned)
	noRow(t, s, "ha-coder-1")

	other := strings.Repeat("e", 32)
	begin(t, s, other, spawnAt, "ha-cöder-1")
	_, err = s.BindSpawned(ctx, fullWorker("ha-cöder-1"), other, spawnAt)
	require.NoError(t, err, "a non-ASCII id is bound as planned, byte for byte")
	assert.True(t, linkOf(t, s, other, "ha-cöder-1").Bound)
}

// TestSpawnWritesRaceOnce: two binds of one planned id at once bind it
// once, and two finishes of one record at once store one answer; the other
// of each is ErrSpawnKeyChanged. Two prunes at once both succeed and delete
// an old record once.
func TestSpawnWritesRaceOnce(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	begin(t, s, retryKey, spawnAt, "ha-coder-1")
	race := func(write func(i int) error) []error {
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() { errs[i] = write(i) })
		}
		wg.Wait()
		return errs
	}
	once := func(errs []error) int {
		won := -1
		for i, err := range errs {
			if err == nil {
				require.Equal(t, -1, won, "only one of them wrote")
				won = i
				continue
			}
			require.ErrorIs(t, err, store.ErrSpawnKeyChanged)
		}
		require.NotEqual(t, -1, won, "one of them wrote")
		return won
	}
	once(race(func(int) error {
		_, err := s.BindSpawned(ctx, fullWorker("ha-coder-1"), retryKey, spawnAt)
		return err
	}))
	row, err := s.WorkerRow(ctx, "ha-coder-1")
	require.NoError(t, err)
	assert.Zero(t, row.Generation, "bound once")

	answers := []store.SpawnAnswer{{Code: "spawn_failed", Message: "first"}, {Code: "spawn_failed", Message: "second"}}
	won := once(race(func(i int) error { return s.FinishSpawn(ctx, retryKey, answers[i], spawnAt) }))
	rec, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	assert.Equal(t, answers[won], rec.Answer, "the winner's answer is kept")

	old := strings.Repeat("0", 32)
	begin(t, s, old, spawnAt.Add(-15*24*time.Hour), "ha-coder-2")
	pruned := make([]int64, 2)
	errs := race(func(i int) error {
		var err error
		pruned[i], err = s.PruneSpawnKeys(ctx, spawnAt)
		return err
	})
	assert.Equal(t, []error{nil, nil}, errs, "both prunes succeed")
	assert.ElementsMatch(t, []int64{0, 1}, pruned, "two prunes at once delete the old record once")
}

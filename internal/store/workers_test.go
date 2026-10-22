package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestWorkersReadsTheActiveBindings: Workers is what the team model joins on
// (D5): every worker not retired, lost ones included, whatever its status,
// with its binding (A35), ordered by logical id so two reads of the same rows
// agree. The workforce commands (NERD-5254) write the rows, so this test
// writes them the way they will, through SQL.
func TestWorkersReadsTheActiveBindings(t *testing.T) {
	t.Parallel()

	const insert = "INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, retired_at, terminal_id, shell_pid, shell_start) VALUES "
	tests := map[string]struct {
		rows []string
		// handoff begins one for this logical id.
		handoff string
		want    []store.Worker
	}{
		"no rows": {want: []store.Worker{}},
		"active rows, ordered by logical id whatever the insert order": {
			rows: []string{
				"('qa-1', 'agentisan', 'qa', 'wY:p1', 0, 'active', 1, NULL, 'term_9', NULL, NULL)",
				"('coders-2', 'agentisan', 'coders', 'wN:p3', 4, 'active', 1, NULL, 'term_3', 4242, 9223372036854775807)",
				"('coders-1', 'agentisan', 'coders', 'wN:p2', 1, 'starting', 1, NULL, 'term_2', NULL, NULL)",
			},
			want: []store.Worker{
				{LogicalID: "coders-1", Project: "agentisan", Group: "coders", PaneID: "wN:p2", Generation: 1, TerminalID: "term_2", Status: "starting"},
				{LogicalID: "coders-2", Project: "agentisan", Group: "coders", PaneID: "wN:p3", Generation: 4, TerminalID: "term_3", ShellPID: 4242, ShellStart: 1<<63 - 1, Status: "active"},
				{LogicalID: "qa-1", Project: "agentisan", Group: "qa", PaneID: "wY:p1", TerminalID: "term_9", Status: "active"},
			},
		},
		"a retired worker is not bound to its last pane": {
			rows: []string{
				"('coders-1', 'agentisan', 'coders', 'wN:p2', 1, 'retired', 1, 2, 'term_2', NULL, NULL)",
				"('coders-2', 'agentisan', 'coders', 'wN:p3', 0, 'active', 1, NULL, 'term_3', NULL, NULL)",
			},
			want: []store.Worker{{LogicalID: "coders-2", Project: "agentisan", Group: "coders", PaneID: "wN:p3", TerminalID: "term_3", Status: "active"}},
		},
		"a lost worker is still live, and one from before A35 has no terminal": {
			rows: []string{
				"('coders-1', 'agentisan', 'coders', 'wN:p2', 1, 'lost', 1, NULL, 'term_2', 7, 8)",
				"('coders-2', 'agentisan', 'coders', 'wN:p3', 0, 'active', 1, NULL, '', NULL, NULL)",
			},
			want: []store.Worker{
				{LogicalID: "coders-1", Project: "agentisan", Group: "coders", PaneID: "wN:p2", Generation: 1, TerminalID: "term_2", ShellPID: 7, ShellStart: 8, Status: "lost"},
				{LogicalID: "coders-2", Project: "agentisan", Group: "coders", PaneID: "wN:p3", Status: "active"},
			},
		},
		"a worker with a handoff pending": {
			rows:    []string{"('coders-1', 'agentisan', 'coders', 'wN:p2', 0, 'active', 1, NULL, 'term_2', NULL, NULL)"},
			handoff: "coders-1",
			want:    []store.Worker{{LogicalID: "coders-1", Project: "agentisan", Group: "coders", PaneID: "wN:p2", TerminalID: "term_2", Status: "active", HandoffPending: true}},
		},
		// The schema does not stop two workers naming one pane; Workers
		// reports both, in order, and the model decides (team.Build).
		"two active workers on one pane": {
			rows: []string{
				"('b', '', 'coders', 'wN:p2', 0, 'active', 1, NULL, 'term_2', NULL, NULL)",
				"('a', '', 'coders', 'wN:p2', 0, 'active', 1, NULL, 'term_2', NULL, NULL)",
			},
			want: []store.Worker{
				{LogicalID: "a", Group: "coders", PaneID: "wN:p2", TerminalID: "term_2", Status: "active"},
				{LogicalID: "b", Group: "coders", PaneID: "wN:p2", TerminalID: "term_2", Status: "active"},
			},
		},
		"non-ASCII ids and names are read back byte for byte": {
			rows: []string{"('cödér 1', 'prøject', '◆ grp', 'w1:p1', 0, 'active', 1, NULL, 'térm', NULL, NULL)"},
			want: []store.Worker{{LogicalID: "cödér 1", Project: "prøject", Group: "◆ grp", PaneID: "w1:p1", TerminalID: "térm", Status: "active"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			for _, row := range tc.rows {
				_, err := store.DB(s).ExecContext(t.Context(), insert+row)
				require.NoError(t, err)
			}
			if tc.handoff != "" {
				_, err := s.BeginHandoff(t.Context(), tc.handoff, "w9:p9", "term_new", t1)
				require.NoError(t, err)
			}

			got, err := s.Workers(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestWorkersOnAClosedStore fails the way every other read does.
func TestWorkersOnAClosedStore(t *testing.T) {
	t.Parallel()
	s, err := store.Open(t.Context(), dbPath(t))
	require.NoError(t, err)
	require.NoError(t, s.Close())

	_, err = s.Workers(context.Background())
	require.ErrorIs(t, err, store.ErrStore)
}

// liveRow reads id's live row, as the team poll does.
func liveRow(t *testing.T, s *store.Store, id string) store.Worker {
	t.Helper()
	ws, err := s.Workers(t.Context())
	require.NoError(t, err)
	for _, w := range ws {
		if w.LogicalID == id {
			return w
		}
	}
	t.Fatalf("%s is not live", id)
	return store.Worker{}
}

// TestThePollsWritesAreCompareAndSet is A35's writer race: each of the team
// poll's writes lands only on the row as the poll read it, at its
// generation and terminal, live and not lost. A row the workforce commands
// changed meanwhile is left as they left it, ErrWorkerChanged, and lost is
// never written over.
func TestThePollsWritesAreCompareAndSet(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 3)
	read := liveRow(t, s, "w")

	require.NoError(t, s.RebindWorker(ctx, read, "w20:p1"), "a move keeps the terminal")
	require.NoError(t, s.SetWorkerShell(ctx, read, 4242, 77))
	got := liveRow(t, s, "w")
	assert.Equal(t, store.Worker{
		LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w20:p1", Generation: 3,
		TerminalID: "term_w14:p2", ShellPID: 4242, ShellStart: 77, Status: "active",
	}, got)

	require.NoError(t, s.RekeyWorker(ctx, got, "w20:p1", "term_after"))
	got = liveRow(t, s, "w")
	assert.Equal(t, "term_after", got.TerminalID)
	assert.Equal(t, uint32(4242), got.ShellPID, "a re-key keeps the shell it proved")
	// Each writer is refused on the terminal before the re-key alone: read
	// has the row's generation, and otherwise differs from it only in
	// columns no write compares (its pane and shell).
	for name, err := range map[string]error{
		"rebind": s.RebindWorker(ctx, read, "w21:p1"),
		"shell":  s.SetWorkerShell(ctx, read, 1, 1),
		"re-key": s.RekeyWorker(ctx, read, "w21:p1", "term_y"),
		"lost":   s.MarkWorkerLost(ctx, read),
	} {
		require.ErrorIs(t, err, store.ErrWorkerChanged, "planned on the terminal before: %s", name)
	}

	// And each on another generation alone, at the row's terminal.
	stale := got
	stale.Generation = 2
	for name, err := range map[string]error{
		"rebind": s.RebindWorker(ctx, stale, "w21:p1"),
		"shell":  s.SetWorkerShell(ctx, stale, 1, 1),
		"re-key": s.RekeyWorker(ctx, stale, "w21:p1", "term_y"),
		"lost":   s.MarkWorkerLost(ctx, stale),
	} {
		require.ErrorIs(t, err, store.ErrWorkerChanged, "another generation: %s", name)
	}
	assert.Equal(t, got, liveRow(t, s, "w"), "no refused write changed anything")

	for name, err := range map[string]error{
		"a pid of 0":       s.SetWorkerShell(ctx, got, 0, 1),
		"no pane":          s.RebindWorker(ctx, got, ""),
		"no terminal":      s.RekeyWorker(ctx, got, "w20:p1", ""),
		"a control byte":   s.RekeyWorker(ctx, got, "w20:p1", "term\x00"),
		"invalid UTF-8":    s.RekeyWorker(ctx, got, "w20:p1", "term\xff"),
		"a pane to re-key": s.RekeyWorker(ctx, got, "", "term_x"),
	} {
		require.ErrorIs(t, err, store.ErrInvalid, name)
	}
	require.ErrorIs(t, s.SetWorkerShell(ctx, got, 1, 1<<63), store.ErrStore, "a start past SQLite's integers")

	require.NoError(t, s.MarkWorkerLost(ctx, got))
	lost := liveRow(t, s, "w")
	assert.Equal(t, store.StatusLost, lost.Status, "lost is not retired: still live, generation and all")
	assert.Equal(t, int64(3), lost.Generation)
	for name, err := range map[string]error{
		"rebind": s.RebindWorker(ctx, lost, "w21:p1"),
		"shell":  s.SetWorkerShell(ctx, lost, 5, 5),
		"re-key": s.RekeyWorker(ctx, lost, "w21:p1", "term_x"),
		"lost":   s.MarkWorkerLost(ctx, lost),
	} {
		require.ErrorIs(t, err, store.ErrWorkerChanged, "a lost row is the boss's to retire or reuse: %s", name)
	}
	_, err := s.BeginHandoff(ctx, "w", "w21:p1", "term_x", t1)
	require.ErrorIs(t, err, store.ErrNotBound)
	assert.Equal(t, lost, liveRow(t, s, "w"))

	retire(t, s, "w")
	require.ErrorIs(t, s.RebindWorker(ctx, got, "w21:p1"), store.ErrWorkerChanged, "a retired row")

	// And each on a row retired, not lost, since it was read: retired
	// alone.
	seedWorker(t, s, "v", "w14:p5", 3)
	v := liveRow(t, s, "v")
	retire(t, s, "v")
	for name, err := range map[string]error{
		"rebind": s.RebindWorker(ctx, v, "w21:p1"),
		"shell":  s.SetWorkerShell(ctx, v, 1, 1),
		"re-key": s.RekeyWorker(ctx, v, "w21:p1", "term_y"),
		"lost":   s.MarkWorkerLost(ctx, v),
	} {
		require.ErrorIs(t, err, store.ErrWorkerChanged, "retired since it was read: %s", name)
	}
}

// TestAPendingHandoffIsNeverLost: while a handoff of the worker is pending,
// the poll may not mark it lost, since the handoff's new pane is its next
// binding; once the handoff fails, it may.
func TestAPendingHandoffIsNeverLost(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 0)
	h, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	read := liveRow(t, s, "w")
	require.True(t, read.HandoffPending)

	require.ErrorIs(t, s.MarkWorkerLost(ctx, read), store.ErrWorkerChanged)
	require.NoError(t, s.RebindWorker(ctx, read, "w20:p2"), "a move is still followed")
	require.NoError(t, s.FailHandoff(ctx, h, t1))
	require.NoError(t, s.MarkWorkerLost(ctx, liveRow(t, s, "w")))
	assert.Equal(t, store.StatusLost, liveRow(t, s, "w").Status)
}

// TestCompleteHandoffBindsTheNewTerminal: completing binds the handoff's
// generation to the new pane and its terminal, whatever pane hint the poll
// left meanwhile, and clears the shell, which is the replacement's to read.
// The status is left alone. A worker retired and reused meanwhile is at
// another generation, which the handoff never completes onto.
func TestCompleteHandoffBindsTheNewTerminal(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 5)
	require.NoError(t, s.SetWorkerShell(ctx, liveRow(t, s, "w"), 4242, 77))
	h, err := s.BeginHandoff(ctx, "w", "w14:p7", "term_next", t1)
	require.NoError(t, err)
	require.NoError(t, s.RebindWorker(ctx, liveRow(t, s, "w"), "w20:p2"), "the old pane moved mid-handoff")

	require.NoError(t, s.CompleteHandoff(ctx, h, t1))
	assert.Equal(t, store.Worker{
		LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w14:p7", Generation: 5,
		TerminalID: "term_next", Status: "active",
	}, liveRow(t, s, "w"))

	h, err = s.BeginHandoff(ctx, "w", "w14:p8", "term_third", t1)
	require.NoError(t, err)
	_, err = s.BeginHandoff(ctx, "w", "w14:p9", "term_next", t1)
	require.ErrorIs(t, err, store.ErrHandoffPending)
	retire(t, s, "w")
	reuse(t, s, "w")
	require.ErrorIs(t, s.CompleteHandoff(ctx, h, t1), store.ErrNotBound, "the id was reused at generation 6")
	assert.Equal(t, "term_next", liveRow(t, s, "w").TerminalID)
}

// TestBindWorkerLifecycle: a new logical id is bound at generation 0 and
// starting, with its terminal; it goes active, then retires; the id reused
// binds the same row to the new pane and terminal at the next generation,
// with its retirement cleared (I1). A live id is never rebound, and Workers
// only ever sees live rows.
func TestBindWorkerLifecycle(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	t0 := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	w := store.Worker{LogicalID: "ha-coder-2", Project: "herdr-agentisan", Group: "coder", PaneID: "w14:p7", TerminalID: "term_a"}

	gen, err := s.BindWorker(ctx, w, t0)
	require.NoError(t, err)
	assert.Zero(t, gen)
	w.Status = store.StatusStarting
	row, err := s.WorkerRow(ctx, "ha-coder-2")
	require.NoError(t, err)
	assert.Equal(t, store.Row{Worker: w, CreatedAt: t0}, row.InUTC())

	require.NoError(t, s.SetWorkerStatus(ctx, w, store.StatusActive, t0.Add(time.Second)))
	_, err = s.BindWorker(ctx, store.Worker{LogicalID: "ha-coder-2", PaneID: "w14:p9", TerminalID: "term_b"}, t0.Add(2*time.Second))
	require.ErrorIs(t, err, store.ErrWorkerLive, "a live id is never rebound")
	w.Status = store.StatusActive
	live, err := s.Workers(ctx)
	require.NoError(t, err)
	assert.Equal(t, []store.Worker{w}, live)

	require.NoError(t, s.SetWorkerStatus(ctx, w, store.StatusRetired, t0.Add(time.Minute)))
	require.ErrorIs(t, s.SetWorkerStatus(ctx, w, store.StatusRetired, t0.Add(2*time.Minute)), store.ErrNoWorker,
		"a retired row stays retired at its first retirement")
	row, err = s.WorkerRow(ctx, "ha-coder-2")
	require.NoError(t, err)
	w.Status = store.StatusRetired
	assert.Equal(t, store.Row{Worker: w, CreatedAt: t0, RetiredAt: t0.Add(time.Minute)}, row.InUTC())
	live, err = s.Workers(ctx)
	require.NoError(t, err)
	assert.Empty(t, live)

	again := store.Worker{LogicalID: "ha-coder-2", Project: "assay", Group: "qa", PaneID: "w14:pA", TerminalID: "term_c"}
	gen, err = s.BindWorker(ctx, again, t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), gen)
	row, err = s.WorkerRow(ctx, "ha-coder-2")
	require.NoError(t, err)
	again.Generation, again.Status = 1, store.StatusStarting
	assert.Equal(t, store.Row{Worker: again, CreatedAt: t0}, row.InUTC(), "the new project and group, the row's first creation")
	require.ErrorIs(t, s.SetWorkerStatus(ctx, w, store.StatusRetired, t0.Add(2*time.Hour)), store.ErrNoWorker,
		"a write planned on generation 0 never ends generation 1")
}

// TestBindWorkerReusesALostID: a lost row is live, so starting and active
// never bring it back, while an ending retires it; BindWorker alone reuses
// its id, at the next generation with the new terminal and the shell cleared
// for the team poll to read (A35).
func TestBindWorkerReusesALostID(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	t0 := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	seedWorker(t, s, "w", "w14:p2", 3)
	require.NoError(t, s.SetWorkerShell(ctx, liveRow(t, s, "w"), 4242, 77))
	require.NoError(t, s.MarkWorkerLost(ctx, liveRow(t, s, "w")))
	lost := liveRow(t, s, "w")
	for _, status := range []string{store.StatusStarting, store.StatusActive} {
		require.ErrorIs(t, s.SetWorkerStatus(ctx, lost, status, t0), store.ErrNoWorker, status)
	}
	assert.Equal(t, lost, liveRow(t, s, "w"))

	gen, err := s.BindWorker(ctx, store.Worker{LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w14:p9", TerminalID: "term_new"}, t0)
	require.NoError(t, err)
	assert.Equal(t, int64(4), gen)
	assert.Equal(t, store.Worker{
		LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w14:p9", Generation: 4,
		TerminalID: "term_new", Status: store.StatusStarting,
	}, liveRow(t, s, "w"))

	require.NoError(t, s.MarkWorkerLost(ctx, liveRow(t, s, "w")))
	require.NoError(t, s.SetWorkerStatus(ctx, liveRow(t, s, "w"), store.StatusRetired, t0), "an ending retires a lost row")
	row, err := s.WorkerRow(ctx, "w")
	require.NoError(t, err)
	assert.Equal(t, store.StatusRetired, row.Status)
}

// TestBindWorkerFailsAStaleHandoff: a handoff left pending on a row retired
// by a path that did not end it fails when the id is bound again, in the same
// transaction, so it neither holds the new generation's inbox nor blocks its
// own handoff, and it never completes. A bind refused because the id is live
// leaves its handoff pending (#29's QA N2, NERD-5261 B3).
func TestBindWorkerFailsAStaleHandoff(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p1", 0)
	done, err := s.BeginHandoff(ctx, "w", "w14:p2", terminal("w14:p2"), t1)
	require.NoError(t, err)
	require.NoError(t, s.CompleteHandoff(ctx, done, t1))
	stale, err := s.BeginHandoff(ctx, "w", "w14:p7", terminal("w14:p7"), t1)
	require.NoError(t, err)
	retire(t, s, "w")

	gen, err := s.BindWorker(ctx, store.Worker{LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w14:p9", TerminalID: "term_new"}, t1.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), gen)
	assert.Equal(t, "failed", handoffState(t, s, stale))
	assert.Equal(t, "acknowledged", handoffState(t, s, done), "a handoff that completed stays completed")
	assert.False(t, liveRow(t, s, "w").HandoffPending)
	// Active, as spawn marks it once its agent is up: a starting inbox waits.
	require.NoError(t, s.SetWorkerStatus(ctx, store.Worker{LogicalID: "w", Generation: gen}, store.StatusActive, t1.Add(time.Hour)))
	send(t, s, worker("w"), boss, "for the new session")
	assert.Equal(t, "for the new session", claim(t, s, worker("w")).Body, "the inbox is the new generation's")
	_, err = s.BeginHandoff(ctx, "w", "w14:pA", "term_next", t1.Add(2*time.Hour))
	require.NoError(t, err, "the new generation can hand off")
	require.ErrorIs(t, s.CompleteHandoff(ctx, stale, t1.Add(2*time.Hour)), store.ErrNoHandoff)

	seedWorker(t, s, "v", "w15:p2", 0)
	held, err := s.BeginHandoff(ctx, "v", "w15:p7", terminal("w15:p7"), t1)
	require.NoError(t, err)
	_, err = s.BindWorker(ctx, store.Worker{LogicalID: "v", PaneID: "w15:p9", TerminalID: "term_other"}, t1.Add(time.Hour))
	require.ErrorIs(t, err, store.ErrWorkerLive)
	assert.Equal(t, "pending", handoffState(t, s, held), "a refused bind changes nothing")
}

// TestSetWorkerStatusEndings: failed and gone retire a row as retired does;
// a retirement dated before the row's creation (a clock stepped back) is
// recorded at the creation, which the schema's CHECK requires.
func TestSetWorkerStatusEndings(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	for _, status := range []string{store.StatusRetired, store.StatusFailed, store.StatusGone} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			w := store.Worker{LogicalID: "x-qa-1", PaneID: "w1:p1", TerminalID: "term_1"}
			_, err := s.BindWorker(t.Context(), w, t0)
			require.NoError(t, err)

			require.NoError(t, s.SetWorkerStatus(t.Context(), w, status, t0.Add(-time.Hour)))
			row, err := s.WorkerRow(t.Context(), "x-qa-1")
			require.NoError(t, err)
			assert.Equal(t, status, row.Status)
			assert.True(t, row.RetiredAt.Equal(t0), "retired at its creation, not before it")
		})
	}
}

// TestWorkerWritesRefuseWhatTheyCannotRecord: an empty id, pane or
// terminal, an unknown status, a time nanoseconds cannot hold, an unknown id
// and a closed store each fail with their own sentinel.
func TestWorkerWritesRefuseWhatTheyCannotRecord(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	t0 := time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)
	w := store.Worker{LogicalID: "x-qa-1", PaneID: "w1:p1", TerminalID: "term_1"}

	for name, bad := range map[string]store.Worker{
		"no id":       {PaneID: "w1:p1", TerminalID: "term_1"},
		"no pane":     {LogicalID: "x-qa-1", TerminalID: "term_1"},
		"no terminal": {LogicalID: "x-qa-1", PaneID: "w1:p1"},
	} {
		_, err := s.BindWorker(ctx, bad, t0)
		require.ErrorIs(t, err, store.ErrInvalid, name)
	}
	_, err := s.BindWorker(ctx, w, time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC))
	require.ErrorIs(t, err, store.ErrInvalid)
	require.ErrorIs(t, s.SetWorkerStatus(ctx, w, "paused", t0), store.ErrInvalid)
	require.ErrorIs(t, s.SetWorkerStatus(ctx, w, store.StatusActive, time.Time{}), store.ErrInvalid)
	require.ErrorIs(t, s.SetWorkerStatus(ctx, w, store.StatusActive, t0), store.ErrNoWorker)
	_, err = s.WorkerRow(ctx, "x-qa-1")
	require.ErrorIs(t, err, store.ErrNoWorker)

	require.NoError(t, s.Close())
	_, err = s.BindWorker(context.Background(), w, t0)
	require.ErrorIs(t, err, store.ErrStore)
	require.ErrorIs(t, s.SetWorkerStatus(context.Background(), w, store.StatusActive, t0), store.ErrStore)
	_, err = s.WorkerRow(context.Background(), "x-qa-1")
	require.ErrorIs(t, err, store.ErrStore)
}

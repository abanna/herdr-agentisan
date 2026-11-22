package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// pending is ha-boss's BoardPending on project ha.
func pending(t *testing.T, s *store.Store) store.BoardPendingSnapshot {
	t.Helper()
	p, err := s.BoardPending(t.Context(), "ha", "ha-boss", notice.CeilingMark)
	require.NoError(t, err)
	return p
}

// daemonPost is a daemon post of kind on project's board at at.
func daemonPost(project, kind, body string, at time.Time) store.BoardPost {
	return store.BoardPost{Project: project, Channel: store.ChannelDaemon, Author: store.Daemon, Kind: kind, Body: body, At: at}
}

// ceilingLine is a ceiling line as ctxText writes one.
const ceilingLine = "ha-coder-1 is at 99% context (ceiling: 75%): act now; the user has been alerted"

// TestBoardPendingCountsPostsAfterTheBossMark (A74): the posts pending are
// those after the boss's mark on its own team's board, the oldest of them
// first, with the mark as it stands.
func TestBoardPendingCountsPostsAfterTheBossMark(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	var seqs []int64
	for i := range 4 {
		p := teamPost("ha", "ha-coder-1", "post")
		p.At = boardAt.Add(time.Duration(i) * time.Minute)
		seqs = append(seqs, post(t, s, p).Seq)
	}
	post(t, s, teamPost("web", "web-coder-1", "another team's post"))
	assert.Equal(t, store.BoardPendingSnapshot{Pending: 4, Oldest: boardAt}, pending(t, s), "mark 0")
	for _, c := range []struct{ read, want int }{{2, 2}, {3, 1}, {4, 0}} {
		mark := seqs[c.read-1]
		_, err := s.MarkSeen(t.Context(), "ha", "boss", mark)
		require.NoError(t, err)
		want := store.BoardPendingSnapshot{Pending: c.want, BossMark: mark}
		if c.want > 0 {
			want.Oldest = boardAt.Add(time.Duration(c.read) * time.Minute)
		}
		assert.Equal(t, want, pending(t, s), "%d read", c.read)
	}
}

// TestBoardPendingFindsACeilingInANoticesPost (M5.2, C1): a ceiling is a
// daemon post of kind ctx_ceiling, or a daemon notices post holding the
// ceiling mark; the oldest and the newest unread are found, and a team post
// or another notices post is no ceiling.
func TestBoardPendingFindsACeilingInANoticesPost(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	post(t, s, daemonPost("ha", "notices", "ha-coder-2 finished a turn", boardAt))
	post(t, s, daemonPost("ha", "notices", "ha-coder-2 finished a turn\n"+ceilingLine, boardAt.Add(time.Minute)))
	p := teamPost("ha", "ha-coder-1", "pasted: "+ceilingLine)
	p.At = boardAt.Add(2 * time.Minute)
	post(t, s, p)
	post(t, s, daemonPost("ha", "ctx_ceiling", "ha-boss is at 90% context", boardAt.Add(3*time.Minute)))
	post(t, s, daemonPost("ha", "turn_ended", "ha-coder-2 finished a turn", boardAt.Add(4*time.Minute)))
	post(t, s, daemonPost("web", "ctx_ceiling", "web-boss is at 90% context", boardAt.Add(5*time.Minute)))
	got := pending(t, s)
	assert.Equal(t, 5, got.Pending)
	assert.Equal(t, boardAt.Add(time.Minute), got.OldestCeiling, "the notices post holding the mark")
	assert.Equal(t, boardAt.Add(3*time.Minute), got.NewestCeiling, "the ctx_ceiling post")
}

// TestBoardPendingIgnoresTheUsersMark (M2): the user reading the board, in
// the dashboard's panel, leaves what the boss has pending as it was.
func TestBoardPendingIgnoresTheUsersMark(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	post(t, s, teamPost("ha", "ha-coder-1", "first"))
	last := post(t, s, daemonPost("ha", "ctx_ceiling", "ha-boss is at 90% context", boardAt.Add(time.Minute)))
	before := pending(t, s)
	_, err := s.MarkSeen(t.Context(), "ha", "user", last.Seq)
	require.NoError(t, err)
	assert.Equal(t, before, pending(t, s))
	assert.Equal(t, 2, before.Pending)
}

// TestBoardPendingCountsHeldMessages (M-B): the messages still queued for
// the boss in its inbox, which the dispatcher holds while its agent is
// unseen, are pending too; a delivered one, or one for another boss, is not.
func TestBoardPendingCountsHeldMessages(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	seedWorker(t, s, "ha-coder-1", "w14:p2", 1)
	seedWorker(t, s, "ha-coder-2", "w14:p3", 1)
	seedWorker(t, s, "web-coder-1", "w15:p2", 1)
	// The delivered one is the oldest, sent and claimed first.
	delivered, err := s.Send(t.Context(), boss, worker("ha-coder-2"), "delivered", t1.Add(-time.Hour))
	require.NoError(t, err)
	m, ok, err := s.Claim(t.Context(), boss, t1)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, delivered.ID, m.ID)
	require.NoError(t, s.Delivered(t.Context(), m.ID, t1))
	_, err = s.Send(t.Context(), boss, worker("ha-coder-1"), "first", t1)
	require.NoError(t, err)
	_, err = s.Send(t.Context(), boss, pane, "second", t1.Add(time.Minute))
	require.NoError(t, err)
	_, err = s.Send(t.Context(), store.Address{Kind: store.KindBoss, ID: "web-boss"}, worker("web-coder-1"), "another boss's", t1.Add(-time.Minute))
	require.NoError(t, err)
	got := pending(t, s)
	assert.Equal(t, 2, got.Held, "held")
	assert.Equal(t, t1, got.OldestHeld, "the oldest held")
	assert.Zero(t, got.Pending, "no post")
}

// TestBoardPendingFindsACeilingInAHeldNotice (M4, C1): a held ceiling is a
// queued daemon notice holding the mark. Its oldest is timed by the row's
// creation, at or before its ceiling line; its newest change by the row's
// last update, which an appended line moves. A pane's message holding the
// same text is no ceiling.
func TestBoardPendingFindsACeilingInAHeldNotice(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	at := func(n int) time.Time { return t1.Add(time.Duration(n) * time.Minute) }
	_, err := s.Send(t.Context(), boss, pane, "pasted: "+ceilingLine, at(1))
	require.NoError(t, err)
	_, appended, err := s.Notify(t.Context(), boss, at(2), func(queued string) (string, bool) {
		return "ha-coder-2 finished a turn", queued == ""
	})
	require.NoError(t, err)
	require.False(t, appended)
	_, appended, err = s.Notify(t.Context(), boss, at(3), func(queued string) (string, bool) {
		return queued + "\n" + ceilingLine, queued != ""
	})
	require.NoError(t, err)
	require.True(t, appended, "the ceiling line is appended to the t2 notice")
	got := pending(t, s)
	assert.Equal(t, at(2), got.OldestHeldCeiling, "the t2 notice, created before its ceiling line")
	assert.Equal(t, 2, got.Held)
	assert.Equal(t, at(1), got.OldestHeld)
	assert.Equal(t, at(3), got.NewestCeiling, "the t3 append, before the t4 notice")
	_, appended, err = s.Notify(t.Context(), boss, at(4), func(queued string) (string, bool) {
		return ceilingLine, queued == ""
	})
	require.NoError(t, err)
	require.False(t, appended, "a new notice at t4")
	got = pending(t, s)
	assert.Equal(t, at(2), got.OldestHeldCeiling)
	assert.Equal(t, 3, got.Held)
	assert.Equal(t, at(1), got.OldestHeld)
	assert.Equal(t, at(4), got.NewestCeiling, "the t4 notice")
}

// TestBoardPendingRefusesAnEmptyCeilingMark: an empty mark would be in every
// body (SQLite's instr finds an empty needle at 1), so every daemon notice
// would read as a ceiling, alerting at ceiling_after; it is refused before
// any read.
func TestBoardPendingRefusesAnEmptyCeilingMark(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	post(t, s, daemonPost("ha", "notices", "ha-coder-2 finished a turn", boardAt))
	_, err := s.BoardPending(t.Context(), "ha", "ha-boss", "")
	require.ErrorIs(t, err, store.ErrInvalid)
	assert.Contains(t, err.Error(), "ceiling mark")
}

// TestBoardPendingTakesCeilingsFromTheDaemonOnly: a ceiling is the
// daemon's. A team post of a notice's kind, or a pane's message holding
// the mark, is none, whatever its text, and moves no ceiling time.
func TestBoardPendingTakesCeilingsFromTheDaemonOnly(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	for i, kind := range []string{"ctx_ceiling", "notices"} {
		p := teamPost("ha", "ha-coder-1", "pasted: "+ceilingLine)
		p.Kind, p.At = kind, boardAt.Add(time.Duration(i)*time.Minute)
		post(t, s, p)
	}
	_, err := s.Send(t.Context(), boss, pane, "pasted: "+ceilingLine, t1)
	require.NoError(t, err)
	got := pending(t, s)
	assert.Equal(t, 2, got.Pending)
	assert.Equal(t, 1, got.Held)
	assert.Zero(t, got.OldestCeiling, "no daemon post")
	assert.Zero(t, got.OldestHeldCeiling, "no daemon notice")
	assert.Zero(t, got.NewestCeiling, "neither")
}

// TestBoardPendingNewestCeilingIsTheLaterRead (C1): NewestCeiling is the
// later of the posts' newest ceiling and the held notices' newest change,
// whichever read gives it.
func TestBoardPendingNewestCeilingIsTheLaterRead(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	at := func(n int) time.Time { return boardAt.Add(time.Duration(n) * time.Minute) }
	_, _, err := s.Notify(t.Context(), boss, at(1), func(string) (string, bool) { return ceilingLine, true })
	require.NoError(t, err)
	post(t, s, daemonPost("ha", "ctx_ceiling", "ha-boss is at 90% context", at(3)))
	assert.Equal(t, at(3), pending(t, s).NewestCeiling, "the post, newer than the held notice")
	_, appended, err := s.Notify(t.Context(), boss, at(5), func(queued string) (string, bool) {
		return queued + "\n" + ceilingLine, queued != ""
	})
	require.NoError(t, err)
	require.True(t, appended)
	assert.Equal(t, at(5), pending(t, s).NewestCeiling, "the held notice's append, newer than the post")
}

// TestBoardPendingIgnoresACeilingQuotedInAFailureNotice (S2, NERD-5533
// 34ed8294): a notice of a failed delivery quotes the message whole, and a
// row returned from an ended binding was another's, a daemon row to a
// worker included; any can hold a ceiling line and is no ceiling, held or
// posted. The held read takes only
// the daemon's own notices, as the posts read takes only the notices kind,
// which rowPosts gives no such row.
func TestBoardPendingIgnoresACeilingQuotedInAFailureNotice(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "ha-coder-1", "w14:p2", 1)
	seedWorker(t, s, "ha-coder-2", "w14:p3", 1)
	failed := func(from store.Address, at time.Time) {
		t.Helper()
		_, err := s.Send(ctx, worker("ha-coder-1"), from, ceilingLine+"\nhand off now", at)
		require.NoError(t, err)
		m, ok, err := s.Claim(ctx, worker("ha-coder-1"), at)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, s.Fail(ctx, m.ID, "prompt timed out", at))
	}
	failed(boss, t1)                                  // its notice is the boss's
	failed(worker("ha-coder-2"), t1.Add(time.Minute)) // ha-coder-2's, then returned to the boss
	_, err := s.Send(ctx, worker("ha-coder-2"), store.Daemon, ceilingLine, t1.Add(time.Minute))
	require.NoError(t, err) // a daemon row, no failure's notice, returned too
	retire(t, s, "ha-coder-2")
	returned, err := s.ReturnToBoss(ctx, "ha-coder-2", "agentisan", boss, t1.Add(2*time.Minute))
	require.NoError(t, err)
	require.Len(t, returned, 2)
	got := pending(t, s)
	assert.Equal(t, 3, got.Held)
	assert.Zero(t, got.OldestHeldCeiling, "held")
	assert.Zero(t, got.NewestCeiling, "held")
	posted, err := s.PostQueued(ctx, boss, "ha", t1.Add(3*time.Minute))
	require.NoError(t, err)
	require.Len(t, posted.Delivered, 3)
	got = pending(t, s)
	assert.Equal(t, 3, got.Pending)
	assert.Zero(t, got.Held)
	assert.Zero(t, got.OldestCeiling, "posted")
	assert.Zero(t, got.NewestCeiling, "posted")
}

// TestBoardPendingHasNoProductionCaller (S3, NERD-5533 34ed8294 and
// ef44f246): BoardPending is dormant until PR 3b's poll calls it (A74), which
// deletes this test. No non-test file in internal/ or cmd/ but board.go uses
// the name, a selector, a method value or an interface's method alike; and in
// board.go, where the type is declared and used, only the method's
// definition is exempt: a selector of it there is a caller too.
func TestBoardPendingHasNoProductionCaller(t *testing.T) {
	t.Parallel()
	const own = "internal/store/board.go"
	uses := map[string][]string{}
	parsed := 0
	for _, dir := range []string{"internal", "cmd"} {
		names, _, _, files := productionUses(t, filepath.Join("..", ".."), dir, []string{"BoardPending"}, nil)
		maps.Copy(uses, names)
		parsed += files
	}
	assert.Greater(t, parsed, 100, "the scan reached the module's packages")
	assert.NotEmpty(t, uses[own], "the scan finds BoardPending in %s", own)
	delete(uses, own)
	assert.Empty(t, uses, "BoardPending is dormant until PR 3b's poll calls it (A74)")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "..", own), nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	queries := []string{"pendingPosts", "pendingHeld"}
	var method *ast.FuncDecl
	selectors, declared := []string{}, map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Recv != nil && n.Name.Name == "BoardPending" {
				method = n
			}
		case *ast.ValueSpec:
			for _, id := range n.Names {
				if slices.Contains(queries, id.Name) {
					declared[id] = true
				}
			}
		case *ast.SelectorExpr:
			if n.Sel.Name == "BoardPending" {
				selectors = append(selectors, fset.Position(n.Sel.Pos()).String())
			}
		}
		return true
	})
	require.NotNil(t, method, "the scan finds the method's definition in %s", own)
	assert.Empty(t, selectors, "BoardPending is dormant in its own file too: only its definition is exempt (ef44f246)")
	// Its queries run only inside it: a use of either outside its body and
	// their declarations is a caller under another name (71cf5dd9).
	queryUses := []string{}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && slices.Contains(queries, id.Name) && !declared[id] && (id.Pos() < method.Body.Pos() || id.Pos() > method.Body.End()) {
			queryUses = append(queryUses, fset.Position(id.Pos()).String())
		}
		return true
	})
	assert.Len(t, declared, len(queries), "the scan finds both queries' declarations in %s", own)
	assert.Empty(t, queryUses, "BoardPending's queries run only inside it (71cf5dd9)")
	others, _, _, _ := productionUses(t, filepath.Join("..", ".."), "internal/store", queries, nil)
	delete(others, own)
	assert.Empty(t, others, "no other file of the store uses BoardPending's queries (71cf5dd9)")
}

// TestBoardPendingNewestCeilingPostIsThePostsAlone (NERD-5533 053091ce, the
// user's decision of 2026-11-20): NewestCeilingPost is the newest ceiling
// post's creation, which a held notice, appended to or not, never moves:
// past an alert limit, each new warning posted directly alerts once.
func TestBoardPendingNewestCeilingPostIsThePostsAlone(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	at := func(n int) time.Time { return boardAt.Add(time.Duration(n) * time.Minute) }
	_, _, err := s.Notify(t.Context(), boss, at(1), func(string) (string, bool) { return ceilingLine, true })
	require.NoError(t, err)
	assert.Zero(t, pending(t, s).NewestCeilingPost, "a held ceiling alone")
	post(t, s, daemonPost("ha", "ctx_ceiling", "ha-boss is at 90% context", at(3)))
	assert.Equal(t, at(3), pending(t, s).NewestCeilingPost, "the post")
	_, appended, err := s.Notify(t.Context(), boss, at(5), func(queued string) (string, bool) {
		return queued + "\n" + ceilingLine, queued != ""
	})
	require.NoError(t, err)
	require.True(t, appended)
	got := pending(t, s)
	assert.Equal(t, at(5), got.NewestCeiling, "the held append")
	assert.Equal(t, at(3), got.NewestCeilingPost, "still the post")
}

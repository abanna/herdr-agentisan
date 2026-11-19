package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// daemonPosts are a flush's posts on ha's board, one per body, as the
// daemon makes them (A74).
func daemonPosts(bodies ...string) []store.BoardPost {
	out := make([]store.BoardPost, 0, len(bodies))
	for i, b := range bodies {
		out = append(out, store.BoardPost{
			Project: "ha", Channel: store.ChannelDaemon, Author: store.Daemon, Kind: "blocked",
			Subject: "w1:p2", Worker: "ha-coder-1", Body: b, At: boardAt.Add(time.Duration(i) * time.Second),
		})
	}
	return out
}

// assertOpen fails when ch, a BoardChanged channel, was closed.
func assertOpen(t *testing.T, ch <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(why)
	default:
	}
}

// TestPostAllPostsInOrder (A74): a flush's posts take its board in the
// order given, each returned with its seq and id, and read back as posted.
func TestPostAllPostsInOrder(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	got, err := s.PostAll(t.Context(), daemonPosts("first", "second", "third"))
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"first", "second", "third"}, bodies(got))
	for i, p := range got {
		assert.NotEmpty(t, p.ID, "post %d has an id", i)
		if i > 0 {
			assert.Greater(t, p.Seq, got[i-1].Seq, "post %d comes after post %d", i, i-1)
		}
	}
	read, err := s.ReadBoard(t.Context(), "ha", store.ReaderBoss, store.MaxBoardRead)
	require.NoError(t, err)
	assert.Equal(t, got, read.Posts, "the board holds them as returned")
}

// TestPostAllIsAllOrNone: a post the store refuses rolls the whole flush
// back, and wakes no one.
func TestPostAllIsAllOrNone(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	failBoardInserts(t, s, "NEW.body = 'third'")
	changed := s.BoardChanged()

	_, err := s.PostAll(t.Context(), daemonPosts("first", "second", "third"))
	require.ErrorIs(t, err, store.ErrStore)
	assert.Zero(t, postCount(t, s), "no post is left")
	assertOpen(t, changed, "a rolled-back flush woke the board's waiters")
}

// TestPostAllChecksBeforeWriting: every post is checked as Post checks one
// before any is written; one refused refuses them all.
func TestPostAllChecksBeforeWriting(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ps := daemonPosts("first", "second", "third")
	ps[2].Subject = "w1\np2"
	changed := s.BoardChanged()

	_, err := s.PostAll(t.Context(), ps)
	require.ErrorIs(t, err, store.ErrInvalid)
	assert.Zero(t, postCount(t, s), "nothing is posted")
	assertOpen(t, changed, "a refused flush woke the board's waiters")
}

// TestPostAllWakesOnceAfterCommit: the waiters are woken once the flush is
// committed, when the board's newest post is its last.
func TestPostAllWakesOnceAfterCommit(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	changed := s.BoardChanged()

	got, err := s.PostAll(t.Context(), daemonPosts("one", "two"))
	require.NoError(t, err)
	require.Len(t, got, 2)
	select {
	case <-changed:
	default:
		t.Fatal("the flush woke no waiter")
	}
	st, err := s.BoardStatus(t.Context(), "ha", store.ReaderBoss)
	require.NoError(t, err)
	assert.Equal(t, got[1].Seq, st.Newest)
	assertOpen(t, s.BoardChanged(), "the channel after the wake is already closed")
}

// TestPostAllOfNothing: an empty flush posts nothing and wakes no one.
func TestPostAllOfNothing(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	changed := s.BoardChanged()
	for _, ps := range [][]store.BoardPost{nil, {}} {
		got, err := s.PostAll(t.Context(), ps)
		require.NoError(t, err)
		assert.Empty(t, got)
	}
	assertOpen(t, changed, "an empty flush woke the board's waiters")
	assert.Zero(t, postCount(t, s))
}

// TestFitID: a subject or worker the board would take is kept; one it
// would refuse is emptied, so the post is made without it (A74).
func TestFitID(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "w14:p2", "ha-coder-1", strings.Repeat("é", 128)} {
		assert.Equal(t, v, store.FitID(v), "kept: %q", v)
	}
	for _, v := range []string{strings.Repeat("a", 257), "a\nb", "\uFEFFa", "a\tb", "\xff"} {
		assert.Empty(t, store.FitID(v), "emptied: %q", v)
	}
}

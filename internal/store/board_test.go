package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

var boardAt = time.Date(2026, 10, 23, 5, 0, 0, 0, time.UTC)

// teamPost is a team post on project's board by worker id.
func teamPost(project, id, body string) store.BoardPost {
	return store.BoardPost{Project: project, Channel: store.ChannelTeam, Author: worker(id), Kind: "post", Body: body, At: boardAt}
}

func post(t *testing.T, s *store.Store, p store.BoardPost) store.BoardPost {
	t.Helper()
	got, err := s.Post(t.Context(), p)
	require.NoError(t, err)
	return got
}

func bodies(ps []store.BoardPost) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Body)
	}
	return out
}

// TestBoardPostAndRead (A60): posts take their project's board in order,
// each with a seq and an id; a read is the reader's unread posts, oldest
// first, up to its limit, and moves only its own mark, so the boss reading
// never clears the user's unread. Another project's board is its own.
func TestBoardPostAndRead(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	first := post(t, s, teamPost("ha", "ha-coder-1", "PR #12 open"))
	assert.NotEmpty(t, first.ID)
	assert.Positive(t, first.Seq)
	notice := post(t, s, store.BoardPost{Project: "ha", Channel: store.ChannelDaemon, Author: store.Daemon, Kind: "turn_ended", Subject: "w1:p2", Worker: "ha-coder-2", Body: "ha-coder-2 finished a turn", At: boardAt.Add(time.Second)})
	post(t, s, store.BoardPost{Project: "ha", Channel: store.ChannelTeam, Author: pane, Kind: "post", Body: "from a pane", At: boardAt})
	post(t, s, teamPost("web", "web-coder-1", "another team"))
	assert.Greater(t, notice.Seq, first.Seq)

	read, err := s.ReadBoard(t.Context(), "ha", store.ReaderBoss, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"PR #12 open", "ha-coder-2 finished a turn"}, bodies(read.Posts))
	assert.Equal(t, 1, read.More)
	assert.Equal(t, notice.Seq, read.Mark)
	got := read.Posts[1]
	assert.Equal(t, notice, got, "a post reads back as it was posted")
	assert.Equal(t, time.UTC, got.At.Location())

	read, err = s.ReadBoard(t.Context(), "ha", store.ReaderBoss, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"from a pane"}, bodies(read.Posts))
	assert.Equal(t, 0, read.More)
	mark := read.Mark

	read, err = s.ReadBoard(t.Context(), "ha", store.ReaderBoss, 2)
	require.NoError(t, err)
	assert.Empty(t, read.Posts, "a post is read once")
	assert.Equal(t, mark, read.Mark)

	boss, err := s.BoardStatus(t.Context(), "ha", store.ReaderBoss)
	require.NoError(t, err)
	assert.Equal(t, store.BoardState{Newest: mark, Mark: mark, Unread: 0}, boss)
	user, err := s.BoardStatus(t.Context(), "ha", store.ReaderUser)
	require.NoError(t, err)
	assert.Equal(t, store.BoardState{Newest: mark, Mark: 0, Unread: 3}, user, "the boss's reads leave the user's mark")
	web, err := s.BoardStatus(t.Context(), "web", store.ReaderBoss)
	require.NoError(t, err)
	assert.Equal(t, 1, web.Unread, "another project's board is untouched")
	empty, err := s.BoardStatus(t.Context(), "", store.ReaderUser)
	require.NoError(t, err)
	assert.Equal(t, store.BoardState{}, empty, "the unnamed project's board, empty")
}

// TestBoardLatest (A60): the panel's read is the newest posts, newest
// first, and moves no mark.
func TestBoardLatest(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	for _, b := range []string{"one", "two", "three"} {
		post(t, s, teamPost("ha", "ha-coder-1", b))
	}
	latest, err := s.LatestPosts(t.Context(), "ha", 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"three", "two"}, bodies(latest))
	st, err := s.BoardStatus(t.Context(), "ha", store.ReaderUser)
	require.NoError(t, err)
	assert.Equal(t, 3, st.Unread, "the panel's read moves no mark")
	none, err := s.LatestPosts(t.Context(), "web", 2)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TestBoardMarkSeen (A60): the user's mark moves to what the panel showed,
// only forward, and never past the newest post, so a seq not yet handed out
// is never marked read; it moves no other reader's mark, nor another
// project's.
func TestBoardMarkSeen(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	mark, err := s.MarkSeen(t.Context(), "ha", store.ReaderUser, 9)
	require.NoError(t, err)
	assert.Equal(t, int64(0), mark, "an empty board marks nothing")

	var seqs []int64
	for _, b := range []string{"one", "two", "three"} {
		seqs = append(seqs, post(t, s, teamPost("ha", "ha-coder-1", b)).Seq)
	}
	post(t, s, teamPost("web", "web-coder-1", "another team"))
	mark, err = s.MarkSeen(t.Context(), "ha", store.ReaderUser, seqs[1])
	require.NoError(t, err)
	assert.Equal(t, seqs[1], mark)
	mark, err = s.MarkSeen(t.Context(), "ha", store.ReaderUser, seqs[0])
	require.NoError(t, err)
	assert.Equal(t, seqs[1], mark, "a mark only moves forward")
	mark, err = s.MarkSeen(t.Context(), "ha", store.ReaderUser, 0)
	require.NoError(t, err)
	assert.Equal(t, seqs[1], mark, "nothing shown moves nothing")
	mark, err = s.MarkSeen(t.Context(), "ha", store.ReaderUser, seqs[2]+100)
	require.NoError(t, err)
	assert.Equal(t, seqs[2], mark, "never past the newest post")

	next := post(t, s, teamPost("ha", "ha-coder-1", "four"))
	user, err := s.BoardStatus(t.Context(), "ha", store.ReaderUser)
	require.NoError(t, err)
	assert.Equal(t, store.BoardState{Newest: next.Seq, Mark: seqs[2], Unread: 1}, user, "a post after the clamp is unread")
	boss, err := s.BoardStatus(t.Context(), "ha", store.ReaderBoss)
	require.NoError(t, err)
	assert.Equal(t, 4, boss.Unread, "the user's mark leaves the boss's")
	web, err := s.BoardStatus(t.Context(), "web", store.ReaderUser)
	require.NoError(t, err)
	assert.Equal(t, 1, web.Unread, "and another project's")
	read, err := s.ReadBoard(t.Context(), "ha", store.ReaderUser, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"four"}, bodies(read.Posts), "a read starts after the seen mark")

	_, err = s.MarkSeen(t.Context(), "ha", "ha-coder-1", 1)
	require.ErrorIs(t, err, store.ErrInvalid, "workers never read the board")
	_, err = s.MarkSeen(t.Context(), "ha", store.ReaderUser, -1)
	require.ErrorIs(t, err, store.ErrInvalid)
}

// TestBoardRefuses: what the board must not hold, and reads it cannot make,
// are refused before anything is written.
func TestBoardRefuses(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ok := teamPost("ha", "ha-coder-1", "fine")
	with := func(change func(*store.BoardPost)) store.BoardPost {
		p := ok
		change(&p)
		return p
	}
	tests := map[string]struct {
		post store.BoardPost
		want error
	}{
		"no channel":           {with(func(p *store.BoardPost) { p.Channel = "" }), store.ErrInvalid},
		"another channel":      {with(func(p *store.BoardPost) { p.Channel = "boss" }), store.ErrInvalid},
		"no author":            {with(func(p *store.BoardPost) { p.Author = store.Address{} }), store.ErrInvalid},
		"an author of no kind": {with(func(p *store.BoardPost) { p.Author.Kind = "user" }), store.ErrInvalid},
		"no kind":              {with(func(p *store.BoardPost) { p.Kind = "" }), store.ErrInvalid},
		"a long kind":          {with(func(p *store.BoardPost) { p.Kind = strings.Repeat("k", 65) }), store.ErrInvalid},
		"a kind with a line":   {with(func(p *store.BoardPost) { p.Kind = "post\nx" }), store.ErrInvalid},
		"a project with a BOM": {with(func(p *store.BoardPost) { p.Project = "ha\uFEFF" }), store.ErrInvalid},
		"a subject with a tab": {with(func(p *store.BoardPost) { p.Subject = "w1\tp2" }), store.ErrInvalid},
		"a worker not UTF-8":   {with(func(p *store.BoardPost) { p.Worker = "\xff" }), store.ErrInvalid},
		"an empty body":        {with(func(p *store.BoardPost) { p.Body = "" }), store.ErrInvalid},
		"a body with a NUL":    {with(func(p *store.BoardPost) { p.Body = "a\x00b" }), store.ErrInvalid},
		"a body not UTF-8":     {with(func(p *store.BoardPost) { p.Body = "a\xffb" }), store.ErrInvalid},
		"a body past the cap":  {with(func(p *store.BoardPost) { p.Body = strings.Repeat("b", store.MaxBody+1) }), store.ErrTooLarge},
		"the zero time":        {with(func(p *store.BoardPost) { p.At = time.Time{} }), store.ErrInvalid},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := s.Post(t.Context(), tc.post)
			require.ErrorIs(t, err, tc.want)
		})
	}
	edge := post(t, s, with(func(p *store.BoardPost) {
		p.Body, p.Kind = strings.Repeat("b", store.MaxBody), strings.Repeat("k", 64)
	}))
	assert.Equal(t, int64(1), edge.Seq, "nothing refused was written; the cap itself is taken")

	_, err := s.ReadBoard(t.Context(), "ha", "worker", 1)
	require.ErrorIs(t, err, store.ErrInvalid, "workers never read the board")
	_, err = s.BoardStatus(t.Context(), "ha", "")
	require.ErrorIs(t, err, store.ErrInvalid)
	for _, limit := range []int{0, -1, store.MaxBoardRead + 1} {
		_, err = s.ReadBoard(t.Context(), "ha", store.ReaderBoss, limit)
		require.ErrorIs(t, err, store.ErrInvalid, "limit %d", limit)
		_, err = s.LatestPosts(t.Context(), "ha", limit)
		require.ErrorIs(t, err, store.ErrInvalid, "limit %d", limit)
	}
	read, err := s.ReadBoard(t.Context(), "ha", store.ReaderBoss, store.MaxBoardRead)
	require.NoError(t, err)
	assert.Len(t, read.Posts, 1, "the largest read is taken")
}

// TestPruneBoard (A60): posts go 30 days after they were made, and past
// BoardKeep a project, oldest first; a seq is never handed out again, so a
// mark at a pruned seq still counts the posts after it.
func TestPruneBoard(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	old := post(t, s, teamPost("ha", "ha-coder-1", "old"))
	_, err := s.ReadBoard(t.Context(), "ha", store.ReaderBoss, 1)
	require.NoError(t, err)
	n, err := s.PruneBoard(t.Context(), boardAt.Add(store.BoardRetention))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "a post exactly 30 days old stays")
	n, err = s.PruneBoard(t.Context(), boardAt.Add(store.BoardRetention+time.Nanosecond))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	next := post(t, s, teamPost("ha", "ha-coder-1", "new"))
	assert.Greater(t, next.Seq, old.Seq, "a pruned seq is never handed out again")
	st, err := s.BoardStatus(t.Context(), "ha", store.ReaderBoss)
	require.NoError(t, err)
	assert.Equal(t, store.BoardState{Newest: next.Seq, Mark: old.Seq, Unread: 1}, st)

	// BoardKeep+2 posts on ha in one statement, and one on web.
	_, err = store.DB(s).ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO board_posts (id, project, channel, author_kind, author, kind, body, created_at)
		SELECT 'bulk' || i, 'ha', 'team', 'worker', 'ha-coder-1', 'post', 'bulk ' || i, ? FROM n`, store.BoardKeep+2, boardAt.UnixNano())
	require.NoError(t, err)
	post(t, s, teamPost("web", "web-coder-1", "web's"))
	n, err = s.PruneBoard(t.Context(), boardAt)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n, "ha's three oldest: the post called new and the first two bulk ones")
	latest, err := s.LatestPosts(t.Context(), "ha", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"bulk 2002"}, bodies(latest))
	var oldest string
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT body FROM board_posts WHERE project = 'ha' ORDER BY seq LIMIT 1").Scan(&oldest))
	assert.Equal(t, "bulk 3", oldest)
	web, err := s.LatestPosts(t.Context(), "web", 5)
	require.NoError(t, err)
	assert.Equal(t, []string{"web's"}, bodies(web), "another project keeps its own")
}

// TestBoardChanged (A60): board.wait's wake. The channel closes at the next
// post, whichever writer makes it; a refused post wakes nobody, and each
// wake needs a fresh channel.
func TestBoardChanged(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ch := s.BoardChanged()
	assert.Equal(t, ch, s.BoardChanged(), "one channel until a post")
	_, err := s.Post(t.Context(), store.BoardPost{})
	require.Error(t, err)
	select {
	case <-ch:
		t.Fatal("a refused post woke a waiter")
	default:
	}
	post(t, s, teamPost("ha", "ha-coder-1", "wake"))
	select {
	case <-ch:
	default:
		t.Fatal("a post did not wake the waiter")
	}
	next := s.BoardChanged()
	select {
	case <-next:
		t.Fatal("the channel after a wake is already closed")
	default:
	}
}

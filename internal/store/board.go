package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The board's channels (ADR-001 A60): the team's posts, by its workers,
// panes and bosses, and the daemon's notices.
const (
	ChannelTeam   = "team"
	ChannelDaemon = "daemon"
)

// The board's readers (A60): the boss, by board read, and the user, by the
// dashboard's panel. Each has a mark of its own.
const (
	ReaderBoss = "boss"
	ReaderUser = "user"
)

const (
	// BoardRetention is how long a post is kept after it was made (A60).
	BoardRetention = 30 * 24 * time.Hour
	// BoardKeep is how many posts a project's board keeps at most: past it
	// the oldest go first.
	BoardKeep = 2000
	// MaxBoardRead is the most posts one read returns.
	MaxBoardRead = 200
	// maxBoardKind is the longest kind a post takes, in bytes.
	maxBoardKind = 64
)

// BoardPost is one post on a project's board.
type BoardPost struct {
	// Seq orders a project's posts: a larger Seq was posted later. Post
	// assigns it, and never hands one out twice.
	Seq int64
	// ID is the post's ULID, which Post assigns.
	ID      string
	Project string
	Channel string
	// Author is who posted it: a worker, a boss or a pane on the team
	// channel; the daemon on its own.
	Author Address
	// Kind is "post" for a team post, or the notice's kind for a daemon's.
	Kind string
	// Subject is the pane or PR a notice is about, and Worker the worker it
	// names; "" for a team post.
	Subject string
	Worker  string
	Body    string
	At      time.Time
}

// BoardRead is one reader's read of a project's board.
type BoardRead struct {
	// Posts are the reader's unread posts, oldest first, at most the limit.
	Posts []BoardPost
	// More counts the unread posts left after Posts.
	More int
	// Mark is the reader's mark after the read.
	Mark int64
}

// BoardState is where a reader of a project's board stands.
type BoardState struct {
	// Newest is the newest post's seq, 0 when the board is empty.
	Newest int64
	// Mark is the reader's mark: every post up to it is read.
	Mark int64
	// Unread counts the posts after Mark.
	Unread int
}

// boardWake is board.wait's wake (A60): its channel is closed, and a new
// one made, at each post, so a waiter wakes whichever writer posted. The
// zero value is ready.
type boardWake struct {
	mu sync.Mutex
	ch chan struct{}
}

func (w *boardWake) changed() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil {
		w.ch = make(chan struct{})
	}
	return w.ch
}

func (w *boardWake) signal() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch != nil {
		close(w.ch)
		w.ch = nil
	}
}

// BoardChanged is a channel closed at the next post on any board, through
// this store, which every writer of the board posts through. Ask again
// after each wake.
func (s *Store) BoardChanged() <-chan struct{} { return s.wake.changed() }

const insertPost = `INSERT INTO board_posts (id, project, channel, author_kind, author, kind, subject, worker, body, created_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)
RETURNING seq`

// Post adds p to its project's board and returns it with its Seq and ID.
// Its body is checked as a message's is (CheckBody), its channel is one of
// the board's, its author an address that can send, and its kind, subject
// and worker ids a line could carry (ErrInvalid, ErrTooLarge). Every waiter
// on BoardChanged is woken once it is committed.
func (s *Store) Post(ctx context.Context, p BoardPost) (BoardPost, error) {
	if err := checkPost(p); err != nil {
		return BoardPost{}, err
	}
	ns, err := nanos(p.At)
	if err != nil {
		return BoardPost{}, err
	}
	if p.ID, err = newULID(p.At, entropy); err != nil {
		return BoardPost{}, err
	}
	err = s.db.QueryRowContext(ctx, insertPost, p.ID, p.Project, p.Channel, p.Author.Kind, p.Author.ID, p.Kind, p.Subject, p.Worker, p.Body, ns).Scan(&p.Seq)
	if err != nil {
		return BoardPost{}, fmt.Errorf("%w: post to %q's board: %w", ErrStore, p.Project, err)
	}
	s.wake.signal()
	return p, nil
}

// checkPost refuses a post the board must not hold.
func checkPost(p BoardPost) error {
	if p.Channel != ChannelTeam && p.Channel != ChannelDaemon {
		return fmt.Errorf("%w: board channel %q", ErrInvalid, p.Channel)
	}
	if err := checkAddress(p.Author, false); err != nil {
		return err
	}
	if err := checkLine("project", p.Project, true); err != nil {
		return err
	}
	if err := checkLine("kind", p.Kind, false); err != nil {
		return err
	}
	if len(p.Kind) > maxBoardKind {
		return fmt.Errorf("%w: kind %q: at most %d bytes", ErrInvalid, p.Kind, maxBoardKind)
	}
	if err := checkLine("subject", p.Subject, true); err != nil {
		return err
	}
	if err := checkLine("worker", p.Worker, true); err != nil {
		return err
	}
	return CheckBody(p.Body)
}

// checkLine refuses a field that checkID would, but an empty one when it
// may be empty.
func checkLine(what, v string, empty bool) error {
	if v == "" && empty {
		return nil
	}
	if v == "" || len(v) > maxID || !utf8.ValidString(v) || strings.ContainsFunc(v, invisible) {
		return fmt.Errorf("%w: %s %q: want up to %d bytes of UTF-8 with no control or format character", ErrInvalid, what, v, maxID)
	}
	return nil
}

func checkReader(reader string) error {
	if reader != ReaderBoss && reader != ReaderUser {
		return fmt.Errorf("%w: board reader %q", ErrInvalid, reader)
	}
	return nil
}

func checkLimit(limit int) error {
	if limit < 1 || limit > MaxBoardRead {
		return fmt.Errorf("%w: a read of %d posts: want 1 to %d", ErrInvalid, limit, MaxBoardRead)
	}
	return nil
}

// The posts' reads, each with every column scanPosts reads, in its order.
const (
	selectUnread = `SELECT seq, id, project, channel, author_kind, author, kind, subject, worker, body, created_at
FROM board_posts WHERE project = ? AND seq > ? ORDER BY seq LIMIT ?`
	selectLatest = `SELECT seq, id, project, channel, author_kind, author, kind, subject, worker, body, created_at
FROM board_posts WHERE project = ? ORDER BY seq DESC LIMIT ?`
)

func scanPosts(rows *sql.Rows) ([]BoardPost, error) {
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports a failed read
	out := []BoardPost{}
	for rows.Next() {
		var p BoardPost
		var ns int64
		if err := rows.Scan(&p.Seq, &p.ID, &p.Project, &p.Channel, &p.Author.Kind, &p.Author.ID, &p.Kind, &p.Subject, &p.Worker, &p.Body, &ns); err != nil {
			return nil, fmt.Errorf("%w: read a post: %w", ErrStore, err)
		}
		p.At = time.Unix(0, ns).UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: read the posts: %w", ErrStore, err)
	}
	return out, nil
}

// querier is what both a transaction and the store's connection run.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// boardState is where reader stands on project's board, read through q.
func boardState(ctx context.Context, q querier, project, reader string) (BoardState, error) {
	var st BoardState
	err := q.QueryRowContext(ctx, `SELECT
		coalesce((SELECT max(seq) FROM board_posts WHERE project = ?1), 0),
		coalesce((SELECT seq FROM board_marks WHERE project = ?1 AND reader = ?2), 0)`, project, reader).Scan(&st.Newest, &st.Mark)
	if err != nil {
		return BoardState{}, fmt.Errorf("%w: read %q's board: %w", ErrStore, project, err)
	}
	err = q.QueryRowContext(ctx, "SELECT count(*) FROM board_posts WHERE project = ? AND seq > ?", project, st.Mark).Scan(&st.Unread)
	if err != nil {
		return BoardState{}, fmt.Errorf("%w: count %q's unread posts: %w", ErrStore, project, err)
	}
	return st, nil
}

// BoardStatus is where reader stands on project's board.
func (s *Store) BoardStatus(ctx context.Context, project, reader string) (BoardState, error) {
	if err := checkReader(reader); err != nil {
		return BoardState{}, err
	}
	return boardState(ctx, s.db, project, reader)
}

const setMark = `INSERT INTO board_marks (project, reader, seq) VALUES (?1, ?2, ?3)
ON CONFLICT (project, reader) DO UPDATE SET seq = max(seq, excluded.seq)`

// ReadBoard is reader's unread posts on project's board, oldest first, at
// most limit (1 to MaxBoardRead), and moves reader's mark past them in the
// same transaction: a post is read once. It moves no other reader's mark.
func (s *Store) ReadBoard(ctx context.Context, project, reader string, limit int) (BoardRead, error) {
	if err := checkReader(reader); err != nil {
		return BoardRead{}, err
	}
	if err := checkLimit(limit); err != nil {
		return BoardRead{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BoardRead{}, fmt.Errorf("%w: read %q's board: %w", ErrStore, project, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	st, err := boardState(ctx, tx, project, reader)
	if err != nil {
		return BoardRead{}, err
	}
	rows, err := tx.QueryContext(ctx, selectUnread, project, st.Mark, limit)
	if err != nil {
		return BoardRead{}, fmt.Errorf("%w: read %q's board: %w", ErrStore, project, err)
	}
	posts, err := scanPosts(rows)
	if err != nil {
		return BoardRead{}, err
	}
	read := BoardRead{Posts: posts, More: st.Unread - len(posts), Mark: st.Mark}
	if len(posts) > 0 {
		read.Mark = posts[len(posts)-1].Seq
		if _, err := tx.ExecContext(ctx, setMark, project, reader, read.Mark); err != nil {
			return BoardRead{}, fmt.Errorf("%w: mark %q's board read: %w", ErrStore, project, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return BoardRead{}, fmt.Errorf("%w: read %q's board: %w", ErrStore, project, err)
	}
	return read, nil
}

// LatestPosts is project's newest posts, newest first, at most limit (1 to
// MaxBoardRead). It moves no mark.
func (s *Store) LatestPosts(ctx context.Context, project string, limit int) ([]BoardPost, error) {
	if err := checkLimit(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, selectLatest, project, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: read %q's board: %w", ErrStore, project, err)
	}
	return scanPosts(rows)
}

// MarkSeen moves reader's mark on project's board to upto, as when the
// dashboard's panel has shown every post up to it, and returns the mark. A
// mark only moves forward, and never past the newest post: a seq not yet
// handed out is never marked read. It moves no other reader's mark.
func (s *Store) MarkSeen(ctx context.Context, project, reader string, upto int64) (int64, error) {
	if err := checkReader(reader); err != nil {
		return 0, err
	}
	if upto < 0 {
		return 0, fmt.Errorf("%w: a mark of %d", ErrInvalid, upto)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: mark %q's board seen: %w", ErrStore, project, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	st, err := boardState(ctx, tx, project, reader)
	if err != nil {
		return 0, err
	}
	mark := max(st.Mark, min(upto, st.Newest))
	if mark > st.Mark {
		if _, err := tx.ExecContext(ctx, setMark, project, reader, mark); err != nil {
			return 0, fmt.Errorf("%w: mark %q's board seen: %w", ErrStore, project, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: mark %q's board seen: %w", ErrStore, project, err)
	}
	return mark, nil
}

// PruneBoard deletes the posts made more than BoardRetention before now,
// and each project's oldest past BoardKeep, and reports how many it
// deleted. The marks stay: a seq is never handed out again, so a mark only
// ever counts posts after it.
func (s *Store) PruneBoard(ctx context.Context, now time.Time) (int64, error) {
	cutoff, err := nanos(now.Add(-BoardRetention))
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM board_posts WHERE created_at < ?1 OR seq IN (
		SELECT seq FROM (SELECT seq, row_number() OVER (PARTITION BY project ORDER BY seq DESC) AS n FROM board_posts) WHERE n > ?2)`,
		cutoff, BoardKeep)
	if err != nil {
		return 0, fmt.Errorf("%w: prune the board: %w", ErrStore, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%w: prune the board: %w", ErrStore, err)
	}
	return n, nil
}

// BoardPending is what a board-mode boss has left waiting (ADR-001 A74).
type BoardPending struct {
	Pending               int       // posts after the boss's mark
	Oldest, OldestCeiling time.Time // zero: none
	BossMark              int64
	Held                  int       // messages queued for the boss in its inbox
	OldestHeld            time.Time // zero: none
	OldestHeldCeiling     time.Time // the oldest held daemon row holding ceilingMark; zero: none
	NewestCeiling         time.Time // the latest ceiling change pending: max(created_at) of the ceiling posts, max(updated_at) of the held daemon rows holding ceilingMark; zero: none
	NewestCeilingPost     time.Time // max(created_at) of the ceiling posts alone, for rule 5 past a limit (user decision 2026-11-20); zero: none
}

// pendingPosts is the posts after the boss's mark on a project's board
// (?1), with its ceilings, ?2 being the ceiling mark: a daemon post of kind
// ctx_ceiling, or a daemon notices post holding the mark. It runs on
// board_posts_by_project (project = ? AND seq > ?).
const pendingPosts = `SELECT count(*), min(created_at),
  min(CASE WHEN channel = 'daemon' AND (kind = 'ctx_ceiling' OR (kind = 'notices' AND instr(body, ?2) > 0)) THEN created_at END),
  max(CASE WHEN channel = 'daemon' AND (kind = 'ctx_ceiling' OR (kind = 'notices' AND instr(body, ?2) > 0)) THEN created_at END),
  (SELECT coalesce(max(seq), 0) FROM board_marks WHERE project = ?1 AND reader = 'boss')
FROM board_posts
WHERE project = ?1 AND seq > (SELECT coalesce(max(seq), 0) FROM board_marks WHERE project = ?1 AND reader = 'boss')`

// pendingHeld is the messages queued for a boss (?1) in its inbox, with its
// held ceilings, ?2 being the ceiling mark: a daemon notice holding the
// mark, the oldest timed by its creation, at or before its ceiling line,
// the newest change by its last update, which an appended line moves. A
// notice of a failed delivery (notice_of) or a returned row (returned_from)
// is none, whatever it quotes: posted, rowPosts gives it no notices kind. It
// runs on messages_inbox. PR 3c adds its acknowledgement filter to this
// WHERE, and the ceilings are then read over the same rows.
const pendingHeld = `SELECT count(*), min(created_at),
  min(CASE WHEN sender_kind = 'daemon' AND notice_of = '' AND returned_from = '' AND instr(body, ?2) > 0 THEN created_at END),
  max(CASE WHEN sender_kind = 'daemon' AND notice_of = '' AND returned_from = '' AND instr(body, ?2) > 0 THEN updated_at END)
FROM messages
WHERE recipient_kind = 'boss' AND recipient = ?1 AND state = 'queued'`

// BoardPending is what the boss of project has left waiting on its board
// and in its inbox, in one read transaction (ADR-001 A74). ceilingMark is
// notice.CeilingMark. The user's read mark is never read: the dashboard's
// panel moves it. An empty ceilingMark is refused (ErrInvalid): it would be
// in every body, and every daemon notice would read as a ceiling.
func (s *Store) BoardPending(ctx context.Context, project, boss, ceilingMark string) (BoardPending, error) {
	if ceilingMark == "" {
		return BoardPending{}, fmt.Errorf("%w: an empty ceiling mark matches every notice", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BoardPending{}, fmt.Errorf("%w: read what %q has pending: %w", ErrStore, boss, err)
	}
	defer tx.Rollback() //nolint:errcheck // a read: nothing to commit
	var p BoardPending
	var oldest, oldestCeiling, newestCeiling, oldestHeld, oldestHeldCeiling, newestHeldCeiling sql.NullInt64
	err = tx.QueryRowContext(ctx, pendingPosts, project, ceilingMark).Scan(&p.Pending, &oldest, &oldestCeiling, &newestCeiling, &p.BossMark)
	if err != nil {
		return BoardPending{}, fmt.Errorf("%w: read %q's pending posts: %w", ErrStore, project, err)
	}
	err = tx.QueryRowContext(ctx, pendingHeld, boss, ceilingMark).Scan(&p.Held, &oldestHeld, &oldestHeldCeiling, &newestHeldCeiling)
	if err != nil {
		return BoardPending{}, fmt.Errorf("%w: read %q's held messages: %w", ErrStore, boss, err)
	}
	p.Oldest, p.OldestCeiling, p.OldestHeld, p.OldestHeldCeiling = pendingTime(oldest), pendingTime(oldestCeiling), pendingTime(oldestHeld), pendingTime(oldestHeldCeiling)
	p.NewestCeiling = pendingTime(newestCeiling)
	p.NewestCeilingPost = p.NewestCeiling
	if newest := pendingTime(newestHeldCeiling); newest.After(p.NewestCeiling) {
		p.NewestCeiling = newest
	}
	return p, nil
}

// pendingTime is a pending read's time, zero for NULL: none.
func pendingTime(ns sql.NullInt64) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return time.Unix(0, ns.Int64).UTC()
}

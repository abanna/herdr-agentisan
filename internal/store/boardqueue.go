package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// partRoom is the room a part's "(part i of n) " prefix takes in a post.
const partRoom = 32

// MaxQueuePost is the most rows one PostQueued call posts. A call holds the
// store's one connection for its whole transaction, and its caller gives it
// 5 s. Measured under -race, a batch of 8 KiB rows took 0.85 s, and one of
// the largest rows, failure notices of two posts each (8,800 bytes), 1.03 s:
// about a second. A backlog drains a batch a call (NERD-5533 1245ef8b, the
// boss's ruling on the cap's value).
const MaxQueuePost = 100

// noticesHeader is the first line of A44's notices to a boss. Their post
// drops it when more follows it.
const noticesHeader = "[agentisan] notices:"

// PostedQueue is what PostQueued did.
type PostedQueue struct {
	Posts     []BoardPost // every post it made, in seq order
	Delivered []Message   // rows posted, as they now stand (state delivered)
	Failed    []Message   // rows the board refused, as they now stand (state failed)
	More      bool        // queued rows for to remained after its last row
}

const (
	// selectBossQueue is a boss's oldest ?2 queued messages, oldest first;
	// one dispatching is the dispatcher's, never selected. messages_inbox
	// (migration 0002) serves it.
	selectBossQueue = `SELECT ` + messageColumns + ` FROM messages
WHERE recipient_kind = 'boss' AND recipient = ?1 AND state = 'queued' ORDER BY seq LIMIT ?2`
	deliverQueued = `UPDATE messages SET state = 'delivered', updated_at = max(?2, created_at)
WHERE id = ?1 AND state = 'queued' RETURNING ` + messageColumns
	failQueued = `UPDATE messages SET state = 'failed', reason = ?2, updated_at = max(?3, created_at)
WHERE id = ?1 AND state = 'queued' RETURNING ` + messageColumns
	// refusedFormat is the undelivered post in a refused row's place.
	refusedFormat = "[agentisan] message %s from %s could not be posted on the board (%s); it is kept, failed, in state.db"
)

// PostQueued posts the oldest MaxQueuePost messages queued for the boss `to`
// on project's board, oldest first, instead of typing it (ADR-001 A74), in
// one transaction, and says in More whether more remain for a later call.
// Each row posted is marked delivered in it; a body over MaxBody is posted
// in parts, "(part i of n) " before each. A row the board refuses is failed
// on its own, with an undelivered post from the daemon in its place. A message dispatching is the dispatcher's and is left alone.
// Any statement that fails rolls the whole call back (ErrStore), every row
// still queued. Every waiter on BoardChanged is woken once the posts are
// committed, and only when there are some.
func (s *Store) PostQueued(ctx context.Context, to Address, project string, at time.Time) (PostedQueue, error) {
	if to.Kind != KindBoss {
		return PostedQueue{}, fmt.Errorf("%w: posts are made from a boss's queue, not a %s's", ErrInvalid, to.Kind)
	}
	if err := checkAddress(to, true); err != nil {
		return PostedQueue{}, err
	}
	if err := checkLine("project", project, true); err != nil {
		return PostedQueue{}, err
	}
	ns, err := nanos(at)
	if err != nil {
		return PostedQueue{}, err
	}
	out, err := s.postQueued(ctx, to, project, ns)
	if err != nil {
		return PostedQueue{}, fmt.Errorf("%w: post %s's queue: %w", ErrStore, to.ID, err)
	}
	if len(out.Posts) > 0 {
		s.wake.signal()
	}
	return out, nil
}

// postQueued is PostQueued's transaction.
func (s *Store) postQueued(ctx context.Context, to Address, project string, ns int64) (PostedQueue, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PostedQueue{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	rows, err := tx.QueryContext(ctx, selectBossQueue, to.ID, MaxQueuePost+1)
	if err != nil {
		return PostedQueue{}, fmt.Errorf("select: %w", err)
	}
	queued, err := scanMessages(rows)
	if err != nil {
		return PostedQueue{}, err
	}
	var out PostedQueue
	if len(queued) > MaxQueuePost {
		queued, out.More = queued[:MaxQueuePost], true
	}
	for _, m := range queued {
		if err := postRow(ctx, tx, m, project, ns, &out); err != nil {
			return PostedQueue{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PostedQueue{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// postRow posts m and marks it delivered or, when the board refuses one of
// its posts, inserts none of them and fails it instead, and records what it
// did in out.
func postRow(ctx context.Context, tx *sql.Tx, m Message, project string, ns int64, out *PostedQueue) error {
	posts, err := rowPosts(ctx, tx, m, project)
	if err != nil {
		return err
	}
	for _, p := range posts {
		if refused := checkPost(p); refused != nil {
			return refuseRow(ctx, tx, m, project, refused, ns, out)
		}
	}
	for _, p := range posts {
		if err := insertBoardPost(ctx, tx, &p); err != nil {
			return err
		}
		out.Posts = append(out.Posts, p)
	}
	d, err := scanMessage(tx.QueryRowContext(ctx, deliverQueued, m.ID, ns))
	if err != nil {
		return fmt.Errorf("deliver %s: %w", m.ID, err)
	}
	out.Delivered = append(out.Delivered, d)
	return nil
}

// rowPosts is m as posts: its row-to-post rule, the first that matches, a
// body over MaxBody in parts. The first post takes m's id unless a post
// holds it already; every other takes a fresh one.
func rowPosts(ctx context.Context, tx *sql.Tx, m Message, project string) ([]BoardPost, error) {
	p := BoardPost{ID: m.ID, Project: project, Channel: ChannelTeam, Author: m.From, Kind: "post", Body: m.Body, At: m.CreatedAt}
	daemon := m.From.Kind == KindDaemon
	if daemon {
		p.Channel, p.Author = ChannelDaemon, Daemon
	}
	switch {
	case m.ReturnedFrom != "":
		p.Kind, p.Subject = "returned", m.ReturnedFrom
	case !daemon:
	case m.NoticeOf != "":
		p.Kind, p.Subject = "undelivered", m.NoticeOf
	default:
		p.Kind = "notices"
		if rest, ok := strings.CutPrefix(m.Body, noticesHeader+"\n"); ok && rest != "" {
			p.Body = rest
		}
	}
	bodies := splitBody(p.Body)
	posts := make([]BoardPost, len(bodies))
	for i, b := range bodies {
		posts[i] = p
		posts[i].Body = b
		if i == 0 {
			taken, err := postIDTaken(ctx, tx, m.ID)
			if err != nil {
				return nil, err
			}
			if !taken {
				continue
			}
		}
		id, err := newULID(m.CreatedAt, entropy)
		if err != nil {
			return nil, err
		}
		posts[i].ID = id
	}
	return posts, nil
}

// postIDTaken reports whether a post already holds id: board_posts.id is
// UNIQUE, so a post reusing it would abort the call, pass after pass.
func postIDTaken(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM board_posts WHERE id = ?1", id).Scan(new(int))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check post id %s: %w", id, err)
	}
	return true, nil
}

// splitBody is body as one post's, or, over MaxBody, as parts of at most
// MaxBody - partRoom bytes, each cut moved back to a rune boundary, at most
// 3 bytes, and each part prefixed "(part i of n) ".
func splitBody(body string) []string {
	if len(body) <= MaxBody {
		return []string{body}
	}
	var parts []string
	for body != "" {
		n := min(len(body), MaxBody-partRoom)
		for back := 0; back < utf8.UTFMax-1 && n < len(body) && !utf8.RuneStart(body[n]); back++ {
			n--
		}
		parts = append(parts, body[:n])
		body = body[n:]
	}
	for i := range parts {
		parts[i] = fmt.Sprintf("(part %d of %d) ", i+1, len(parts)) + parts[i]
	}
	return parts
}

// refuseRow fails m, whose post the board refused for why, and puts the
// daemon's undelivered post in its place. That post is not checked: every
// field is the daemon's own or an id the row holds, so it cannot wedge the
// call. No notice goes to m's sender, and no messages row is written: after
// the split only an id can be refused, and a notice to a malformed id would
// wait forever, since the prune never removes a queued message.
func refuseRow(ctx context.Context, tx *sql.Tx, m Message, project string, why error, ns int64, out *PostedQueue) error {
	reason := CleanReason("the board refused it: " + why.Error())
	f, err := scanMessage(tx.QueryRowContext(ctx, failQueued, m.ID, reason, ns))
	if err != nil {
		return fmt.Errorf("fail %s: %w", m.ID, err)
	}
	id, err := newULID(m.CreatedAt, entropy)
	if err != nil {
		return err
	}
	p := BoardPost{
		ID: id, Project: project, Channel: ChannelDaemon, Author: Daemon, Kind: "undelivered", Subject: m.ID,
		Body: fmt.Sprintf(refusedFormat, m.ID, strconv.Quote(m.From.ID), reason), At: m.CreatedAt,
	}
	if err := insertBoardPost(ctx, tx, &p); err != nil {
		return err
	}
	out.Posts = append(out.Posts, p)
	out.Failed = append(out.Failed, f)
	return nil
}

// insertBoardPost inserts p in tx and sets its Seq.
func insertBoardPost(ctx context.Context, tx *sql.Tx, p *BoardPost) error {
	err := tx.QueryRowContext(ctx, insertPost, p.ID, p.Project, p.Channel, p.Author.Kind, p.Author.ID, p.Kind, p.Subject, p.Worker, p.Body, p.At.UnixNano()).Scan(&p.Seq)
	if err != nil {
		return fmt.Errorf("insert post %s: %w", p.ID, err)
	}
	return nil
}

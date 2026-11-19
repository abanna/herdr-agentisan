package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The user's acknowledgements (ADR-001 A74, migration 0011), made in the
// dashboard's board panel alone: of posts, a record per post that moves
// neither reader's mark and goes with its post at the prune, and of the
// messages waiting in a boss's inbox, one record per project and boss that a
// later one replaces. They are dormant: nothing in production calls them
// before the board's PR 3c.

// AckResult is what an acknowledgement wrote.
type AckResult struct {
	Acked       int       // posts newly acknowledged
	HeldThrough int64     // AckUpto only: the newest seq queued for boss, 0 with none
	HeldAt      time.Time // AckUpto only: the latest updated_at queued for boss, zero with none
}

// ackPosts acknowledges, at ?2, the posts on ?1's board the boss has not
// read and no one acknowledged, that the caller's condition, from ?3 on,
// picks.
const ackPosts = `INSERT INTO board_acks (seq, project, acked_at)
SELECT seq, project, ?2 FROM board_posts AS p
WHERE project = ?1
	AND seq > coalesce((SELECT seq FROM board_marks WHERE project = ?1 AND reader = 'boss'), 0)
	AND NOT EXISTS (SELECT 1 FROM board_acks AS a WHERE a.seq = p.seq)
	AND `

const (
	ackOnePost  = ackPosts + `seq = ?3`
	ackUptoPost = ackPosts + `seq <= ?3 AND (?4 = '' OR channel = ?4)`
	// ackHeld records, for project ?1 and boss ?2 at ?3, every message then
	// queued for the boss: the newest seq and the latest updated_at among
	// them, 0 and 0 with none. A message queued later, a notice added to a
	// waiting one and a message returned to the boss each have a newer seq
	// or updated_at, so each counts again. It replaces the last record rather
	// than keep the larger values: each covers everything queued at its
	// moment, and a clock stepped back must not hide a later append behind an
	// earlier record. The SELECT keeps its WHERE: SQLite's parser needs one
	// before ON CONFLICT in an INSERT ... SELECT.
	ackHeld = `INSERT INTO board_held_acks (project, boss, through_seq, through_at, acked_at)
SELECT ?1, ?2, coalesce(max(seq), 0), coalesce(max(updated_at), 0), ?3 FROM messages
WHERE recipient_kind = 'boss' AND recipient = ?2 AND state = 'queued'
ON CONFLICT (project, boss) DO UPDATE SET through_seq = excluded.through_seq,
	through_at = excluded.through_at, acked_at = excluded.acked_at
RETURNING through_seq, through_at`
)

// AckPost acknowledges post seq on project's board for the user (A74): only
// a post the boss has not read, not yet acknowledged. A seq not on that
// board acknowledges nothing and is no error: the prune may have removed it.
func (s *Store) AckPost(ctx context.Context, project string, seq int64, at time.Time) (AckResult, error) {
	if err := checkLine("project", project, true); err != nil {
		return AckResult{}, err
	}
	if seq < 0 {
		return AckResult{}, fmt.Errorf("%w: a post seq of %d", ErrInvalid, seq)
	}
	ns, err := nanos(at)
	if err != nil {
		return AckResult{}, err
	}
	var r AckResult
	err = s.ack(ctx, project, func(tx *sql.Tx) error {
		r.Acked, err = ackRows(ctx, tx, ackOnePost, project, ns, seq)
		return err
	})
	if err != nil {
		return AckResult{}, err
	}
	return r, nil
}

// AckUpto acknowledges every such post up to min(upto, newest), on channel
// ("" for both), and every message then queued for boss, which it records
// in board_held_acks for project and boss, replacing the last record.
func (s *Store) AckUpto(ctx context.Context, project, boss string, upto int64, channel string, at time.Time) (AckResult, error) {
	if err := checkLine("project", project, true); err != nil {
		return AckResult{}, err
	}
	if upto < 0 {
		return AckResult{}, fmt.Errorf("%w: an acknowledgement up to %d", ErrInvalid, upto)
	}
	if channel != "" && channel != ChannelTeam && channel != ChannelDaemon {
		return AckResult{}, fmt.Errorf("%w: board channel %q", ErrInvalid, channel)
	}
	if err := checkID(KindBoss, boss); err != nil {
		return AckResult{}, err
	}
	ns, err := nanos(at)
	if err != nil {
		return AckResult{}, err
	}
	var r AckResult
	err = s.ack(ctx, project, func(tx *sql.Tx) error {
		if r.Acked, err = ackRows(ctx, tx, ackUptoPost, project, ns, upto, channel); err != nil {
			return err
		}
		var heldAt int64
		if err := tx.QueryRowContext(ctx, ackHeld, project, boss, ns).Scan(&r.HeldThrough, &heldAt); err != nil {
			return fmt.Errorf("%w: acknowledge %s's held messages: %w", ErrStore, boss, err)
		}
		if heldAt != 0 {
			r.HeldAt = time.Unix(0, heldAt).UTC()
		}
		return nil
	})
	if err != nil {
		return AckResult{}, err
	}
	return r, nil
}

// ack runs write in one transaction, rolled back on any error.
func (s *Store) ack(ctx context.Context, project string, write func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: acknowledge %q's posts: %w", ErrStore, project, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	if err := write(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: acknowledge %q's posts: %w", ErrStore, project, err)
	}
	return nil
}

// ackRows runs one of the acknowledgements' inserts and counts its rows.
func ackRows(ctx context.Context, tx *sql.Tx, insert, project string, args ...any) (int, error) {
	res, err := tx.ExecContext(ctx, insert, append([]any{project}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("%w: acknowledge %q's posts: %w", ErrStore, project, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%w: acknowledge %q's posts: %w", ErrStore, project, err)
	}
	return int(n), nil
}

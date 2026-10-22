package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// queuedNotice is the newest message the daemon sent the boss ?1 that is
// still queued, other than a notice of a failed delivery. A message the
// daemon sent that was returned from a worker is one of those: the daemon
// sends a worker nothing else.
const queuedNotice = `SELECT ` + messageColumns + ` FROM messages
WHERE recipient_kind = 'boss' AND recipient = ?1 AND sender_kind = 'daemon' AND state = 'queued' AND notice_of = ''
ORDER BY seq DESC LIMIT 1`

// appendNotice rewrites the queued message ?1's body to ?2, at ?3.
const appendNotice = `UPDATE messages SET body = ?2, updated_at = max(?3, created_at)
WHERE id = ?1 AND state = 'queued'
RETURNING ` + messageColumns

// Notify queues a notice from the daemon for the boss `to` (ADR-001 A44), in
// one transaction: the newest such notice still queued for it gets the body
// body(its body), when body takes it; otherwise, or with none queued, a new
// message gets body(""). So a boss that is away gets one notice, not a pile.
// A notice of a failed delivery and a notice already taken for dispatch
// are never appended to. Each body is checked as
// Send checks one. It returns the message, and whether it was appended to.
func (s *Store) Notify(ctx context.Context, to Address, at time.Time, body func(queued string) (string, bool)) (Message, bool, error) {
	if to.Kind != KindBoss {
		return Message{}, false, fmt.Errorf("%w: notices go to a boss, not a %s", ErrInvalid, to.Kind)
	}
	if err := checkAddress(to, true); err != nil {
		return Message{}, false, err
	}
	ns, err := nanos(at)
	if err != nil {
		return Message{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, false, fmt.Errorf("%w: notify: %w", ErrStore, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	m, appended, err := notify(ctx, tx, to, at, ns, body)
	if err != nil {
		return Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, fmt.Errorf("%w: notify: %w", ErrStore, err)
	}
	return m, appended, nil
}

func notify(ctx context.Context, tx *sql.Tx, to Address, at time.Time, ns int64, body func(string) (string, bool)) (Message, bool, error) {
	q, err := scanMessage(tx.QueryRowContext(ctx, queuedNotice, to.ID))
	switch {
	case err == nil:
		if b, ok := body(q.Body); ok {
			if err := CheckBody(b); err != nil {
				return Message{}, false, err
			}
			m, err := scanMessage(tx.QueryRowContext(ctx, appendNotice, q.ID, b, ns))
			if err != nil {
				return Message{}, false, fmt.Errorf("%w: notify: append to %s: %w", ErrStore, q.ID, err)
			}
			return m, true, nil
		}
	case !errors.Is(err, sql.ErrNoRows):
		return Message{}, false, fmt.Errorf("%w: notify: %w", ErrStore, err)
	}
	b, _ := body("")
	if err := CheckBody(b); err != nil {
		return Message{}, false, err
	}
	id, err := newULID(at, entropy)
	if err != nil {
		return Message{}, false, err
	}
	m, err := scanMessage(tx.QueryRowContext(ctx, sendMessage, id, to.Kind, to.ID, "", sql.NullInt64{}, Daemon.Kind, Daemon.ID, sql.NullInt64{}, b, ns))
	if err != nil {
		return Message{}, false, fmt.Errorf("%w: notify: %w", ErrStore, err)
	}
	return m, false, nil
}

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The kinds of address a message has (ADR-001 D8, as amended by A2).
const (
	// KindWorker is a logical worker id, which a workers row binds.
	KindWorker = "worker"
	// KindBoss is a team's boss, <short>-boss. It has no workers row.
	KindBoss = "boss"
	// KindPane is a herdr pane id: a sender with no other identity, and so
	// where its notices go.
	KindPane = "pane"
	// KindDaemon is the daemon itself. It sends; nothing is sent to it.
	KindDaemon = "daemon"
)

// The states a message goes through (A2): queued, dispatching, then
// delivered, or failed when it may not have arrived.
const (
	MessageQueued      = "queued"
	MessageDispatching = "dispatching"
	MessageDelivered   = "delivered"
	MessageFailed      = "failed"
)

const (
	// MaxBody is the longest body Send takes, in bytes. A notice carries
	// the body it reports, so it can be longer.
	MaxBody = 8 << 10
	// MessageRetention is how long a delivered or failed message is kept
	// after its last change (A1). Elastic keeps the long-term history.
	MessageRetention = 14 * 24 * time.Hour
	// maxID is the longest id an address takes, in bytes.
	maxID = 256
	// maxReason is the longest reason kept, in bytes. A longer one is cut,
	// never refused: a failure must always be recorded.
	maxReason = 512
)

var (
	// ErrNotBound means no live workers row has the logical id.
	ErrNotBound = errors.New("no live worker has that logical id")
	// ErrTooLarge means a body is longer than MaxBody.
	ErrTooLarge = errors.New("message body too large")
	// ErrNoMessage means no message has the id, or not in the state the
	// operation moves it from.
	ErrNoMessage = errors.New("no such message in that state")
)

// Daemon is the sender of every notice.
var Daemon = Address{Kind: KindDaemon, ID: "agentisan"}

// entropy is the random part of every ULID.
var entropy = rand.Reader

// Address is who a message is to or from.
type Address struct {
	Kind string
	ID   string
}

// Message is one row of the inbox.
type Message struct {
	// ID is the message's ULID.
	ID string
	To Address
	// Project and Generation are the binding of the worker it was sent to,
	// so a logical id reused later never inherits it. A message returned to
	// the boss keeps its Project; one sent to a boss or a pane has "" and 0.
	Project    string
	Generation int64
	From       Address
	Body       string
	State      string
	// Attempts counts its claims.
	Attempts int
	// Reason is why the last attempt did not deliver it.
	Reason string
	// NoticeOf is, for a notice, the id of the message whose failure it
	// reports; "" for any other message.
	NoticeOf string
	// ReturnedFrom is, for a message returned to the boss, the worker it was
	// sent to.
	ReturnedFrom string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const messageColumns = "id, recipient_kind, recipient, project, generation, sender_kind, sender, body, state, attempts, reason, notice_of, returned_from, created_at, updated_at"

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var (
		m                Message
		gen              sql.NullInt64
		created, updated int64
	)
	err := row.Scan(&m.ID, &m.To.Kind, &m.To.ID, &m.Project, &gen, &m.From.Kind, &m.From.ID, &m.Body,
		&m.State, &m.Attempts, &m.Reason, &m.NoticeOf, &m.ReturnedFrom, &created, &updated)
	if err != nil {
		return Message{}, fmt.Errorf("read message: %w", err)
	}
	m.Generation = gen.Int64
	m.CreatedAt, m.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	return m, nil
}

const sendMessage = `INSERT INTO messages (id, recipient_kind, recipient, project, generation, sender_kind, sender, sender_generation, body, created_at, updated_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?10)
RETURNING ` + messageColumns

// Send queues body, from `from` to `to`, and returns the queued message. A
// worker, as recipient or sender, must have a live workers row, or it is
// ErrNotBound: the message belongs to that binding, its generation recorded.
// A body is UTF-8 text with no NUL, at most MaxBody bytes (ErrTooLarge).
// Nothing is sent to the daemon.
func (s *Store) Send(ctx context.Context, to, from Address, body string, at time.Time) (Message, error) {
	if err := checkAddress(to, true); err != nil {
		return Message{}, err
	}
	if err := checkAddress(from, false); err != nil {
		return Message{}, err
	}
	if err := checkBody(body); err != nil {
		return Message{}, err
	}
	ns, err := nanos(at)
	if err != nil {
		return Message{}, err
	}
	id, err := newULID(at, entropy)
	if err != nil {
		return Message{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, fmt.Errorf("%w: send: %w", ErrStore, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	project, gen, err := binding(ctx, tx, to)
	if err != nil {
		return Message{}, err
	}
	_, senderGen, err := binding(ctx, tx, from)
	if err != nil {
		return Message{}, err
	}
	m, err := scanMessage(tx.QueryRowContext(ctx, sendMessage, id, to.Kind, to.ID, project, gen, from.Kind, from.ID, senderGen, body, ns))
	if err != nil {
		return Message{}, fmt.Errorf("%w: send: %w", ErrStore, err)
	}
	if err := tx.Commit(); err != nil {
		return Message{}, fmt.Errorf("%w: send: %w", ErrStore, err)
	}
	return m, nil
}

// binding is a worker address's live binding, its project and generation;
// any other address has none.
func binding(ctx context.Context, tx *sql.Tx, a Address) (string, sql.NullInt64, error) {
	if a.Kind != KindWorker {
		return "", sql.NullInt64{}, nil
	}
	var project string
	var gen int64
	err := tx.QueryRowContext(ctx, "SELECT project, generation FROM workers WHERE logical_id = ? AND retired_at IS NULL", a.ID).Scan(&project, &gen)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.NullInt64{}, fmt.Errorf("%w: %s", ErrNotBound, a.ID)
	}
	if err != nil {
		return "", sql.NullInt64{}, fmt.Errorf("%w: read worker %s: %w", ErrStore, a.ID, err)
	}
	return project, sql.NullInt64{Int64: gen, Valid: true}, nil
}

// claimNext is one statement, so it is atomic: the oldest queued message to
// a recipient with none dispatching, and, for a worker, only while the
// binding it was sent to is live, neither lost (no pane is proven to type
// into, A35) nor starting (no agent is ready yet), and no handoff holds its
// inbox.
const claimNext = `UPDATE messages SET state = 'dispatching', attempts = attempts + 1, updated_at = max(?3, created_at)
WHERE seq = (
	SELECT m.seq FROM messages AS m
	WHERE m.recipient_kind = ?1 AND m.recipient = ?2 AND m.state = 'queued'
		AND NOT EXISTS (SELECT 1 FROM messages AS d WHERE d.recipient_kind = ?1 AND d.recipient = ?2 AND d.state = 'dispatching')
		AND (?1 <> 'worker' OR EXISTS (
			SELECT 1 FROM workers AS w
			WHERE w.logical_id = ?2 AND w.retired_at IS NULL AND w.generation = m.generation
				AND w.status NOT IN ('lost', 'starting')
				AND NOT EXISTS (SELECT 1 FROM handoffs AS h WHERE h.logical_id = ?2 AND h.state = 'pending')))
	ORDER BY m.seq LIMIT 1)
RETURNING ` + messageColumns

// Claim takes the oldest queued message to `to` for dispatch: it moves it
// to dispatching and counts the attempt. It reports false when there is
// none to take now: the recipient has a message in flight already (one at a
// time, in the order queued), or, for a worker, the binding the message was
// sent to has ended (ReturnToBoss takes those), is lost or still starting,
// or a pending handoff holds the inbox (I3).
func (s *Store) Claim(ctx context.Context, to Address, at time.Time) (Message, bool, error) {
	if err := checkAddress(to, true); err != nil {
		return Message{}, false, err
	}
	ns, err := nanos(at)
	if err != nil {
		return Message{}, false, err
	}
	m, err := scanMessage(s.db.QueryRowContext(ctx, claimNext, to.Kind, to.ID, ns))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("%w: claim: %w", ErrStore, err)
	}
	return m, true, nil
}

// Delivered records that the dispatch of id landed.
func (s *Store) Delivered(ctx context.Context, id string, at time.Time) error {
	return s.settle(ctx, id, at, "deliver",
		"UPDATE messages SET state = 'delivered', updated_at = max(?2, created_at) WHERE id = ?1 AND state = 'dispatching'")
}

// Requeue puts id back in the queue after a dispatch that certainly sent
// nothing, such as a refusal before herdr wrote a byte: the next claim tries
// again, and reason says why this one did not.
func (s *Store) Requeue(ctx context.Context, id, reason string, at time.Time) error {
	return s.settle(ctx, id, at, "requeue",
		"UPDATE messages SET state = 'queued', updated_at = max(?2, created_at), reason = ?3 WHERE id = ?1 AND state = 'dispatching'",
		cleanReason(reason))
}

// settle moves the dispatching message id with query, whose first two
// parameters are the id and the time. ErrNoMessage means id is not
// dispatching.
func (s *Store) settle(ctx context.Context, id string, at time.Time, what, query string, args ...any) error {
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, query, append([]any{id, ns}, args...)...)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrStore, what, id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("%w: %s is not dispatching", ErrNoMessage, id)
	}
	return nil
}

// Fail records that the dispatch of id may have reached the pane, or may
// not: a timeout, or a failure after herdr was sent the prompt. It is never
// redelivered, since the pane cannot discard a duplicate, and in the same
// transaction its sender is sent a notice saying so, with the body to
// resend (A2's crash rule). A notice that fails sends none, and nothing is
// sent to the daemon.
func (s *Store) Fail(ctx context.Context, id, reason string, at time.Time) error {
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: fail %s: %w", ErrStore, id, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	if err := fail(ctx, tx, id, cleanReason(reason), at, ns); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: fail %s: %w", ErrStore, id, err)
	}
	return nil
}

// RecoverDispatching fails every message still dispatching, with reason,
// and notifies their senders, in one transaction. A daemon runs it when it
// starts: a row left dispatching is a dispatch a crash cut off, which may
// or may not have reached its pane (A2).
func (s *Store) RecoverDispatching(ctx context.Context, reason string, at time.Time) (int64, error) {
	ns, err := nanos(at)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: recover: %w", ErrStore, err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	ids, err := dispatching(ctx, tx)
	if err != nil {
		return 0, err
	}
	reason = cleanReason(reason)
	for _, id := range ids {
		if err := fail(ctx, tx, id, reason, at, ns); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("%w: recover: %w", ErrStore, err)
	}
	return int64(len(ids)), nil
}

// dispatching lists the messages dispatching, oldest first.
func dispatching(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM messages WHERE state = 'dispatching' ORDER BY seq")
	if err != nil {
		return nil, fmt.Errorf("%w: recover: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%w: recover: %w", ErrStore, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: recover: %w", ErrStore, err)
	}
	return ids, nil
}

const (
	failMessage = `UPDATE messages SET state = 'failed', reason = ?2, updated_at = max(?3, created_at)
WHERE id = ?1 AND state = 'dispatching'
RETURNING recipient, sender_kind, sender, sender_generation, body, notice_of`
	// sendNotice addresses the notice to the sender's binding: a worker's
	// generation is the one it sent from.
	sendNotice = `INSERT INTO messages (id, recipient_kind, recipient, project, generation, sender_kind, sender, body, notice_of, created_at, updated_at)
VALUES (?1, ?2, ?3, CASE ?2 WHEN 'worker' THEN coalesce((SELECT project FROM workers WHERE logical_id = ?3), '') ELSE '' END,
	?4, 'daemon', ?5, ?6, ?7, ?8, ?8)`
	// noticeFormat is what a sender is told: the message's id, its
	// recipient, why, and the body to resend.
	noticeFormat = "[agentisan] message %s to %s may not have been delivered (%s); resend if needed: %s"
)

// fail fails the dispatching message id in tx and queues its notice.
func fail(ctx context.Context, tx *sql.Tx, id, reason string, at time.Time, ns int64) error {
	var (
		to, senderKind, sender, body, noticeOf string
		senderGen                              sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, failMessage, id, reason, ns).Scan(&to, &senderKind, &sender, &senderGen, &body, &noticeOf)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s is not dispatching", ErrNoMessage, id)
	}
	if err != nil {
		return fmt.Errorf("%w: fail %s: %w", ErrStore, id, err)
	}
	if noticeOf != "" || senderKind == KindDaemon {
		return nil
	}
	nid, err := newULID(at, entropy)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, sendNotice, nid, senderKind, sender, senderGen, Daemon.ID,
		fmt.Sprintf(noticeFormat, id, to, reason, body), id, ns); err != nil {
		return fmt.Errorf("%w: notify the sender of %s: %w", ErrStore, id, err)
	}
	return nil
}

// ReturnToBoss gives boss the messages still queued for worker in project
// whose binding has ended: its row is retired, or the id was reused at a
// later generation (D11: undelivered messages return to the boss). They keep
// their ids and order, and record the worker in ReturnedFrom. Messages sent
// to a live binding, any in flight, and those of another project, which go
// to that project's boss, stay.
func (s *Store) ReturnToBoss(ctx context.Context, worker, project string, boss Address, at time.Time) (int64, error) {
	if boss.Kind != KindBoss {
		return 0, fmt.Errorf("%w: messages return to a boss, not a %s", ErrInvalid, boss.Kind)
	}
	if err := errors.Join(checkID(KindWorker, worker), checkID(KindBoss, boss.ID)); err != nil {
		return 0, err
	}
	ns, err := nanos(at)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE messages
SET recipient_kind = 'boss', recipient = ?2, generation = NULL, returned_from = ?1, updated_at = max(?3, created_at)
WHERE recipient_kind = 'worker' AND recipient = ?1 AND project = ?4 AND state = 'queued'
	AND NOT EXISTS (SELECT 1 FROM workers AS w WHERE w.logical_id = ?1 AND w.retired_at IS NULL AND w.generation = messages.generation)`,
		worker, boss.ID, ns, project)
	if err != nil {
		return 0, fmt.Errorf("%w: return %s's messages: %w", ErrStore, worker, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%w: return %s's messages: %w", ErrStore, worker, err)
	}
	return n, nil
}

// QueuedRecipients lists every address with a message still queued, once
// each, the one whose oldest queued message is oldest first: the order a
// dispatcher serves them in.
func (s *Store) QueuedRecipients(ctx context.Context) ([]Address, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT recipient_kind, recipient FROM messages WHERE state = 'queued' GROUP BY recipient_kind, recipient ORDER BY min(seq)")
	if err != nil {
		return nil, fmt.Errorf("%w: queued recipients: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	var out []Address
	for rows.Next() {
		var a Address
		if err := rows.Scan(&a.Kind, &a.ID); err != nil {
			return nil, fmt.Errorf("%w: queued recipients: %w", ErrStore, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: queued recipients: %w", ErrStore, err)
	}
	return out, nil
}

// EndedQueue is a worker with messages queued to a binding that has ended,
// and the project they belong to: whose boss ReturnToBoss gives them to.
type EndedQueue struct {
	Worker  string
	Project string
}

// EndedQueues lists the workers whose queued messages ReturnToBoss would
// take: sent to a binding that is retired, or that a later generation
// replaced. A lost binding has not ended, so its queue stays held.
func (s *Store) EndedQueues(ctx context.Context) ([]EndedQueue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT m.recipient, m.project FROM messages AS m
WHERE m.recipient_kind = 'worker' AND m.state = 'queued'
	AND NOT EXISTS (SELECT 1 FROM workers AS w WHERE w.logical_id = m.recipient AND w.retired_at IS NULL AND w.generation = m.generation)
ORDER BY m.recipient, m.project`)
	if err != nil {
		return nil, fmt.Errorf("%w: ended queues: %w", ErrStore, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	var out []EndedQueue
	for rows.Next() {
		var q EndedQueue
		if err := rows.Scan(&q.Worker, &q.Project); err != nil {
			return nil, fmt.Errorf("%w: ended queues: %w", ErrStore, err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: ended queues: %w", ErrStore, err)
	}
	return out, nil
}

// selectMessage spells messageColumns out: TestSendQueuesForTheBinding
// reads every message back through it and compares it with what Send
// returned, so the two lists cannot drift.
const selectMessage = `SELECT id, recipient_kind, recipient, project, generation, sender_kind, sender, body, state, attempts, reason, notice_of, returned_from, created_at, updated_at
FROM messages WHERE id = ?`

// Message returns the message id, in whatever state. ErrNoMessage means
// there is none.
func (s *Store) Message(ctx context.Context, id string) (Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, selectMessage, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, fmt.Errorf("%w: %s", ErrNoMessage, id)
	}
	if err != nil {
		return Message{}, fmt.Errorf("%w: %w", ErrStore, err)
	}
	return m, nil
}

// PruneMessages deletes the delivered and failed messages whose last change
// is more than MessageRetention before now (A1), and reports how many it
// deleted. A queued or dispatching message is never pruned.
func (s *Store) PruneMessages(ctx context.Context, now time.Time) (int64, error) {
	cutoff, err := nanos(now.Add(-MessageRetention))
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM messages WHERE state IN ('delivered', 'failed') AND updated_at < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("%w: prune messages: %w", ErrStore, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%w: prune messages: %w", ErrStore, err)
	}
	return n, nil
}

// checkAddress refuses an address of no known kind or with a bad id, and,
// for a recipient, the daemon.
func checkAddress(a Address, recipient bool) error {
	switch a.Kind {
	case KindWorker, KindBoss, KindPane:
	case KindDaemon:
		if recipient {
			return fmt.Errorf("%w: nothing is sent to the daemon", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: address kind %q", ErrInvalid, a.Kind)
	}
	return checkID(a.Kind, a.ID)
}

// checkID refuses an id that is empty, longer than maxID, not UTF-8, or
// holds a control or format character, such as a line break or a
// byte-order mark: ids are matched byte for byte, and end up in a prompt
// typed into a pane.
func checkID(kind, id string) error {
	if id == "" || len(id) > maxID || !utf8.ValidString(id) || strings.ContainsFunc(id, invisible) {
		return fmt.Errorf("%w: %s id %q: want 1 to %d bytes of UTF-8 with no control or format character", ErrInvalid, kind, id, maxID)
	}
	return nil
}

func invisible(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

// checkBody refuses a body Send must not queue. The error never repeats it.
func checkBody(body string) error {
	switch {
	case body == "":
		return fmt.Errorf("%w: an empty body", ErrInvalid)
	case len(body) > MaxBody:
		return fmt.Errorf("%w: %d bytes, at most %d", ErrTooLarge, len(body), MaxBody)
	case !utf8.ValidString(body):
		return fmt.Errorf("%w: the body is not UTF-8", ErrInvalid)
	case strings.IndexByte(body, 0) >= 0:
		return fmt.Errorf("%w: the body holds a NUL byte", ErrInvalid)
	}
	return nil
}

// cleanReason is reason as kept: UTF-8, control characters as spaces, cut to
// maxReason bytes on a character boundary.
func cleanReason(reason string) string {
	reason = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(reason, "�"))
	if len(reason) > maxReason {
		reason = reason[:maxReason]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	return reason
}

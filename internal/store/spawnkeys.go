package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// worker spawn's retry keys (ADR-001 D11 and its retry-key amendment, A73;
// NERD-5254): spawn_requests records the request a key was first used for,
// and spawn_request_workers the ids it planned and, once each is bound, the
// generation it was bound at. Dormant: nothing in production reads or writes
// them yet. Their migration is one-way, as every migration is.

// SpawnKeyRetention is how long a retry key is kept, from its first use.
const SpawnKeyRetention = 14 * 24 * time.Hour

// The bounds the schema holds a request to.
const (
	maxSpawnCount  = 64
	maxSpawnModel  = 128
	maxSpawnEffort = 16
)

var (
	// ErrNoSpawnKey means no record has the retry key: it was never used,
	// or its record was dropped or pruned.
	ErrNoSpawnKey = errors.New("no spawn record has that retry key")
	// ErrSpawnKeyExists means a record already has the retry key.
	ErrSpawnKeyExists = errors.New("a spawn record already has that retry key")
	// ErrSpawnKeyChanged means a record is no longer as a write expects: it
	// is done, or its planned id is already bound. Nothing returns it yet.
	ErrSpawnKeyChanged = errors.New("the spawn record changed since it was read")
)

// SpawnKey is a retry key and the request it was first used for, as asked:
// the model, effort and provider as passed ("" for none), and the task
// packet's SHA-256 in lowercase hex ("" for none), never its text (A50).
type SpawnKey struct {
	Key, Project, Group                 string
	Count                               int
	Model, Effort, Provider, TaskDigest string
}

// SpawnAnswer is a request's stored answer: Code "ok" with Result the
// SpawnResult as JSON, or a refusal's Code and Message.
type SpawnAnswer struct {
	Code, Message string
	Result        []byte
}

// SpawnRecord is a retry key's record: its request, running until its
// answer is stored, then done, and the ids it planned.
type SpawnRecord struct {
	SpawnKey
	// State is "running" or "done".
	State string
	// Answer is zero while the record is running.
	Answer SpawnAnswer
	// CreatedAt is when the key was first used, the answer's "begun";
	// FinishedAt is zero while the record is running.
	CreatedAt, FinishedAt time.Time
	// Planned is the planned ids, in planned order.
	Planned []SpawnLink
}

// SpawnLink is one id a request planned, and the worker it binds now.
type SpawnLink struct {
	LogicalID string
	// Bound is false for an id never bound under the key.
	Bound bool
	// Generation is the generation the id was bound at, 0 when not bound.
	Generation int64
	// Row is the workers row now, as WorkerRow reads it, while it is still
	// at Generation; nil when the id is not bound, or was bound again since
	// (superseded).
	Row *Row
	// Briefs counts the messages to the worker at Generation from a boss or
	// a pane, in any state.
	Briefs int
}

// spawnFailed is a failed statement of the retry keys'. It names what was
// being done, never the request: no error of theirs carries its input.
func spawnFailed(what string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrStore, what, err)
}

// checkRetryKey refuses a key that is not 32 lowercase hex characters.
func checkRetryKey(key string) error {
	if len(key) != 32 || !lowerHex(key) {
		return fmt.Errorf("%w: a retry key is 32 lowercase hex characters", ErrInvalid)
	}
	return nil
}

// lowerHex reports whether every byte of s is 0-9 or a-f.
func lowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// lowerLetters reports whether every byte of s is a-z.
func lowerLetters(s string) bool {
	for i := range len(s) {
		if c := s[i]; c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// checkSpawn refuses a request or plan the schema would refuse, naming the
// field and never its value.
func checkSpawn(k SpawnKey, planned []string) error {
	if err := checkRetryKey(k.Key); err != nil {
		return err
	}
	var bad string
	switch {
	case k.Project == "":
		bad = "an empty project"
	case k.Group == "":
		bad = "an empty group"
	case k.Count < 1 || k.Count > maxSpawnCount:
		bad = fmt.Sprintf("a count outside 1 to %d", maxSpawnCount)
	case len(k.Model) > maxSpawnModel:
		bad = fmt.Sprintf("a model over %d bytes", maxSpawnModel)
	case len(k.Effort) > maxSpawnEffort || !lowerLetters(k.Effort):
		bad = fmt.Sprintf("an effort that is not up to %d lowercase letters", maxSpawnEffort)
	case k.Provider != "" && k.Provider != ProviderClaude && k.Provider != ProviderCodex:
		bad = "a provider other than none, " + ProviderClaude + " or " + ProviderCodex
	case k.TaskDigest != "" && (len(k.TaskDigest) != 64 || !lowerHex(k.TaskDigest)):
		bad = "a task digest that is not 64 lowercase hex characters"
	case len(planned) != k.Count:
		bad = "a plan whose length is not the count"
	}
	if bad != "" {
		return fmt.Errorf("%w: a spawn request with %s", ErrInvalid, bad)
	}
	seen := make(map[string]bool, len(planned))
	for i, id := range planned {
		if checkID(KindWorker, id) != nil {
			return fmt.Errorf("%w: planned id %d is not a worker id", ErrInvalid, i+1)
		}
		if seen[id] {
			return fmt.Errorf("%w: planned id %d repeats an earlier one", ErrInvalid, i+1)
		}
		seen[id] = true
	}
	return nil
}

const (
	insertSpawnRequest = `INSERT INTO spawn_requests (retry_key, project, grp, count, model, effort, provider, task_digest, state, created_at)
VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, 'running', ?9)`
	insertSpawnPlanned = `INSERT INTO spawn_request_workers (retry_key, seq, logical_id) VALUES (?1, ?2, ?3)`
	selectSpawnRequest = `SELECT project, grp, count, model, effort, provider, task_digest, state, outcome, message, answer, created_at, finished_at
FROM spawn_requests WHERE retry_key = ?`
	// Each planned id, with its workers row while still at the linked
	// generation, read as WorkerRow reads it, and its briefs: the messages
	// to the worker at that generation from a boss or a pane, in any state.
	// An id never bound joins nothing and counts none.
	selectSpawnLinks = `SELECT l.logical_id, l.bound_id IS NOT NULL, coalesce(l.generation, 0), w.logical_id IS NOT NULL,
	coalesce(w.project, ''), coalesce(w.grp, ''), coalesce(w.pane_id, ''), coalesce(w.terminal_id, ''), coalesce(w.status, ''),
	coalesce(w.created_at, 0), w.retired_at,
	w.routed_at, coalesce(w.routed_role, ''), coalesce(w.routed_provider, ''), coalesce(w.routed_binding, ''),
	coalesce(w.routed_model_id, ''), coalesce(w.routed_effort, ''), w.override_binding, w.override_by, w.override_effort,
	coalesce(w.assignment_digest, ''), coalesce(w.policy_version, ''), coalesce(w.provider, ''),
	(SELECT count(*) FROM messages m WHERE m.recipient_kind = 'worker' AND m.recipient = l.bound_id
		AND m.generation = l.generation AND m.sender_kind IN ('boss', 'pane'))
FROM spawn_request_workers l
LEFT JOIN workers w ON w.logical_id = l.bound_id AND w.generation = l.generation
WHERE l.retry_key = ? ORDER BY l.seq`
	// dropSpawn deletes a running record none of whose planned ids is bound;
	// its planned rows go with it (ON DELETE CASCADE).
	dropSpawn = `DELETE FROM spawn_requests WHERE retry_key = ?1 AND state = 'running'
AND NOT EXISTS (SELECT 1 FROM spawn_request_workers WHERE retry_key = ?1 AND bound_id IS NOT NULL)`
)

// BeginSpawn records k's request, running, and its planned ids in order,
// as of at, in one transaction. A request or plan the schema would refuse
// (a key off the grammar, a field out of bounds, a plan whose length is not
// k.Count, a repeated or empty id) is ErrInvalid. A key some record already
// has is ErrSpawnKeyExists. Either way nothing is written.
func (s *Store) BeginSpawn(ctx context.Context, k SpawnKey, planned []string, at time.Time) error {
	if err := checkSpawn(k, planned); err != nil {
		return err
	}
	ns, err := nanos(at)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return spawnFailed("begin a spawn", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	// The insert itself tells an existing key, never a read before it: a
	// replace could not go unnoticed.
	_, err = tx.ExecContext(ctx, insertSpawnRequest, k.Key, k.Project, k.Group, k.Count, k.Model, k.Effort, k.Provider, k.TaskDigest, ns)
	var e *sqlite.Error
	if errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
		return fmt.Errorf("%w: begin a spawn", ErrSpawnKeyExists)
	}
	if err != nil {
		return spawnFailed("begin a spawn", err)
	}
	for i, id := range planned {
		if _, err := tx.ExecContext(ctx, insertSpawnPlanned, k.Key, i+1, id); err != nil {
			return spawnFailed("record a planned id", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return spawnFailed("begin a spawn", err)
	}
	return nil
}

// SpawnRecord reads key's record and its planned ids, with each one's
// workers row and briefs now, in one transaction. A key off the grammar is
// ErrInvalid, and a key no record has ErrNoSpawnKey.
func (s *Store) SpawnRecord(ctx context.Context, key string) (SpawnRecord, error) {
	if err := checkRetryKey(key); err != nil {
		return SpawnRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SpawnRecord{}, spawnFailed("read a spawn record", err)
	}
	defer tx.Rollback() //nolint:errcheck // a read: nothing to commit
	r := SpawnRecord{SpawnKey: SpawnKey{Key: key}}
	var (
		outcome, message sql.NullString
		created          int64
		finished         sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, selectSpawnRequest, key).Scan(&r.Project, &r.Group, &r.Count, &r.Model, &r.Effort, &r.Provider,
		&r.TaskDigest, &r.State, &outcome, &message, &r.Answer.Result, &created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return SpawnRecord{}, fmt.Errorf("%w: read a spawn record", ErrNoSpawnKey)
	}
	if err != nil {
		return SpawnRecord{}, spawnFailed("read a spawn record", err)
	}
	r.Answer.Code, r.Answer.Message = outcome.String, message.String
	r.CreatedAt = time.Unix(0, created).UTC()
	if finished.Valid {
		r.FinishedAt = time.Unix(0, finished.Int64).UTC()
	}
	if r.Planned, err = spawnLinks(ctx, tx, key); err != nil {
		return SpawnRecord{}, err
	}
	return r, nil
}

// spawnLinks reads key's planned ids, in order, through tx.
func spawnLinks(ctx context.Context, tx *sql.Tx, key string) ([]SpawnLink, error) {
	rows, err := tx.QueryContext(ctx, selectSpawnLinks, key)
	if err != nil {
		return nil, spawnFailed("read a spawn's planned ids", err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports what matters
	out := []SpawnLink{}
	for rows.Next() {
		var (
			l                   SpawnLink
			live                bool
			r                   Row
			created             int64
			retiredAt, routedAt sql.NullInt64
			rt                  Routing
			override            [3]sql.NullString
		)
		if err := rows.Scan(&l.LogicalID, &l.Bound, &l.Generation, &live,
			&r.Project, &r.Group, &r.PaneID, &r.TerminalID, &r.Status, &created, &retiredAt,
			&routedAt, &rt.Role, &rt.Provider, &rt.Binding, &rt.ModelID, &rt.Effort, &override[0], &override[1], &override[2],
			&rt.AssignmentDigest, &rt.PolicyVersion, &r.Provider, &l.Briefs); err != nil {
			return nil, spawnFailed("read a spawn's planned ids", err)
		}
		if live {
			r.LogicalID, r.Generation = l.LogicalID, l.Generation
			r.CreatedAt = time.Unix(0, created)
			if retiredAt.Valid {
				r.RetiredAt = time.Unix(0, retiredAt.Int64)
			}
			if routedAt.Valid {
				rt.RoutedAt = time.Unix(0, routedAt.Int64)
				rt.OverrideBinding, rt.OverrideBy, rt.OverrideEffort = override[0].String, override[1].String, override[2].String
				r.Routing = &rt
			}
			l.Row = &r
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, spawnFailed("read a spawn's planned ids", err)
	}
	return out, nil
}

// DropSpawn deletes key's record, with its planned ids, only while it is
// running and none of them is bound, and reports whether it deleted it. A
// key off the grammar is ErrInvalid.
func (s *Store) DropSpawn(ctx context.Context, key string) (bool, error) {
	if err := checkRetryKey(key); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, dropSpawn, key)
	if err != nil {
		return false, spawnFailed("drop a spawn record", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, spawnFailed("drop a spawn record", err)
	}
	return n == 1, nil
}

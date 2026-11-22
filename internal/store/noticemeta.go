package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The notice ledger's metadata (NERD-5533 B0a.3-queries): what it holds,
// read without changing it. Each count is read with its quota's own query,
// so it is the number that quota compares: the hour is counted per source,
// as Q8 counts it. It is dormant, as the
// ledger is: nothing in production reads it yet.

// NoticeMeta is what the ledger tells about a pending notice without its
// content: never its text and never its receipt key.
type NoticeMeta struct {
	CaptureID  string
	Source     string
	Kind       string
	Subject    string
	Worker     string
	Repo       string    // "" for a notice with no receipt
	PR         int64     // 0 for a notice with no receipt
	ObservedAt time.Time // UTC
	AdmittedAt time.Time // UTC
}

// NoticeStats are the counts the ledger's quotas compare, exact, for a
// project and for the whole store, as of at.
type NoticeStats struct {
	Pending, PendingBytes, Identities                int64 // the project's: Q1, Q4, Q6
	GitHubLastHour, DaemonLastHour                   int64 // the project's Q8, per source
	StorePending, StorePendingBytes, StoreIdentities int64 // the store's: Q2, Q5, Q7
}

// noQuotaLimit is the LIMIT a quota's query takes to read every row: SQLite
// reads a negative LIMIT as none.
const noQuotaLimit = -1

const selectPendingNotices = `SELECT i.capture_id, i.source, i.kind, coalesce(i.repo, ''), coalesce(i.pr, 0), i.admitted_at,
	p.capture_id, p.subject, p.worker, p.observed_at
FROM notice_identities AS i LEFT JOIN notice_payloads AS p ON p.capture_id = i.capture_id
WHERE i.project = ?1 AND i.state = 'pending'
ORDER BY i.admitted_at, i.capture_id
LIMIT ?2`

// PendingNotices lists project's pending notices, oldest admitted first, a
// tie by capture ID, at most limit of them, with both times in UTC. None is
// an empty slice, never nil. project may be any string, "" included; limit
// must be 1 to MaxBoardRead, else it is ErrInvalid. A pending notice whose
// payload is missing is ErrStore: a row edited by hand is never skipped.
// It recomputes no digest, since it hands on no content: PendingNotice
// re-checks a notice before it is told. It writes nothing and wakes no
// reader.
func (s *Store) PendingNotices(ctx context.Context, project string, limit int) ([]NoticeMeta, error) {
	if limit < 1 || limit > MaxBoardRead {
		return nil, fmt.Errorf("%w: a read of %d notices: want 1 to %d", ErrInvalid, limit, MaxBoardRead)
	}
	const listing = "list pending notices"
	rows, err := s.db.QueryContext(ctx, selectPendingNotices, project, limit)
	if err != nil {
		return nil, ledgerFailed(listing, err)
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err() reports a failed read
	out := []NoticeMeta{}
	for rows.Next() {
		var (
			m                       NoticeMeta
			admitted                int64
			payload, subject, actor sql.NullString
			observed                sql.NullInt64
		)
		if err := rows.Scan(&m.CaptureID, &m.Source, &m.Kind, &m.Repo, &m.PR, &admitted, &payload, &subject, &actor, &observed); err != nil {
			return nil, ledgerFailed(listing, err)
		}
		if !payload.Valid {
			return nil, fmt.Errorf("%w: a pending notice has no payload", ErrStore)
		}
		m.Subject, m.Worker = subject.String, actor.String
		m.ObservedAt, m.AdmittedAt = time.Unix(0, observed.Int64).UTC(), time.Unix(0, admitted).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, ledgerFailed(listing, err)
	}
	return out, nil
}

// NoticeStats is the counts project's and the store's quotas compare as of
// at, each exact, never capped at its quota's limit, all read in one
// snapshot. A count quota refuses exactly when its count reaches its limit,
// a byte quota when the count plus the next notice's bytes would pass it.
// The hour is counted per source, GitHub and daemon, as Q8 counts it. at
// must be a time the store can hold, else it is ErrInvalid; a total 0010
// keeps that is missing is ErrStore. Its cost is not bounded by the quotas' caps: it is
// off the admission path. It writes nothing and wakes no reader.
func (s *Store) NoticeStats(ctx context.Context, project string, at time.Time) (NoticeStats, error) {
	ns, err := nanos(at)
	if err != nil {
		return NoticeStats{}, noticeInvalid() // nanos would format at
	}
	const reading = "read the notice stats"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NoticeStats{}, ledgerFailed(reading, err)
	}
	defer tx.Rollback() //nolint:errcheck // reads only
	var st NoticeStats
	since := rateWindowStart(ns)
	for _, read := range []struct {
		n     *int64
		query string
		args  []any
	}{
		{&st.Pending, selectPendingPerProject, []any{project, noQuotaLimit}},
		{&st.StorePending, selectPendingInStore, []any{noQuotaLimit}},
		{&st.PendingBytes, selectBytesPerProject, []any{project, noQuotaLimit}},
		{&st.StorePendingBytes, selectBytesInStore, []any{noQuotaLimit}},
		{&st.GitHubLastHour, selectSourceAdmissionsInWindow, []any{project, since, noQuotaLimit, noticeFromGitHub}},
		{&st.DaemonLastHour, selectSourceAdmissionsInWindow, []any{project, since, noQuotaLimit, noticeFromDaemon}},
	} {
		if *read.n, err = readQuota(ctx, tx, read.query, read.args...); err != nil {
			return NoticeStats{}, err
		}
	}
	// Q6's rule: a project with no total and no identity has none.
	var (
		total  sql.NullInt64
		exists bool
	)
	if err := tx.QueryRowContext(ctx, selectProjectIdentities, project).Scan(&total, &exists); err != nil {
		return NoticeStats{}, ledgerFailed(reading, err)
	}
	if !total.Valid && exists {
		return NoticeStats{}, noticeTotalMissing()
	}
	st.Identities = total.Int64
	switch err := tx.QueryRowContext(ctx, selectStoreIdentities).Scan(&st.StoreIdentities); {
	case errors.Is(err, sql.ErrNoRows):
		return NoticeStats{}, noticeTotalMissing()
	case err != nil:
		return NoticeStats{}, ledgerFailed(reading, err)
	}
	return st, nil
}

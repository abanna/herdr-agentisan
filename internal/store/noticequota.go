package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// The notice ledger's quotas (NERD-5533 B0a.3-limits). An admission that
// would pass one is refused, never made room for: nothing already admitted
// is evicted. They are the store's own, not configuration: open sets the
// defaults, and only tests change them, through export_test.go. They are a
// field of each Store, not a package variable, so tests in parallel each
// set their own. A limit of 0 refuses every admission its quota applies to.
//
// A pending admission faces Q1 to Q8, an import (a notice github_seen
// already holds) Q6 to Q8 only, and a duplicate or an invalid envelope
// none: AdmitNotice returns both before the quotas run. A daemon notice
// (B0a.4) is pending and has no pull request, so it faces every quota but
// Q3; a ctx_ceiling skips Q8 (B0a.4-hour). A count quota refuses when count >= limit, a byte quota when the
// pending bytes plus this notice's would pass its limit.
type noticeQuotas struct {
	PendingPerProject, PendingInStore, PendingPerPR int64
	BytesPerProject, BytesInStore                   int64
	IdentitiesPerProject, IdentitiesInStore         int64
	AdmissionsPerHour                               int64
}

var defaultNoticeQuotas = noticeQuotas{
	PendingPerProject: 1000, PendingInStore: 5000, PendingPerPR: 32,
	BytesPerProject: 8 << 20, BytesInStore: 32 << 20, IdentitiesPerProject: 100_000,
	IdentitiesInStore: 500_000, AdmissionsPerHour: 600,
}

// noticeRateWindow is Q8's window, bounded below only: an admission stamped
// after the one being checked counts too.
const noticeRateWindow = time.Hour

// ErrNoticeQuota means an admission would pass one of the notice ledger's
// quotas. Each quota's own error wraps it; nothing was written. Which quota
// is reported when several are full is not part of the contract: a caller
// branches on ErrNoticeQuota.
var ErrNoticeQuota = errors.New("notice ledger quota reached")

// The quotas' own errors, Q1 to Q8, each wrapping ErrNoticeQuota.
var (
	// ErrNoticePendingPerProject is Q1: a project's pending notices.
	ErrNoticePendingPerProject = fmt.Errorf("%w: pending notices per project", ErrNoticeQuota)
	// ErrNoticePendingInStore is Q2: the store's pending notices.
	ErrNoticePendingInStore = fmt.Errorf("%w: pending notices in the store", ErrNoticeQuota)
	// ErrNoticePendingPerPR is Q3: a pull request's pending notices.
	ErrNoticePendingPerPR = fmt.Errorf("%w: pending notices per PR", ErrNoticeQuota)
	// ErrNoticeBytesPerProject is Q4: the bytes of a project's pending notices.
	ErrNoticeBytesPerProject = fmt.Errorf("%w: pending notice bytes per project", ErrNoticeQuota)
	// ErrNoticeBytesInStore is Q5: the bytes of the store's pending notices.
	ErrNoticeBytesInStore = fmt.Errorf("%w: pending notice bytes in the store", ErrNoticeQuota)
	// ErrNoticeIdentitiesPerProject is Q6: a project's notices, every state.
	ErrNoticeIdentitiesPerProject = fmt.Errorf("%w: notices per project", ErrNoticeQuota)
	// ErrNoticeIdentitiesInStore is Q7: the store's notices, every state.
	ErrNoticeIdentitiesInStore = fmt.Errorf("%w: notices in the store", ErrNoticeQuota)
	// ErrNoticeAdmissionRate is Q8: a project's admissions of one source in
	// the last hour.
	ErrNoticeAdmissionRate = fmt.Errorf("%w: notices of one source admitted per project in an hour", ErrNoticeQuota)
)

// The quotas' queries, run on the admission's transaction. Each walk stops
// at its LIMIT, a bound parameter, so each check reads at most its cap's
// rows, however large the ledger grows, except Q8, which reads at most the
// project's hour of both sources, about 1,200 rows, and names its index
// (INDEXED BY notice_identities_by_state) so that no index without the
// project can take it over. Q4 and Q5 walk at most Q1's and
// Q2's caps: their LIMIT cuts rows only when Q1 or Q2 refuses anyway.
// Q6 and Q7 read 0010's totals, one row each.
const (
	selectPendingPerProject = `SELECT count(*) FROM (SELECT 1 FROM notice_identities WHERE project = ?1 AND state = 'pending' LIMIT ?2)`
	selectPendingInStore    = `SELECT count(*) FROM (SELECT 1 FROM notice_identities WHERE state = 'pending' LIMIT ?1)`
	selectPendingPerPR      = `SELECT count(*) FROM (SELECT 1 FROM notice_identities WHERE project = ?1 AND repo = ?2 AND pr = ?3 AND state = 'pending' LIMIT ?4)`
	selectBytesPerProject   = `SELECT coalesce(sum(p.bytes), 0) FROM (SELECT capture_id FROM notice_identities WHERE project = ?1 AND state = 'pending' LIMIT ?2) AS i JOIN notice_payloads AS p USING (capture_id)`
	selectBytesInStore      = `SELECT coalesce(sum(p.bytes), 0) FROM (SELECT capture_id FROM notice_identities WHERE state = 'pending' LIMIT ?1) AS i JOIN notice_payloads AS p USING (capture_id)`
	// Q6: the project's total, and whether it has any identity, so a
	// missing total is told from a project with none.
	selectProjectIdentities = `SELECT (SELECT identities FROM notice_counts WHERE project = ?1), EXISTS (SELECT 1 FROM notice_identities WHERE project = ?1)`
	selectStoreIdentities   = `SELECT identities FROM notice_store_counts WHERE id = 1`
	// selectSourceAdmissionsInWindow is Q8: a project's admissions of one
	// source, in every state, at or after ?2. It names its index, which leads
	// with the project, so an index without one (the prune's by_source) never
	// takes it over and walks that source's hour across every project
	// (B0a.4-hour).
	selectSourceAdmissionsInWindow = `SELECT count(*) FROM (SELECT 1 FROM notice_identities INDEXED BY notice_identities_by_state
  WHERE project = ?1 AND source = ?4 AND state IN ('pending', 'imported', 'posted', 'dropped')
    AND admitted_at >= ?2 LIMIT ?3)`
)

// checkingQuotas is what a failed quota query was doing.
const checkingQuotas = "check a notice quota"

// noticeTotalMissing is a total 0010 keeps that is not there: only a hand
// edit removes one. The admission fails closed; the recount in
// docs/architecture.md repairs it.
func noticeTotalMissing() error {
	return fmt.Errorf("%w: a notice ledger total is missing", ErrStore)
}

// quotaRefused is quota's refusal at limit. It names the quota and its
// limit only, never the notice.
func quotaRefused(quota error, limit int64) error {
	return fmt.Errorf("%w (limit %d)", quota, limit)
}

// readQuota runs one quota's query on tx: a count, or a sum of bytes.
func readQuota(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	var n int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, ledgerFailed(checkingQuotas, err)
	}
	return n, nil
}

// checkNoticeQuotas checks a notice of project, from source and of kind,
// on r's pull request if r is not nil (a daemon notice has none), whose
// canonical encoding is bytes long, admitted at ns, against q, on the
// admission's transaction tx, and returns the first quota it would pass, Q1
// to Q8 in order. A pending notice faces all eight, an import Q6 to Q8. Q8
// counts the notice's source only, and a daemon context ceiling skips it
// (B0a.4-hour). Writes are serialized on the store's one connection, so
// nothing overtakes a count between it and the insert after it.
func checkNoticeQuotas(ctx context.Context, tx *sql.Tx, q noticeQuotas, project, source, kind string,
	r *NoticeReceipt, bytes int64, pending bool, ns int64,
) error {
	if pending {
		if err := checkPendingQuotas(ctx, tx, q, project, r, bytes); err != nil {
			return err
		}
	}
	var (
		total  sql.NullInt64
		exists bool
	)
	if err := tx.QueryRowContext(ctx, selectProjectIdentities, project).Scan(&total, &exists); err != nil {
		return ledgerFailed(checkingQuotas, err)
	}
	if !total.Valid && exists {
		return noticeTotalMissing()
	}
	if total.Int64 >= q.IdentitiesPerProject {
		return quotaRefused(ErrNoticeIdentitiesPerProject, q.IdentitiesPerProject)
	}
	var all int64
	switch err := tx.QueryRowContext(ctx, selectStoreIdentities).Scan(&all); {
	case errors.Is(err, sql.ErrNoRows):
		return noticeTotalMissing()
	case err != nil:
		return ledgerFailed(checkingQuotas, err)
	}
	if all >= q.IdentitiesInStore {
		return quotaRefused(ErrNoticeIdentitiesInStore, q.IdentitiesInStore)
	}
	if source == noticeFromDaemon && kind == noticeKindCeiling {
		return nil
	}
	n, err := readQuota(ctx, tx, selectSourceAdmissionsInWindow, project, rateWindowStart(ns), q.AdmissionsPerHour, source)
	if err != nil {
		return err
	}
	if n >= q.AdmissionsPerHour {
		return quotaRefused(ErrNoticeAdmissionRate, q.AdmissionsPerHour)
	}
	return nil
}

// checkPendingQuotas checks Q1 to Q5, the quotas only a pending notice
// faces, Q3 only for a notice with a receipt r: it counts r's pull
// request's. A byte quota compares the pending sum with its limit less this
// notice's bytes, which a canonical encoding's length keeps small, so a sum
// a hand edit has made as large as int64 holds refuses instead of wrapping
// past it and admitting.
func checkPendingQuotas(ctx context.Context, tx *sql.Tx, q noticeQuotas, project string, r *NoticeReceipt, bytes int64) error {
	n, err := readQuota(ctx, tx, selectPendingPerProject, project, q.PendingPerProject)
	if err != nil {
		return err
	}
	if n >= q.PendingPerProject {
		return quotaRefused(ErrNoticePendingPerProject, q.PendingPerProject)
	}
	if n, err = readQuota(ctx, tx, selectPendingInStore, q.PendingInStore); err != nil {
		return err
	}
	if n >= q.PendingInStore {
		return quotaRefused(ErrNoticePendingInStore, q.PendingInStore)
	}
	if r != nil {
		if n, err = readQuota(ctx, tx, selectPendingPerPR, project, r.Repo, r.PR, q.PendingPerPR); err != nil {
			return err
		}
		if n >= q.PendingPerPR {
			return quotaRefused(ErrNoticePendingPerPR, q.PendingPerPR)
		}
	}
	if n, err = readQuota(ctx, tx, selectBytesPerProject, project, q.PendingPerProject); err != nil {
		return err
	}
	if n > q.BytesPerProject-bytes {
		return quotaRefused(ErrNoticeBytesPerProject, q.BytesPerProject)
	}
	if n, err = readQuota(ctx, tx, selectBytesInStore, q.PendingInStore); err != nil {
		return err
	}
	if n > q.BytesInStore-bytes {
		return quotaRefused(ErrNoticeBytesInStore, q.BytesInStore)
	}
	return nil
}

// rateWindowStart is the oldest admitted_at Q8 counts at ns: an admission
// exactly one window old is out, one a nanosecond younger is in. It
// saturates at math.MinInt64, the oldest time the store holds, where the
// subtraction would wrap.
func rateWindowStart(ns int64) int64 {
	if ns < math.MinInt64+int64(noticeRateWindow)-1 {
		return math.MinInt64
	}
	return ns - int64(noticeRateWindow) + 1
}

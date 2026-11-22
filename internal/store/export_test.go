package store

import (
	"context"
	"database/sql"
	"io"
	"io/fs"
	"time"
)

// DB returns the store's connection, so a test can check the schema's own
// constraints on rows the store's methods do not write yet.
func DB(s *Store) *sql.DB { return s.db }

// SetCloseWait sets how long Close waits for the store's connection.
func SetCloseWait(s *Store, d time.Duration) { s.closeWait = d }

// OpenWith opens path migrated by the numbered SQL files at the root of
// migrations instead of the embedded ones.
func OpenWith(ctx context.Context, path string, migrations fs.FS) (*Store, error) {
	return open(ctx, path, migrations)
}

// VerifyPragmas checks the pragmas Open relies on against db's connection.
func VerifyPragmas(ctx context.Context, db *sql.DB) error { return verifyPragmas(ctx, db) }

// NoticeQuotas are the notice ledger's quotas, Q1 to Q8.
type NoticeQuotas = noticeQuotas

// SetNoticeQuotas sets s's quotas, before s is used.
func SetNoticeQuotas(s *Store, q NoticeQuotas) { s.quotas = q }

// NoticeQuotasOf is s's quotas.
func NoticeQuotasOf(s *Store) NoticeQuotas { return s.quotas }

// NoticeRateWindow is Q8's window.
const NoticeRateWindow = noticeRateWindow

// NoticeKindCeiling is the daemon's context ceiling kind, which Q8 never
// refuses: C2 pins it to notice.CtxCeiling.
const NoticeKindCeiling = noticeKindCeiling

// NoticeQuotaQueries are the quotas' queries, by quota.
var NoticeQuotaQueries = map[string]string{
	"Q1": selectPendingPerProject, "Q2": selectPendingInStore, "Q3": selectPendingPerPR,
	"Q4": selectBytesPerProject, "Q5": selectBytesInStore,
	"Q6": selectProjectIdentities, "Q7": selectStoreIdentities, "Q8": selectSourceAdmissionsInWindow,
}

// SetNoticeEntropy sets the reader s draws its daemon notices' IDs' random
// bits from, before s is used.
func SetNoticeEntropy(s *Store, r io.Reader) { s.noticeEntropy = r }

// NoticeEncoding is e's nt1 layout, unchecked, for a test's hand-made rows
// only: the vectors' bytes are the test's own literals.
func NoticeEncoding(e NoticeEnvelope) []byte { return nt1Layout(e) }

// NoticeBytes is the size the ledger stores for e: its canonical
// encoding's length, which Q4 and Q5 sum.
func NoticeBytes(e NoticeEnvelope) (int64, error) {
	c, err := captureOf(e)
	return c.bytes, err
}

// InUTC is r with its times in UTC, for comparing with a test's literals.
func (r Row) InUTC() Row {
	r.CreatedAt = r.CreatedAt.UTC()
	if !r.RetiredAt.IsZero() {
		r.RetiredAt = r.RetiredAt.UTC()
	}
	if r.Routing != nil {
		rt := *r.Routing
		rt.RoutedAt = rt.RoutedAt.UTC()
		r.Routing = &rt
	}
	return r
}

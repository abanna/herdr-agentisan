package store_test

import (
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// quotaErrs are the quotas' errors, Q1 to Q8: quotaErrs[n-1] is Qn's.
var quotaErrs = []error{
	store.ErrNoticePendingPerProject, store.ErrNoticePendingInStore, store.ErrNoticePendingPerPR,
	store.ErrNoticeBytesPerProject, store.ErrNoticeBytesInStore,
	store.ErrNoticeIdentitiesPerProject, store.ErrNoticeIdentitiesInStore, store.ErrNoticeAdmissionRate,
}

// quotaFixture admits GitHub notices into one fresh store, each with a key
// of its own, so no two share a receipt.
type quotaFixture struct {
	t    *testing.T
	s    *store.Store
	keys int
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()
	return &quotaFixture{t: t, s: open(t, dbPath(t))}
}

// set changes the store's quotas from what they are now.
func (f *quotaFixture) set(change func(*store.NoticeQuotas)) {
	q := store.NoticeQuotasOf(f.s)
	change(&q)
	store.SetNoticeQuotas(f.s, q)
}

// notice is a new valid notice of project on PR pr of ledgerRepo.
func (f *quotaFixture) notice(project string, pr int64, change ...func(*store.NoticeEnvelope)) store.NoticeEnvelope {
	f.t.Helper()
	f.keys++
	key := fmt.Sprintf("%040x", f.keys)
	return admissible(f.t, append([]func(*store.NoticeEnvelope){func(e *store.NoticeEnvelope) {
		e.Project, e.Receipt.PR, e.Receipt.Key = project, pr, key
	}}, change...)...)
}

// admitAt admits e at at, which must be accepted, in the state want.
func (f *quotaFixture) admitAt(e store.NoticeEnvelope, at time.Time, want string) store.NoticeEnvelope {
	f.t.Helper()
	got, err := f.s.AdmitNotice(f.t.Context(), e, at)
	require.NoError(f.t, err)
	require.Equal(f.t, store.NoticeAdmission{CaptureID: e.CaptureID, State: want, Imported: want == "imported"}, got)
	return e
}

// pending admits a new notice of project on pr, pending.
func (f *quotaFixture) pending(project string, pr int64) store.NoticeEnvelope {
	f.t.Helper()
	return f.admitAt(f.notice(project, pr), ledgerAt, "pending")
}

// imported admits a new notice of project on pr that github_seen holds.
func (f *quotaFixture) imported(project string, pr int64) store.NoticeEnvelope {
	f.t.Helper()
	e := f.notice(project, pr)
	seen(f.t, f.s, project, store.GitHubEvent{PR: int(pr), Kind: e.Kind, Key: e.Receipt.Key})
	return f.admitAt(e, ledgerAt, "imported")
}

// settled admits a new notice of project on pr, then settles it as state,
// posted or dropped, as a later slice will: its payload stays stored.
func (f *quotaFixture) settled(project string, pr int64, state string) store.NoticeEnvelope {
	f.t.Helper()
	e := f.pending(project, pr)
	settle := map[string]string{
		"posted":  "UPDATE notice_identities SET state = 'posted', settled_at = ?, post_seq = 1 WHERE capture_id = ?",
		"dropped": "UPDATE notice_identities SET state = 'dropped', settled_at = ?, drop_reason = 'skipped' WHERE capture_id = ?",
	}[state]
	res, err := store.DB(f.s).ExecContext(f.t.Context(), settle, ledgerAt.UnixNano(), e.CaptureID)
	require.NoError(f.t, err)
	n, err := res.RowsAffected()
	require.NoError(f.t, err)
	require.Equal(f.t, int64(1), n, "settled as %s", state)
	return e
}

// otherStates gives project's PR pr an imported row, a posted row and a
// dropped row, the last two with their payloads still stored (M3): none of
// them may count against Q1 to Q5.
func (f *quotaFixture) otherStates(project string, pr int64) {
	f.t.Helper()
	f.imported(project, pr)
	f.settled(project, pr, "posted")
	f.settled(project, pr, "dropped")
}

// ledgerSnapshot is every row of the ledger's four tables, the totals
// included.
func ledgerSnapshot(t *testing.T, s *store.Store) [][][]any {
	t.Helper()
	db := store.DB(s)
	return [][][]any{
		queryRows(t, db, "SELECT * FROM notice_identities ORDER BY rowid"),
		queryRows(t, db, "SELECT * FROM notice_payloads ORDER BY rowid"),
		queryRows(t, db, "SELECT * FROM notice_counts ORDER BY rowid"),
		queryRows(t, db, "SELECT * FROM notice_store_counts ORDER BY rowid"),
	}
}

// quotaLimit is Qquota's limit in q.
func quotaLimit(q store.NoticeQuotas, quota int) int64 {
	return []int64{
		q.PendingPerProject, q.PendingInStore, q.PendingPerPR, q.BytesPerProject, q.BytesInStore,
		q.IdentitiesPerProject, q.IdentitiesInStore, q.AdmissionsPerHour,
	}[quota-1]
}

// refused admits e at at, which Qquota must refuse with its own error, one
// that wraps ErrNoticeQuota and no other quota's and names the limit it
// refused at, writing nothing.
func (f *quotaFixture) refused(e store.NoticeEnvelope, at time.Time, quota int) {
	f.t.Helper()
	before := ledgerSnapshot(f.t, f.s)
	got, err := f.s.AdmitNotice(f.t.Context(), e, at)
	require.ErrorIs(f.t, err, quotaErrs[quota-1], "Q%d refuses", quota)
	f.t.Logf("Q%d refuses the next: %v", quota, err)
	assert.EqualError(f.t, err, fmt.Sprintf("%v (limit %d)", quotaErrs[quota-1], quotaLimit(store.NoticeQuotasOf(f.s), quota)))
	assert.ErrorIs(f.t, err, store.ErrNoticeQuota)
	for i, other := range quotaErrs {
		if i != quota-1 {
			assert.NotErrorIs(f.t, err, other, "Q%d's refusal is not Q%d's", quota, i+1)
		}
	}
	assert.NotErrorIs(f.t, err, store.ErrInvalid)
	assert.NotErrorIs(f.t, err, store.ErrStore)
	assert.Equal(f.t, store.NoticeAdmission{}, got)
	assert.Equal(f.t, before, ledgerSnapshot(f.t, f.s), "a refusal writes nothing, the totals included")
	assertNoticeTotals(f.t, store.DB(f.s))
}

// bytesOf is the size the ledger stores for each of es, summed.
func bytesOf(t *testing.T, es ...store.NoticeEnvelope) int64 {
	t.Helper()
	var sum int64
	for _, e := range es {
		n, err := store.NoticeBytes(e)
		require.NoError(t, err)
		sum += n
	}
	return sum
}

// TestNoticeQuotaDefaults (QD): an open store has the eight quotas the user
// accepted, and Q8's window is one hour.
func TestNoticeQuotaDefaults(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	assert.Equal(t, store.NoticeQuotas{
		PendingPerProject: 1000, PendingInStore: 5000, PendingPerPR: 32,
		BytesPerProject: 8 << 20, BytesInStore: 32 << 20,
		IdentitiesPerProject: 100_000, IdentitiesInStore: 500_000,
		AdmissionsPerHour: 600,
	}, store.NoticeQuotasOf(s))
	assert.Equal(t, time.Hour, store.NoticeRateWindow)
}

// TestQuotaBoundaries (Q1 to Q8): each quota, set small with the others at
// their defaults, admits up to its cap and refuses past it with its own
// error, counts only what it names and only in its own scope, and writes
// nothing when it refuses.
func TestQuotaBoundaries(t *testing.T) {
	t.Parallel()
	t.Run("Q1", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.otherStates("A", 50)
		for range 3 {
			f.pending("B", 50)
		}
		f.set(func(q *store.NoticeQuotas) { q.PendingPerProject = 3 })
		f.pending("A", 50)
		f.pending("A", 51)
		f.pending("A", 52)
		t.Log("Q1 = 3: the 3rd pending notice of project A is admitted, beside a full project B and A's imported, posted and dropped rows")
		f.refused(f.notice("A", 53), ledgerAt, 1)
	})
	t.Run("Q2", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.otherStates("A", 50)
		f.otherStates("B", 50)
		f.set(func(q *store.NoticeQuotas) { q.PendingInStore = 4 })
		f.pending("A", 50)
		f.pending("A", 51)
		f.pending("B", 50)
		f.pending("B", 51)
		t.Log("Q2 = 4: the 4th pending notice in the store, B's 2nd, is admitted beside A's and B's imported, posted and dropped rows")
		f.refused(f.notice("C", 50), ledgerAt, 2)
	})
	t.Run("Q3", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.otherStates("A", 50)
		f.set(func(q *store.NoticeQuotas) { q.PendingPerPR = 2 })
		f.pending("A", 50)
		f.pending("A", 50)
		t.Log("Q3 = 2: the 2nd pending notice on PR 50 is admitted beside the PR's imported, posted and dropped rows")
		f.refused(f.notice("A", 50), ledgerAt, 3)
		f.admitAt(f.notice("A", 50, func(e *store.NoticeEnvelope) { e.Receipt.Repo = "abanna/other" }), ledgerAt, "pending")
		f.pending("A", 51)
		t.Log("Q3: PR 50 of another repo, and PR 51 of the same repo, are admitted")
	})
	t.Run("Q4", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.otherStates("A", 50)
		f.pending("B", 50)
		first, second, third := f.pending("A", 50), f.pending("A", 51), f.notice("A", 52)
		limit := bytesOf(t, first, second, third)
		f.set(func(q *store.NoticeQuotas) { q.BytesPerProject = limit - 1 })
		f.refused(third, ledgerAt, 4)
		f.set(func(q *store.NoticeQuotas) { q.BytesPerProject = limit })
		f.admitAt(third, ledgerAt, "pending")
		t.Logf("Q4 = %d: the 3rd pending notice of project A is admitted at exactly the limit, one byte lower it is refused", limit)
		f.refused(f.notice("A", 53), ledgerAt, 4)
	})
	t.Run("Q5", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.otherStates("A", 50)
		f.otherStates("B", 50)
		a, b, c := f.pending("A", 50), f.pending("B", 50), f.notice("C", 50)
		limit := bytesOf(t, a, b, c)
		f.set(func(q *store.NoticeQuotas) { q.BytesInStore = limit - 1 })
		f.refused(c, ledgerAt, 5)
		f.set(func(q *store.NoticeQuotas) { q.BytesInStore = limit })
		f.admitAt(c, ledgerAt, "pending")
		t.Logf("Q5 = %d: project C's pending notice is admitted at exactly the store's limit, one byte lower it is refused", limit)
		f.refused(f.notice("C", 51), ledgerAt, 5)
	})
	t.Run("Q6", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.set(func(q *store.NoticeQuotas) { q.IdentitiesPerProject = 3 })
		f.imported("A", 50)
		f.imported("A", 51)
		f.imported("A", 52)
		t.Log("Q6 = 3: the 3rd notice of project A, an import, is admitted; none is pending")
		e := f.notice("A", 53)
		seen(t, f.s, "A", store.GitHubEvent{PR: 53, Kind: e.Kind, Key: e.Receipt.Key})
		f.refused(e, ledgerAt, 6)
		f.refused(f.notice("A", 54), ledgerAt, 6)
		f.pending("B", 50)
		t.Log("Q6: project B is admitted")
	})
	t.Run("Q7", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.set(func(q *store.NoticeQuotas) { q.IdentitiesInStore = 4 })
		f.imported("A", 50)
		f.pending("A", 51)
		f.imported("B", 50)
		f.pending("B", 51)
		t.Log("Q7 = 4: the 4th notice in the store, B's 2nd, is admitted")
		f.refused(f.notice("C", 50), ledgerAt, 7)
	})
	t.Run("Q8", func(t *testing.T) {
		t.Parallel()
		f := newQuotaFixture(t)
		f.admitAt(f.notice("A", 50), ledgerAt.Add(-2*time.Hour), "pending")
		f.admitAt(f.notice("A", 51), ledgerAt.Add(-store.NoticeRateWindow), "pending")
		for pr := range int64(3) {
			f.pending("B", 50+pr)
		}
		f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 3 })
		f.pending("A", 52)
		f.pending("A", 53)
		f.pending("A", 54)
		t.Log("Q8 = 3: project A's 3rd admission within the hour is admitted, beside B's three and A's two older ones")
		f.refused(f.notice("A", 55), ledgerAt, 8)
	})
}

// TestQuotasCountImportsOnlyAsIdentities (Q-import): an import faces only
// Q6 to Q8. With Q1 to Q5 all full it is admitted, and it counts against
// Q6, Q7 and Q8; at a full Q6 it is refused. github_seen is only read.
func TestQuotasCountImportsOnlyAsIdentities(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	db := store.DB(f.s)
	first := f.pending("A", 50)
	size := bytesOf(t, first)
	f.set(func(q *store.NoticeQuotas) {
		q.PendingPerProject, q.PendingInStore, q.PendingPerPR = 1, 1, 1
		q.BytesPerProject, q.BytesInStore = size, size
	})
	_, err := f.s.AdmitNotice(t.Context(), f.notice("A", 50), ledgerAt)
	require.ErrorIs(t, err, store.ErrNoticeQuota, "Q1 to Q5 are full")

	f.imported("A", 50)
	projects, total := noticeTotals(t, db)
	assert.Equal(t, map[string]int64{"A": 2}, projects, "the import moves Q6's total")
	assert.Equal(t, int64(2), total, "the import moves Q7's total")
	f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 2 })
	e := f.notice("A", 50)
	seen(t, f.s, "A", store.GitHubEvent{PR: 50, Kind: e.Kind, Key: e.Receipt.Key})
	f.refused(e, ledgerAt, 8)
	t.Log("the import counts against Q8: two admissions in the hour fill Q8 = 2")

	f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour, q.IdentitiesPerProject = 600, 2 })
	seenBefore := tableRows(t, db, "github_seen")
	f.refused(e, ledgerAt, 6)
	assert.Equal(t, seenBefore, tableRows(t, db, "github_seen"), "github_seen is unchanged, last_seen included")
}

// TestDuplicateWinsOverAFullQuota (Q-dup): a duplicate is told before any
// quota runs, so at a full quota the same envelope, and the same receipt
// with new text or a new time, are still Duplicate, with the first
// capture's ID and state, and an invalid envelope is still ErrInvalid.
// Nothing is written.
func TestDuplicateWinsOverAFullQuota(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		state string
		full  func(*store.NoticeQuotas)
	}{
		"Q1 full": {"pending", func(q *store.NoticeQuotas) { q.PendingPerProject = 1 }},
		"Q6 full": {"imported", func(q *store.NoticeQuotas) { q.IdentitiesPerProject = 1 }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newQuotaFixture(t)
			var first store.NoticeEnvelope
			if tc.state == "imported" {
				first = f.imported("A", 50)
			} else {
				first = f.pending("A", 50)
			}
			f.set(tc.full)
			before := ledgerSnapshot(t, f.s)
			_, err := f.s.AdmitNotice(t.Context(), f.notice("A", 51), ledgerAt)
			require.ErrorIs(t, err, store.ErrNoticeQuota, "the quota is full")

			newText := admissible(t, func(e *store.NoticeEnvelope) {
				e.Project, e.Receipt.Key, e.Text = "A", first.Receipt.Key, "the same receipt, new text"
			})
			newTime := admissible(t, func(e *store.NoticeEnvelope) {
				e.Project, e.Receipt.Key, e.ObservedAt = "A", first.Receipt.Key, first.ObservedAt.Add(time.Minute)
			})
			for what, e := range map[string]store.NoticeEnvelope{"the same envelope": first, "new text": newText, "a new time": newTime} {
				got, err := f.s.AdmitNotice(t.Context(), e, ledgerAt)
				require.NoError(t, err, what)
				assert.Equal(t, store.NoticeAdmission{CaptureID: first.CaptureID, State: tc.state, Duplicate: true}, got, what)
			}
			invalid := first
			invalid.Text = "two\nlines"
			got, err := f.s.AdmitNotice(t.Context(), invalid, ledgerAt)
			require.ErrorIs(t, err, store.ErrInvalid)
			assert.NotErrorIs(t, err, store.ErrNoticeQuota)
			assert.Equal(t, store.NoticeAdmission{}, got)
			assert.Equal(t, before, ledgerSnapshot(t, f.s), "nothing is written")
		})
	}
}

// TestRateWindowIsBoundedBelow (Q-window): Q8 counts a project's
// admissions of the last hour, every state, from one a nanosecond younger
// than an hour to any stamped after the one being checked, and its lower
// bound saturates at the oldest time the store holds, through the last
// nanosecond whose bound would wrap. Q8 is 1, so each project is refused
// exactly when its one earlier row counts.
func TestRateWindowIsBoundedBelow(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 1 })
	for project, tc := range map[string]struct {
		at      time.Time
		counted bool
	}{
		"one-window-old":       {ledgerAt.Add(-store.NoticeRateWindow), false},
		"a-nanosecond-younger": {ledgerAt.Add(-store.NoticeRateWindow + time.Nanosecond), true},
		"stamped-after-at":     {ledgerAt.Add(time.Hour), true},
	} {
		f.admitAt(f.notice(project, 50), tc.at, "pending")
		if tc.counted {
			f.refused(f.notice(project, 51), ledgerAt, 8)
		} else {
			f.pending(project, 51)
		}
	}

	f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 600 })
	f.pending("pending", 50)
	f.imported("imported", 50)
	f.settled("posted", 50, "posted")
	f.settled("dropped", 50, "dropped")
	f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 1 })
	for _, state := range []string{"pending", "imported", "posted", "dropped"} {
		f.refused(f.notice(state, 51), ledgerAt, 8)
	}

	oldest := time.Unix(0, math.MinInt64)
	for project, at := range map[string]time.Time{
		"oldest":            oldest,
		"oldest-and-1ns":    oldest.Add(time.Nanosecond),
		"last-saturating":   oldest.Add(store.NoticeRateWindow - 2*time.Nanosecond),
		"first-unsaturated": oldest.Add(store.NoticeRateWindow - time.Nanosecond),
	} {
		f.admitAt(f.notice(project, 50), oldest, "pending")
		f.refused(f.notice(project, 51), at, 8)
	}
	f.admitAt(f.notice("oldest", 52), oldest.Add(store.NoticeRateWindow), "pending")
}

// TestByteQuotasDoNotWrap (Q-wrap): Q4 and Q5 compare the pending sum with
// the limit less this notice's bytes, so a payload whose bytes a hand edit
// has set to the most int64 holds refuses the next pending notice, its
// project's at Q4 and another project's at Q5, instead of wrapping the sum
// negative and admitting it.
func TestByteQuotasDoNotWrap(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	edited := f.pending("A", 50)
	_, err := store.DB(f.s).ExecContext(t.Context(), "UPDATE notice_payloads SET bytes = ? WHERE capture_id = ?", int64(math.MaxInt64), edited.CaptureID)
	require.NoError(t, err)
	f.refused(f.notice("A", 51), ledgerAt, 4)
	f.refused(f.notice("B", 50), ledgerAt, 5)
}

// fillPending inserts n pending identities of project, by raw SQL in one
// transaction, each on a receipt of its own, admitted at at, on PR pr, or
// each on a PR of its own from 1,000 when pr is 0. The triggers move the
// totals; no payload is stored, so Q4 and Q5 sum nothing for them.
func fillPending(t *testing.T, s *store.Store, project string, n int, pr int64, at time.Time) {
	t.Helper()
	tx, err := store.DB(s).BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for i := range n {
		p := pr
		if p == 0 {
			p = 1000 + int64(i)
		}
		_, err := tx.ExecContext(t.Context(), insertIdentity, fmt.Sprintf("fill:%s:%d", project, i), project, "github", "ci_failed",
			ledgerRepo, p, fmt.Sprintf("fill-%s-%d", project, i), "pending", at.UnixNano(), nil, nil, nil, 0)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
}

// TestQuotasHoldAtTheirDefaults (Q-default): Q1, Q2, Q3 and Q8 at the
// defaults open sets, each filled to cap-1 by raw SQL: the cap-th admission
// is accepted and the next is refused with that quota's own error. It pins
// each walk's LIMIT, which a test at a small cap cannot: a walk bound to a
// smaller quota's cap stops short of the default and never refuses. Q1's to
// Q3's rows are two hours old, outside Q8's window.
func TestQuotasHoldAtTheirDefaults(t *testing.T) {
	t.Parallel()
	old := ledgerAt.Add(-2 * time.Hour)
	for quota, fill := range map[int]func(*quotaFixture) (accepted, next store.NoticeEnvelope){
		1: func(f *quotaFixture) (store.NoticeEnvelope, store.NoticeEnvelope) {
			fillPending(f.t, f.s, "A", 999, 0, old)
			return f.notice("A", 50), f.notice("A", 51)
		},
		2: func(f *quotaFixture) (store.NoticeEnvelope, store.NoticeEnvelope) {
			for i, n := range []int{1000, 1000, 1000, 1000, 999} {
				fillPending(f.t, f.s, fmt.Sprintf("P%d", i), n, 0, old)
			}
			return f.notice("C", 50), f.notice("D", 50)
		},
		3: func(f *quotaFixture) (store.NoticeEnvelope, store.NoticeEnvelope) {
			fillPending(f.t, f.s, "A", 31, 50, old)
			return f.notice("A", 50), f.notice("A", 50)
		},
		8: func(f *quotaFixture) (store.NoticeEnvelope, store.NoticeEnvelope) {
			fillPending(f.t, f.s, "A", 599, 0, ledgerAt)
			return f.notice("A", 50), f.notice("A", 51)
		},
	} {
		t.Run(fmt.Sprintf("Q%d", quota), func(t *testing.T) {
			t.Parallel()
			f := newQuotaFixture(t)
			accepted, next := fill(f)
			f.admitAt(accepted, ledgerAt, "pending")
			t.Logf("Q%d at its default cap of %d: the cap-th admission is accepted", quota, quotaLimit(store.NoticeQuotasOf(f.s), quota))
			f.refused(next, ledgerAt, quota)
		})
	}
}

// TestQuotaHoldsUnderConcurrency (Q-race): the quotas are read in the
// admission's own transaction, so 32 admissions racing for a project with
// Q1 = 10 admit exactly 10, in each of 20 rounds.
func TestQuotaHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	f.set(func(q *store.NoticeQuotas) { q.PendingPerProject = 10 })
	for round := range 20 {
		project := fmt.Sprintf("round-%d", round)
		es := make([]store.NoticeEnvelope, 32)
		for i := range es {
			es[i] = f.notice(project, 50)
		}
		errs := make([]error, len(es))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, e := range es {
			wg.Go(func() {
				<-start
				_, errs[i] = f.s.AdmitNotice(t.Context(), e, ledgerAt)
			})
		}
		close(start)
		wg.Wait()
		admitted := 0
		for _, err := range errs {
			if err == nil {
				admitted++
				continue
			}
			assert.ErrorIs(t, err, store.ErrNoticePendingPerProject, "round %d", round)
		}
		assert.Equal(t, 10, admitted, "round %d admits exactly Q1's 10", round)
	}
	assertNoticeTotals(t, store.DB(f.s))
}

// TestQuotaErrorsCarryNoInput (Q-noinput): each quota, set to 0, refuses a
// notice with its own error, and no refusal's text carries the notice's
// project, repo, key or text.
func TestQuotaErrorsCarryNoInput(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	e := f.notice("project-mark", 50, func(e *store.NoticeEnvelope) {
		e.Receipt.Repo, e.Receipt.Key, e.Text = "owner-mark/repo-mark", strings.Repeat("abcdef12", 5), "text-mark in the notice"
	})
	limits := []func(*store.NoticeQuotas){
		func(q *store.NoticeQuotas) { q.PendingPerProject = 0 },
		func(q *store.NoticeQuotas) { q.PendingInStore = 0 },
		func(q *store.NoticeQuotas) { q.PendingPerPR = 0 },
		func(q *store.NoticeQuotas) { q.BytesPerProject = 0 },
		func(q *store.NoticeQuotas) { q.BytesInStore = 0 },
		func(q *store.NoticeQuotas) { q.IdentitiesPerProject = 0 },
		func(q *store.NoticeQuotas) { q.IdentitiesInStore = 0 },
		func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 0 },
	}
	defaults := store.NoticeQuotasOf(f.s)
	for i, limit := range limits {
		store.SetNoticeQuotas(f.s, defaults)
		f.set(limit)
		_, err := f.s.AdmitNotice(t.Context(), e, ledgerAt)
		require.ErrorIs(t, err, quotaErrs[i], "Q%d at 0 refuses", i+1)
		for _, mark := range []string{"project-mark", "owner-mark", "repo-mark", e.Receipt.Key, "text-mark"} {
			assert.NotContains(t, err.Error(), mark, "Q%d: %v", i+1, err)
		}
	}
}

// TestAMissingTotalIsAnError (K4): Q6 and Q7 read 0010's totals and fail
// closed when one is missing. With the store's row gone, every admission is
// ErrStore; with a project's row gone while it has identities, that
// project's is. Neither writes anything. A project with no identities and
// no row is admitted.
func TestAMissingTotalIsAnError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		remove  string
		project string
	}{
		"the store's row": {"DELETE FROM notice_store_counts WHERE id = 1", "B"},
		"a project's row": {"DELETE FROM notice_counts WHERE project = 'A'", "A"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newQuotaFixture(t)
			f.pending("A", 50)
			_, err := store.DB(f.s).ExecContext(t.Context(), tc.remove)
			require.NoError(t, err)
			before := ledgerSnapshot(t, f.s)
			got, err := f.s.AdmitNotice(t.Context(), f.notice(tc.project, 51), ledgerAt)
			require.ErrorIs(t, err, store.ErrStore)
			assert.NotErrorIs(t, err, store.ErrNoticeQuota)
			assert.Equal(t, store.NoticeAdmission{}, got)
			assert.Equal(t, before, ledgerSnapshot(t, f.s), "nothing is written")
		})
	}
	f := newQuotaFixture(t)
	f.pending("A", 50)
	_, err := store.DB(f.s).ExecContext(t.Context(), "DELETE FROM notice_counts WHERE project = 'A'")
	require.NoError(t, err)
	require.Empty(t, queryRows(t, store.DB(f.s), "SELECT 1 FROM notice_counts WHERE project = 'C'"))
	f.pending("C", 50)
}

// TestQuotaQueriesUseTheirIndexes (Q-plan): each quota's query is planned
// on the index the packet's cost table names, so no check reads more than
// its cap's rows, and no line of any plan scans a table: a SCAN may only
// walk a subquery or a co-routine, or the constant row.
func TestQuotaQueriesUseTheirIndexes(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	for quota, tc := range map[string]struct {
		args []any
		uses []string
	}{
		"Q1": {[]any{"ha", 1000}, []string{"USING COVERING INDEX notice_identities_by_state"}},
		"Q2": {[]any{5000}, []string{"USING COVERING INDEX notice_identities_store_state"}},
		"Q3": {[]any{"ha", ledgerRepo, 50, 32}, []string{"USING COVERING INDEX notice_identities_by_pr"}},
		"Q4": {[]any{"ha", 1000}, []string{"USING INDEX notice_identities_by_state", "USING INDEX sqlite_autoindex_notice_payloads_1"}},
		"Q5": {[]any{5000}, []string{"USING INDEX notice_identities_store_state", "USING INDEX sqlite_autoindex_notice_payloads_1"}},
		"Q6": {[]any{"ha"}, []string{"USING INDEX sqlite_autoindex_notice_counts_1", "USING COVERING INDEX notice_identities_by_state"}},
		"Q7": {nil, []string{"USING INTEGER PRIMARY KEY"}},
		"Q8": {[]any{"ha", 0, 600, "github"}, []string{"USING INDEX notice_identities_by_state (project=? AND state=? AND admitted_at>?)"}},
	} {
		query, ok := store.NoticeQuotaQueries[quota]
		require.True(t, ok, quota)
		plan := queryPlan(t, db, query, tc.args...)
		t.Logf("%s plan: %q", quota, plan)
		for _, use := range tc.uses {
			assert.True(t, slices.ContainsFunc(plan, func(line string) bool { return strings.Contains(line, use) }),
				"%s's plan %q uses %s", quota, plan, use)
		}
		assertNoTableScan(t, plan)
	}
	assert.Len(t, store.NoticeQuotaQueries, 8)
}

// queryPlan is the detail of each line of query's plan on db, with args.
func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	var plan []string
	for _, row := range queryRows(t, db, "EXPLAIN QUERY PLAN "+query, args...) {
		detail, ok := row[3].(string)
		require.True(t, ok, "a plan line: %v", row)
		plan = append(plan, detail)
	}
	return plan
}

// assertNoTableScan fails on a line of plan that scans a table. A SCAN
// line passes only if the rest of it, compared whole, is CONSTANT ROW, a
// name a CO-ROUTINE or MATERIALIZE line of the same plan introduced, or
// one of allowed. Every other line is not a scan.
func assertNoTableScan(t *testing.T, plan []string, allowed ...string) {
	t.Helper()
	walks := map[string]bool{"CONSTANT ROW": true}
	for _, name := range allowed {
		walks[name] = true
	}
	for _, line := range plan {
		for _, kind := range []string{"CO-ROUTINE ", "MATERIALIZE "} {
			if name, ok := strings.CutPrefix(line, kind); ok {
				walks[name] = true
			}
		}
	}
	for _, line := range plan {
		if name, ok := strings.CutPrefix(line, "SCAN "); ok {
			assert.True(t, walks[name], "the plan %q scans %s, a table", plan, name)
		}
	}
}

// TestQ8KeepsItsIndex (S14): Q8 names its index, so an index without the
// project, such as the prune's by_source, never takes it over. The test
// makes such an index itself, so it holds on any schema. Self-check: the
// same query without its INDEXED BY plans on that index, so the test can
// tell a lost pin.
func TestQ8KeepsItsIndex(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	_, err := db.ExecContext(t.Context(), `CREATE INDEX t_by_source ON notice_identities (source, state, admitted_at)`)
	require.NoError(t, err)
	args := []any{"ha", 0, 600, "github"}

	q8 := store.NoticeQuotaQueries["Q8"]
	plan := queryPlan(t, db, q8, args...)
	t.Logf("Q8 plan: %q", plan)
	assert.True(t, slices.ContainsFunc(plan, func(line string) bool {
		return strings.Contains(line, "USING INDEX notice_identities_by_state (project=? AND state=? AND admitted_at>?)")
	}), "Q8's plan %q seeks notice_identities_by_state by project, state and time", plan)
	assertNoTableScan(t, plan)

	unpinned := strings.Replace(q8, "INDEXED BY notice_identities_by_state", "", 1)
	require.NotEqual(t, q8, unpinned, "Q8 names its index")
	lost := queryPlan(t, db, unpinned, args...)
	t.Logf("Q8 unpinned plan: %q", lost)
	assert.True(t, slices.ContainsFunc(lost, func(line string) bool { return strings.Contains(line, "USING INDEX t_by_source") }),
		"without its pin, Q8's plan %q moves to t_by_source", lost)
}

// TestQ8CountsPerSource (S12): Q8 counts a project's admissions of one
// source. With Q8 at 3, a source's three admissions leave the other's hour
// whole, in either order, and a fourth of either source is refused, by an
// error that says the limit is per source. NoticeStats counts each
// source's three. Every notice is admitted at daemonAt, so both sources
// share one window.
func TestQ8CountsPerSource(t *testing.T) {
	t.Parallel()
	for name, githubFirst := range map[string]bool{"GitHub first": true, "the daemon first": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newQuotaFixture(t)
			f.set(func(q *store.NoticeQuotas) { q.AdmissionsPerHour = 3 })
			github := func() {
				for pr := range int64(3) {
					f.admitAt(f.notice("ha", 50+pr), daemonAt, "pending")
				}
			}
			daemon := func() {
				for range 3 {
					admitDaemon(t, f.s, daemonNotice(), daemonAt)
				}
			}
			if githubFirst {
				github()
				daemon()
			} else {
				daemon()
				github()
			}
			for _, next := range []store.NoticeEnvelope{f.notice("ha", 53), daemonNotice()} {
				_, err := f.s.AdmitNotice(t.Context(), next, daemonAt)
				assert.ErrorContains(t, err, "notices of one source admitted per project in an hour", "a %s notice", next.Source)
				f.refused(next, daemonAt, 8)
			}
			st, err := f.s.NoticeStats(t.Context(), "ha", daemonAt)
			require.NoError(t, err)
			assert.Equal(t, int64(3), st.GitHubLastHour)
			assert.Equal(t, int64(3), st.DaemonLastHour)
		})
	}
}

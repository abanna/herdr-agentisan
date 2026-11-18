package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// rawNotice writes one identity of the ledger's by raw SQL, admitted at
// the nanosecond at, as a later slice or a hand edit would: a GitHub one
// on ledgerRepo's PR pr with a receipt key of its own, a daemon one with
// no receipt, as B0a.4 writes it. A settled one gets what its state needs.
// With bytes above 0 it also gets a payload that size, whose subject,
// worker and observed time are the payload's own.
func rawNotice(t *testing.T, s *store.Store, id, project, source, state string, at int64, bytes int64) {
	t.Helper()
	db := store.DB(s)
	row := map[string]any{
		"capture_id": id, "project": project, "source": source, "kind": "turn_ended",
		"state": state, "admitted_at": at, "attempts": 0,
	}
	if source == "github" {
		row["kind"], row["repo"], row["pr"], row["receipt_key"] = "ci_failed", ledgerRepo, int64(1), "key-"+id
	}
	switch state {
	case "posted":
		row["settled_at"], row["post_seq"] = at, int64(1)
	case "dropped":
		row["settled_at"], row["drop_reason"] = at, "skipped"
	}
	require.NoError(t, insertRow(t, db, insertIdentity, identityColumns, row), "identity %s", id)
	if bytes == 0 {
		return
	}
	sum := sha256.Sum256([]byte(id))
	payload := map[string]any{
		"capture_id": id, "version": 1, "subject": "subject of " + id, "worker": "worker of " + id,
		"observed_at": at - 1, "text": "text of " + id, "bytes": bytes, "digest": hex.EncodeToString(sum[:]),
	}
	if source == "github" {
		payload["receipt_version"] = 1
	}
	require.NoError(t, insertRow(t, db, insertPayload, payloadColumns, payload), "payload %s", id)
}

// captureIDs is each notice's capture ID, in order.
func captureIDs(notices []store.NoticeMeta) []string {
	ids := make([]string, 0, len(notices))
	for _, n := range notices {
		ids = append(ids, n.CaptureID)
	}
	return ids
}

// stats is project's NoticeStats as of at, which must be read.
func stats(t *testing.T, s *store.Store, project string, at time.Time) store.NoticeStats {
	t.Helper()
	got, err := s.NoticeStats(t.Context(), project, at)
	require.NoError(t, err)
	return got
}

// TestNoticeMetadataShapesAreClosed (M-shape): NoticeMeta holds the nine
// fields of a pending notice's metadata, never its text or receipt key, and
// NoticeStats the eight counts the quotas compare, each by name, type and
// order.
func TestNoticeMetadataShapesAreClosed(t *testing.T) {
	t.Parallel()
	fields := func(v any) []string {
		typ := reflect.TypeOf(v)
		out := make([]string, 0, typ.NumField())
		for i := range typ.NumField() {
			out = append(out, typ.Field(i).Name+" "+typ.Field(i).Type.String())
		}
		return out
	}
	assert.Equal(t, []string{
		"CaptureID string", "Source string", "Kind string", "Subject string", "Worker string",
		"Repo string", "PR int64", "ObservedAt time.Time", "AdmittedAt time.Time",
	}, fields(store.NoticeMeta{}))
	assert.Equal(t, []string{
		"Pending int64", "PendingBytes int64", "Identities int64",
		"GitHubLastHour int64", "DaemonLastHour int64",
		"StorePending int64", "StorePendingBytes int64", "StoreIdentities int64",
	}, fields(store.NoticeStats{}))
}

// TestPendingNoticesLimit (M-limit): a read lists 1 to MaxBoardRead (200)
// notices, the oldest first; any other limit is ErrInvalid. 201 pending
// notices, each admitted at its own time, on PRs 1 to 7, at most 29 a PR.
func TestPendingNoticesLimit(t *testing.T) {
	t.Parallel()
	require.Equal(t, 200, store.MaxBoardRead)
	f := newQuotaFixture(t)
	admitted := make([]store.NoticeEnvelope, 0, 201)
	for i := range 201 {
		admitted = append(admitted, f.admitAt(f.notice("ha", int64(i%7+1)), ledgerAt.Add(time.Duration(i)*time.Second), "pending"))
	}
	for _, limit := range []int{0, -1, store.MaxBoardRead + 1, math.MinInt, math.MaxInt} {
		got, err := f.s.PendingNotices(t.Context(), "ha", limit)
		require.ErrorIs(t, err, store.ErrInvalid, "a limit of %d", limit)
		assert.EqualError(t, err, fmt.Sprintf("%v: a read of %d notices: want 1 to 200", store.ErrInvalid, limit))
		assert.Nil(t, got)
	}

	one, err := f.s.PendingNotices(t.Context(), "ha", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{admitted[0].CaptureID}, captureIDs(one), "a limit of 1 is the oldest")

	most, err := f.s.PendingNotices(t.Context(), "ha", store.MaxBoardRead)
	require.NoError(t, err)
	want := make([]string, 0, 200)
	for _, e := range admitted[:200] {
		want = append(want, e.CaptureID)
	}
	assert.Equal(t, want, captureIDs(most), "a limit of 200 is the 200 oldest, oldest first")
}

// TestPendingNoticesOldestFirst (M-order): a project's pending notices come
// oldest admitted first, a tie by capture ID, whatever order they were
// written in, and a notice of another state or another project is never
// listed. The pair at one instant is written larger ID first, so an order
// without the tie-break lists it as written.
func TestPendingNoticesOldestFirst(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	at := ledgerAt.UnixNano()
	rawNotice(t, s, "gh:g1", "ha", "github", "pending", at+1, 120)
	rawNotice(t, s, "nt:zz", "ha", "daemon", "pending", at, 80)
	rawNotice(t, s, "gh:old-imported", "ha", "github", "imported", at-5, 0)
	rawNotice(t, s, "nt:d1", "ha", "daemon", "pending", at+2, 90)
	rawNotice(t, s, "gh:old-dropped", "ha", "github", "dropped", at-1, 70)
	rawNotice(t, s, "nt:d0", "ha", "daemon", "pending", at, 60)
	rawNotice(t, s, "nt:posted", "ha", "daemon", "posted", at+1, 50)
	rawNotice(t, s, "gh:hb-1", "hb", "github", "pending", at-3, 40)
	rawNotice(t, s, "nt:hb-2", "hb", "daemon", "pending", at+1, 30)
	rawNotice(t, s, "nt:hb-0", "hb", "daemon", "pending", math.MinInt64+1, 30)
	rawNotice(t, s, "nt:spaces", "", "daemon", "pending", at-4, 20)
	rawNotice(t, s, "nt:upper", "HA", "daemon", "pending", at-6, 20)
	odd := "é-tëam'; DROP TABLE notice_payloads; -- %_*[?]"
	rawNotice(t, s, "nt:odd", odd, "daemon", "pending", at-7, 20)

	got, err := s.PendingNotices(t.Context(), "ha", store.MaxBoardRead)
	require.NoError(t, err)
	assert.Equal(t, []string{"nt:d0", "nt:zz", "gh:g1", "nt:d1"}, captureIDs(got))

	got, err = s.PendingNotices(t.Context(), "hb", store.MaxBoardRead)
	require.NoError(t, err)
	assert.Equal(t, []string{"nt:hb-0", "gh:hb-1", "nt:hb-2"}, captureIDs(got))
	require.NotEmpty(t, got)
	assert.True(t, time.Unix(0, math.MinInt64+1).Equal(got[0].AdmittedAt), "a notice admitted before 1970 is read as its instant: %v", got[0].AdmittedAt)
	assert.True(t, time.Unix(0, math.MinInt64).Equal(got[0].ObservedAt), "observed at the oldest instant: %v", got[0].ObservedAt)

	got, err = s.PendingNotices(t.Context(), "", store.MaxBoardRead)
	require.NoError(t, err)
	assert.Equal(t, []string{"nt:spaces"}, captureIDs(got), "the empty project is a project")

	// A project is matched exactly, byte for byte: another case, a NUL,
	// bytes that are not UTF-8 or a pattern character is another project.
	for project, want := range map[string][]string{
		odd: {"nt:odd"}, "HA": {"nt:upper"},
	} {
		got, err = s.PendingNotices(t.Context(), project, store.MaxBoardRead)
		require.NoError(t, err, "%q", project)
		assert.Equal(t, want, captureIDs(got), "%q", project)
	}
	for _, project := range oddProjects {
		if project == "" || project == "HA" {
			continue // each has a notice of its own here, listed above
		}
		got, err = s.PendingNotices(t.Context(), project, store.MaxBoardRead)
		require.NoError(t, err, "%q", project)
		assert.Equal(t, []string{}, captureIDs(got), "%q", project)
	}
}

// oddProjects are spellings near "ha" that are other projects: a read
// matches a project byte for byte, so each has none of ha's notices. They
// hold a NUL, bytes that are not UTF-8 (cut short, a surrogate's, Latin-1,
// mixed), a byte-order mark, a line ending, a pattern character, a space
// or SQL, and the empty project.
var oddProjects = []string{
	"", "HA", " ha", "ha\x00", "ha\xff", "ha\xc3", "h\xed\xa0\x80", "caf\xe9", "é\xff", "\ufeffha", "ha\r\n",
	"h%", "h_", "é-tëam'; DROP TABLE notice_counts; --",
}

// TestPendingNoticesFields (M-fields): each field of a listed notice is
// the admitted one's, both times in UTC, and a daemon notice, which has no
// receipt, has no repo and PR 0 (F-q1). An empty ledger lists an empty,
// non-nil slice.
func TestPendingNoticesFields(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	none, err := s.PendingNotices(t.Context(), "ha", 1)
	require.NoError(t, err)
	assert.NotNil(t, none)
	assert.Empty(t, none)

	zone := time.FixedZone("UTC-6", -6*3600)
	e := admissible(t, func(e *store.NoticeEnvelope) {
		e.ObservedAt = ledgerAt.Add(-time.Minute).In(zone)
		e.Receipt.PR = 77
	})
	admittedAt := ledgerAt.Add(time.Minute).In(zone)
	got, err := s.AdmitNotice(t.Context(), e, admittedAt)
	require.NoError(t, err)
	require.Equal(t, "pending", got.State)
	daemonAt := ledgerAt.Add(2 * time.Minute).UnixNano()
	rawNotice(t, s, "nt1:daemon", "ha", "daemon", "pending", daemonAt, 64)

	listed, err := s.PendingNotices(t.Context(), "ha", store.MaxBoardRead)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	wantTimes := [][2]time.Time{{e.ObservedAt, admittedAt}, {time.Unix(0, daemonAt-1), time.Unix(0, daemonAt)}}
	for i, n := range listed {
		assert.True(t, wantTimes[i][0].Equal(n.ObservedAt), "row %d observed at %v, read back as %v", i, wantTimes[i][0], n.ObservedAt)
		assert.True(t, wantTimes[i][1].Equal(n.AdmittedAt), "row %d admitted at %v, read back as %v", i, wantTimes[i][1], n.AdmittedAt)
		assert.Equal(t, time.UTC, n.ObservedAt.Location(), "row %d", i)
		assert.Equal(t, time.UTC, n.AdmittedAt.Location(), "row %d", i)
		listed[i].ObservedAt, listed[i].AdmittedAt = time.Time{}, time.Time{}
	}
	assert.Equal(t, []store.NoticeMeta{
		{CaptureID: e.CaptureID, Source: "github", Kind: "ci_failed", Subject: ledgerRepo + "#50", Worker: "ha-coder-3", Repo: ledgerRepo, PR: 77},
		{CaptureID: "nt1:daemon", Source: "daemon", Kind: "turn_ended", Subject: "subject of nt1:daemon", Worker: "worker of nt1:daemon"},
	}, listed)
}

// TestPendingNoticesMissingPayloadIsAnError (M-missing): a pending notice
// whose payload was deleted by hand is ErrStore, never listed with empty
// fields and never skipped.
func TestPendingNoticesMissingPayloadIsAnError(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	e := admissible(t)
	admit(t, s, e)
	admit(t, s, admissible(t, withKey(strings.Repeat("b", 40))))
	_, err := store.DB(s).ExecContext(t.Context(), "DELETE FROM notice_payloads WHERE capture_id = ?", e.CaptureID)
	require.NoError(t, err)

	got, err := s.PendingNotices(t.Context(), "ha", store.MaxBoardRead)
	require.ErrorIs(t, err, store.ErrStore)
	assert.EqualError(t, err, fmt.Sprintf("%v: a pending notice has no payload", store.ErrStore))
	assert.Nil(t, got)
}

// metaRow is one notice of TestStatsMatchTheQuotas' fixture: a GitHub one
// admitted through AdmitNotice, a daemon one written raw. bytes is its
// stored payload's size, 0 for none.
type metaRow struct {
	project, source, state string
	age                    time.Duration
	bytes                  int64
}

// handCount is NoticeStats counted by hand from rows, for project, as of
// ledgerAt.
func handCount(rows []metaRow, project string) store.NoticeStats {
	var want store.NoticeStats
	start := ledgerAt.Add(-store.NoticeRateWindow + time.Nanosecond)
	for _, r := range rows {
		mine := r.project == project
		pending := r.state == "pending"
		inWindow := !ledgerAt.Add(-r.age).Before(start)
		want.StoreIdentities++
		if pending {
			want.StorePending++
			want.StorePendingBytes += r.bytes
		}
		if !mine {
			continue
		}
		want.Identities++
		if pending {
			want.Pending++
			want.PendingBytes += r.bytes
		}
		switch {
		case inWindow && r.source == "github":
			want.GitHubLastHour++
		case inWindow && r.source == "daemon":
			want.DaemonLastHour++
		}
	}
	return want
}

// quotaField is Qquota's limit in q.
func quotaField(q *store.NoticeQuotas, quota int) *int64 {
	return []*int64{
		&q.PendingPerProject, &q.PendingInStore, &q.PendingPerPR, &q.BytesPerProject, &q.BytesInStore,
		&q.IdentitiesPerProject, &q.IdentitiesInStore, &q.AdmissionsPerHour,
	}[quota-1]
}

// TestStatsMatchTheQuotas (M-stats): each stat equals a hand recount, for a
// project and for the store, and is the count its quota compares: a count
// quota set to its stat refuses the next admission and one more admits it;
// a byte quota set to its stat plus the next notice's bytes admits it and
// one byte less refuses it. Q8 is run on hb, whose window holds GitHub rows
// only, so it compares GitHubLastHour before B0a.4-hour and after it. The
// stats are exact, never capped at their quotas' limits.
func TestStatsMatchTheQuotas(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	rows := []metaRow{
		// ha's window: 3 GitHub, 2 daemon, not all pending.
		{"ha", "github", "pending", 30 * time.Minute, 0},
		{"ha", "github", "imported", 20 * time.Minute, 0},
		{"ha", "github", "dropped", 10 * time.Minute, 0},
		{"ha", "daemon", "pending", 15 * time.Minute, 300},
		{"ha", "daemon", "posted", 5 * time.Minute, 200},
		// ha outside it: a daemon row exactly one window old, older rows.
		{"ha", "daemon", "pending", time.Hour, 100},
		{"ha", "github", "posted", 2 * time.Hour, 0},
		{"ha", "github", "pending", 90 * time.Minute, 0},
		{"ha", "daemon", "dropped", 3 * time.Hour, 0},
		// hb's window: GitHub only; its daemon rows are older.
		{"hb", "github", "pending", 40 * time.Minute, 0},
		{"hb", "github", "posted", 50 * time.Minute, 0},
		{"hb", "github", "imported", 5 * time.Minute, 0},
		{"hb", "github", "dropped", 2 * time.Hour, 0},
		{"hb", "daemon", "pending", time.Hour + time.Nanosecond, 50},
		{"hb", "daemon", "posted", 2 * time.Hour, 0},
	}
	for i := range rows {
		r := &rows[i]
		at := ledgerAt.Add(-r.age)
		if r.source == "daemon" {
			rawNotice(t, f.s, fmt.Sprintf("nt:%d", i), r.project, "daemon", r.state, at.UnixNano(), r.bytes)
			continue
		}
		pr := int64(i%3 + 1)
		switch r.state {
		case "imported":
			e := f.notice(r.project, pr)
			seen(t, f.s, r.project, store.GitHubEvent{PR: int(pr), Kind: e.Kind, Key: e.Receipt.Key})
			f.admitAt(e, at, "imported")
		case "pending":
			r.bytes = bytesOf(t, f.admitAt(f.notice(r.project, pr), at, "pending"))
		default:
			e := f.admitAt(f.notice(r.project, pr), at, "pending")
			settle := map[string]string{
				"posted":  "UPDATE notice_identities SET state = 'posted', settled_at = ?, post_seq = 1 WHERE capture_id = ?",
				"dropped": "UPDATE notice_identities SET state = 'dropped', settled_at = ?, drop_reason = 'skipped' WHERE capture_id = ?",
			}[r.state]
			_, err := store.DB(f.s).ExecContext(t.Context(), settle, at.UnixNano(), e.CaptureID)
			require.NoError(t, err)
			// A settled GitHub row keeps its payload, which no stat counts.
		}
	}

	// The recount, in hand-written numbers first.
	ha, hb := handCount(rows, "ha"), handCount(rows, "hb")
	assert.Equal(t, [5]int64{4, 9, 3, 2, 15}, [5]int64{ha.Pending, ha.Identities, ha.GitHubLastHour, ha.DaemonLastHour, ha.StoreIdentities})
	assert.Equal(t, [4]int64{2, 6, 3, 0}, [4]int64{hb.Pending, hb.Identities, hb.GitHubLastHour, hb.DaemonLastHour})
	assert.Equal(t, int64(6), ha.StorePending)
	for project, want := range map[string]store.NoticeStats{"ha": ha, "hb": hb} {
		assert.Equal(t, want, stats(t, f.s, project, ledgerAt), "%s's stats are the recount", project)
	}
	storeOnly := store.NoticeStats{StorePending: ha.StorePending, StorePendingBytes: ha.StorePendingBytes, StoreIdentities: ha.StoreIdentities}
	for _, project := range oddProjects {
		assert.Equal(t, storeOnly, stats(t, f.s, project, ledgerAt), "%q is matched exactly: another project, with none", project)
	}

	// Not capped: with every quota below its count, the stats are the same.
	saved := store.NoticeQuotasOf(f.s)
	f.set(func(q *store.NoticeQuotas) {
		for quota := 1; quota <= 8; quota++ {
			*quotaField(q, quota) = 1
		}
	})
	for project, want := range map[string]store.NoticeStats{"ha": ha, "hb": hb} {
		assert.Equal(t, want, stats(t, f.s, project, ledgerAt), "%s's stats with every quota at 1", project)
	}
	store.SetNoticeQuotas(f.s, saved)

	// Each quota at its stat: only the quota under test can refuse.
	for _, quota := range []int{1, 2, 4, 5, 6, 7, 8} {
		project := "ha"
		if quota == 8 {
			project = "hb"
		}
		st := stats(t, f.s, project, ledgerAt)
		stat := map[int]int64{
			1: st.Pending, 2: st.StorePending, 4: st.PendingBytes, 5: st.StorePendingBytes,
			6: st.Identities, 7: st.StoreIdentities, 8: st.GitHubLastHour,
		}[quota]
		next := f.notice(project, 9)
		limit := stat
		if quota == 4 || quota == 5 {
			limit = stat + bytesOf(t, next) - 1
		}
		f.set(func(q *store.NoticeQuotas) {
			for other := 1; other <= 8; other++ {
				*quotaField(q, other) = 1 << 40
			}
			*quotaField(q, quota) = limit
		})
		f.refused(next, ledgerAt, quota)
		f.set(func(q *store.NoticeQuotas) { *quotaField(q, quota) = limit + 1 })
		f.admitAt(next, ledgerAt, "pending")
	}

	// The text's own case: Q1 at 2, 3 pending rows, Pending 3.
	s := open(t, dbPath(t))
	q := store.NoticeQuotasOf(s)
	q.PendingPerProject = 2
	store.SetNoticeQuotas(s, q)
	for i := range 3 {
		rawNotice(t, s, fmt.Sprintf("nt:%d", i), "ha", "daemon", "pending", ledgerAt.UnixNano()+int64(i), 10)
	}
	assert.Equal(t, int64(3), stats(t, s, "ha", ledgerAt).Pending)
}

// TestStatsWindowMatchesQ8 (M-window): each hour stat counts its own
// source's admissions as Q8 bounds them: one exactly a window old is out,
// one a nanosecond younger is in, as is one stamped after at, and the
// bound saturates at the oldest time the store holds. A row of the other
// source a nanosecond inside the window is never counted (C2).
func TestStatsWindowMatchesQ8(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	at := ledgerAt.UnixNano()
	window := int64(store.NoticeRateWindow)
	pick := func(st store.NoticeStats, source string) int64 {
		if source == "github" {
			return st.GitHubLastHour
		}
		return st.DaemonLastHour
	}
	for _, source := range []string{"github", "daemon"} {
		other := map[string]string{"github": "daemon", "daemon": "github"}[source]
		for _, tc := range []struct {
			name    string
			at      int64
			counted int64
		}{
			{"one-window-old", at - window, 0},
			{"a-nanosecond-younger", at - window + 1, 1},
			{"stamped-after-at", at + window, 1},
		} {
			project := source + "-" + tc.name
			rawNotice(t, s, project+":row", project, source, "pending", tc.at, 0)
			rawNotice(t, s, project+":other", project, other, "pending", at-window+1, 0)
			st := stats(t, s, project, ledgerAt)
			assert.Equal(t, tc.counted, pick(st, source), "%s: its own source", project)
			assert.Equal(t, int64(1), pick(st, other), "%s: the other source's row a nanosecond inside", project)
		}

		oldest := time.Unix(0, math.MinInt64)
		for name, readAt := range map[string]time.Time{
			"oldest":            oldest,
			"oldest-and-1ns":    oldest.Add(time.Nanosecond),
			"last-saturating":   oldest.Add(store.NoticeRateWindow - 2*time.Nanosecond),
			"first-unsaturated": oldest.Add(store.NoticeRateWindow - time.Nanosecond),
			"one-window-on":     oldest.Add(store.NoticeRateWindow),
		} {
			project := source + "-" + name
			rawNotice(t, s, project+":row", project, source, "posted", math.MinInt64, 0)
			want := int64(1)
			if name == "one-window-on" {
				want = 0
			}
			assert.Equal(t, want, pick(stats(t, s, project, readAt), source), "%s", project)
		}
	}
}

// TestStatsMissingTotalIsAnError (M-missing-total): a total 0010 keeps
// that is gone is ErrStore, the store's or a project's that has
// identities, while a project with a kept zero row, or with no row and no
// identities, has none.
func TestStatsMissingTotalIsAnError(t *testing.T) {
	t.Parallel()
	missing := fmt.Sprintf("%v: a notice ledger total is missing", store.ErrStore)

	s := open(t, dbPath(t))
	admit(t, s, admissible(t))
	_, err := store.DB(s).ExecContext(t.Context(), "DELETE FROM notice_store_counts")
	require.NoError(t, err)
	_, err = s.NoticeStats(t.Context(), "ha", ledgerAt)
	require.ErrorIs(t, err, store.ErrStore)
	assert.EqualError(t, err, missing, "the store's total")

	s = open(t, dbPath(t))
	admit(t, s, admissible(t))
	rawNotice(t, s, "nt:gone", "B", "daemon", "pending", ledgerAt.UnixNano(), 0)
	db := store.DB(s)
	_, err = db.ExecContext(t.Context(), "DELETE FROM notice_identities WHERE capture_id = 'nt:gone'")
	require.NoError(t, err)
	require.Equal(t, [][]any{{int64(0)}}, queryRows(t, db, "SELECT identities FROM notice_counts WHERE project = 'B'"), "B keeps a zero row")
	for _, project := range []string{"B", "C"} {
		st, err := s.NoticeStats(t.Context(), project, ledgerAt)
		require.NoError(t, err, project)
		assert.Equal(t, int64(0), st.Identities, project)
		assert.Equal(t, int64(1), st.StoreIdentities, project)
	}
	_, err = db.ExecContext(t.Context(), "DELETE FROM notice_counts WHERE project = 'ha'")
	require.NoError(t, err)
	_, err = s.NoticeStats(t.Context(), "ha", ledgerAt)
	require.ErrorIs(t, err, store.ErrStore)
	assert.EqualError(t, err, missing, "ha's total, while ha has an identity")
}

// TestStatsByteSumOverflowIsAnError: pending bytes that a hand edit has
// made sum past what int64 holds are ErrStore, never a wrapped count.
func TestStatsByteSumOverflowIsAnError(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	rawNotice(t, s, "nt:1", "ha", "daemon", "pending", ledgerAt.UnixNano(), math.MaxInt64)
	assert.Equal(t, int64(math.MaxInt64), stats(t, s, "ha", ledgerAt).PendingBytes)
	rawNotice(t, s, "nt:2", "ha", "daemon", "pending", ledgerAt.UnixNano()+1, 1)
	_, err := s.NoticeStats(t.Context(), "ha", ledgerAt)
	require.ErrorIs(t, err, store.ErrStore)
	assert.ErrorContains(t, err, "integer overflow")
}

// TestMetadataQueriesWriteNothing (M-read): both reads leave every table's
// rows as they were, the ledger's totals included, and wake no board
// reader.
func TestMetadataQueriesWriteNothing(t *testing.T) {
	t.Parallel()
	f := newQuotaFixture(t)
	f.pending("ha", 1)
	f.otherStates("ha", 2)
	rawNotice(t, f.s, "nt:pending", "ha", "daemon", "pending", ledgerAt.UnixNano()+1, 64)
	rawNotice(t, f.s, "nt:dropped", "ha", "daemon", "dropped", ledgerAt.UnixNano()+2, 0)
	db := store.DB(f.s)
	ledger, others := ledgerSnapshot(t, f.s), otherTables(t, db)
	woken := f.s.BoardChanged()

	listed, err := f.s.PendingNotices(t.Context(), "ha", store.MaxBoardRead)
	require.NoError(t, err)
	require.Len(t, listed, 2, "the read saw the fixture")
	st := stats(t, f.s, "ha", ledgerAt)
	require.Equal(t, int64(6), st.Identities, "the stats saw the fixture")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			again, err := f.s.PendingNotices(t.Context(), "ha", store.MaxBoardRead)
			assert.NoError(t, err)
			assert.Equal(t, listed, again, "a read beside others lists the same")
			got, err := f.s.NoticeStats(t.Context(), "ha", ledgerAt)
			assert.NoError(t, err)
			assert.Equal(t, st, got, "stats read beside others are the same")
		})
	}
	wg.Wait()

	assert.Equal(t, ledger, ledgerSnapshot(t, f.s), "the ledger, its totals included, is unchanged")
	assert.Equal(t, others, otherTables(t, db), "no other table changed")
	select {
	case <-woken:
		t.Fatal("a metadata read woke the board's readers")
	default:
	}
}

// TestMetadataErrorsCarryNoInput (M-noinput): no error of the metadata
// reads carries their project, a notice's content or the time they were
// given, and a time the store cannot hold is ErrInvalid, as AdmitNotice
// makes it (F-q2).
func TestMetadataErrorsCarryNoInput(t *testing.T) {
	t.Parallel()
	const mark = "MARK"
	project := "proj" + mark
	s := open(t, dbPath(t))
	ctx := t.Context()
	var errs []error
	expect := func(target error) func(any, error) {
		return func(_ any, err error) {
			t.Helper()
			require.ErrorIs(t, err, target)
			errs = append(errs, err)
		}
	}

	expect(store.ErrInvalid)(s.PendingNotices(ctx, project, 0))
	expect(store.ErrInvalid)(s.PendingNotices(ctx, project, store.MaxBoardRead+1))
	expect(store.ErrInvalid)(s.NoticeStats(ctx, project, time.Time{}))
	assert.NotContains(t, errs[len(errs)-1].Error(), "0001-01-01", "the zero time is not formatted")
	expect(store.ErrInvalid)(s.NoticeStats(ctx, project, time.Date(2263, 1, 1, 0, 0, 0, 0, time.FixedZone("zone"+mark, 3600))))
	assert.NotContains(t, errs[len(errs)-1].Error(), "2263", "a time past what the store holds is not formatted")

	e := admissible(t, func(e *store.NoticeEnvelope) {
		e.Project, e.Subject, e.Worker, e.Text = project, "subj"+mark, "work"+mark, "text"+mark
	})
	admit(t, s, e)
	db := store.DB(s)
	_, err := db.ExecContext(ctx, "DELETE FROM notice_payloads")
	require.NoError(t, err)
	expect(store.ErrStore)(s.PendingNotices(ctx, project, store.MaxBoardRead))
	_, err = db.ExecContext(ctx, "DELETE FROM notice_counts")
	require.NoError(t, err)
	expect(store.ErrStore)(s.NoticeStats(ctx, project, ledgerAt))
	_, err = db.ExecContext(ctx, "INSERT INTO notice_counts (project, identities) VALUES (?, 1)", project)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "DELETE FROM notice_store_counts")
	require.NoError(t, err)
	expect(store.ErrStore)(s.NoticeStats(ctx, project, ledgerAt))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	expect(context.Canceled)(s.PendingNotices(cancelled, project, store.MaxBoardRead))
	expect(context.Canceled)(s.NoticeStats(cancelled, project, ledgerAt))
	for _, err := range errs[len(errs)-2:] {
		require.ErrorIs(t, err, store.ErrStore, "a cancelled read is the store's failure")
	}
	require.NoError(t, s.Close())
	expect(store.ErrStore)(s.PendingNotices(ctx, project, store.MaxBoardRead))
	expect(store.ErrStore)(s.NoticeStats(ctx, project, ledgerAt))

	for i, err := range errs {
		assert.NotContains(t, err.Error(), mark, "error %d: %v", i, err)
	}
}

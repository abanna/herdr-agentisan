package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// The ledger tests' times: ledgerSeen when github_seen was last observed,
// ledgerAt when a notice is admitted, after it.
var (
	ledgerSeen = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ledgerAt   = time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)
)

const (
	ledgerRepo = "abanna/herdr-agentisan"
	headOID    = "1e0fb1b0c8d2a7f3e4b5c6d7e8f90a1b2c3d4e5f"
)

// admissible is a valid GitHub notice, changed by each change, with the
// capture ID GitHubNoticeCaptureID derives for it.
func admissible(t *testing.T, change ...func(*store.NoticeEnvelope)) store.NoticeEnvelope {
	t.Helper()
	e := store.NoticeEnvelope{
		Version: 1, Project: "ha", Source: "github", Kind: "ci_failed", Subject: ledgerRepo + "#50", Worker: "ha-coder-3",
		ObservedAt: ledgerAt, Text: "#50 CI failed at 1e0fb1b (2 of 7 checks)",
		Receipt: &store.NoticeReceipt{Version: 1, Repo: ledgerRepo, PR: 50, Kind: "ci_failed", Key: headOID},
	}
	for _, c := range change {
		c(&e)
	}
	id, err := store.GitHubNoticeCaptureID(e)
	require.NoError(t, err)
	e.CaptureID = id
	return e
}

func withKey(key string) func(*store.NoticeEnvelope) {
	return func(e *store.NoticeEnvelope) { e.Receipt.Key = key }
}

func admit(t *testing.T, s *store.Store, e store.NoticeEnvelope) store.NoticeAdmission {
	t.Helper()
	got, err := s.AdmitNotice(t.Context(), e, ledgerAt)
	require.NoError(t, err)
	return got
}

// ledgerRows counts the ledger's identities and payloads.
func ledgerRows(t *testing.T, s *store.Store) [2]int {
	t.Helper()
	var n [2]int
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(),
		"SELECT (SELECT count(*) FROM notice_identities), (SELECT count(*) FROM notice_payloads)").Scan(&n[0], &n[1]))
	return n
}

// seen records events in project's github_seen, as the typed path does once
// it told them, at ledgerSeen.
func seen(t *testing.T, s *store.Store, project string, events ...store.GitHubEvent) {
	t.Helper()
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: project, Repo: ledgerRepo, Seen: events}, ledgerSeen))
}

// wholeTables reads each table of schema 9, whole, in rowid order: one
// fixed query a table, since SQL takes no table name as a parameter.
// otherTables fails on a table missing here, so a new one is never skipped.
var wholeTables = map[string]string{
	"board_marks":         "SELECT * FROM board_marks ORDER BY rowid",
	"board_posts":         "SELECT * FROM board_posts ORDER BY rowid",
	"focus":               "SELECT * FROM focus ORDER BY rowid",
	"github_open":         "SELECT * FROM github_open ORDER BY rowid",
	"github_seeded":       "SELECT * FROM github_seeded ORDER BY rowid",
	"github_seeded_kinds": "SELECT * FROM github_seeded_kinds ORDER BY rowid",
	"github_seen":         "SELECT * FROM github_seen ORDER BY rowid",
	"handoffs":            "SELECT * FROM handoffs ORDER BY rowid",
	"messages":            "SELECT * FROM messages ORDER BY rowid",
	"notice_payloads":     "SELECT * FROM notice_payloads ORDER BY rowid",
	"sqlite_sequence":     "SELECT * FROM sqlite_sequence ORDER BY rowid",
	"workers":             "SELECT * FROM workers ORDER BY rowid",
}

// tableRows is every row of table, in rowid order, each its columns' values.
func tableRows(t *testing.T, db *sql.DB, table string) [][]any {
	t.Helper()
	query, ok := wholeTables[table]
	require.True(t, ok, "wholeTables has a query for %s", table)
	return queryRows(t, db, query)
}

// queryRows is every row query returns, each its columns' values.
func queryRows(t *testing.T, db *sql.DB, query string, args ...any) [][]any {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // read-only test query
	cols, err := rows.Columns()
	require.NoError(t, err)
	out := [][]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		out = append(out, vals)
	}
	require.NoError(t, rows.Err())
	return out
}

// otherTables is every row of every table in db but the ledger's, by table.
func otherTables(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	out := map[string][][]any{}
	for _, row := range queryRows(t, db, "SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT IN ('notice_identities', 'notice_payloads')") {
		name, ok := row[0].(string)
		require.True(t, ok, "a table's name: %v", row[0])
		out[name] = tableRows(t, db, name)
	}
	return out
}

// TestAdmitHasNoEffect (A1): admitting writes nothing but the ledger, not
// even github_seen's last_seen on an import, and wakes no board reader.
func TestAdmitHasNoEffect(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	seedWorker(t, s, "w", "w14:p2", 1)
	send(t, s, worker("w"), boss, "before the ledger")
	post(t, s, teamPost("ha", "w", "before the ledger"))
	require.NoError(t, s.UpdateGitHub(ctx, store.GitHubUpdate{Project: "ha", Repo: ledgerRepo, Collected: true, SeedKinds: []string{"review"}, Open: []int{50}}, ledgerSeen))
	seen(t, s, "ha", store.GitHubEvent{PR: 50, Kind: "merged", Key: headOID})
	before := otherTables(t, store.DB(s))
	require.Len(t, before["github_seen"], 1)
	woken := s.BoardChanged()

	first := admissible(t)
	admit(t, s, first)
	admit(t, s, first)
	admit(t, s, admissible(t, func(e *store.NoticeEnvelope) { e.ObservedAt = e.ObservedAt.Add(time.Second) }))
	imported := admit(t, s, admissible(t, func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "merged", "merged" }))
	require.True(t, imported.Imported)
	bad := admissible(t, withKey(strings.Repeat("a", 40)))
	bad.CaptureID = first.CaptureID
	_, err := s.AdmitNotice(ctx, bad, ledgerAt)
	require.ErrorIs(t, err, store.ErrInvalid)

	assert.Equal(t, before, otherTables(t, store.DB(s)), "no table but the ledger's changed")
	assert.Equal(t, [2]int{2, 1}, ledgerRows(t, s))
	select {
	case <-woken:
		t.Fatal("an admission woke the board's readers")
	default:
	}
}

// TestAdmitIsAtomic (A2): a payload that cannot be written leaves no
// identity behind.
func TestAdmitIsAtomic(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	_, err := db.ExecContext(t.Context(), "CREATE TEMP TRIGGER refuse_payload BEFORE INSERT ON main.notice_payloads BEGIN SELECT RAISE(ABORT, 'refused by the test'); END")
	require.NoError(t, err)

	e := admissible(t)
	_, err = s.AdmitNotice(t.Context(), e, ledgerAt)
	require.ErrorIs(t, err, store.ErrStore)
	assert.Equal(t, [2]int{0, 0}, ledgerRows(t, s), "the identity went with its payload")

	_, err = db.ExecContext(t.Context(), "DROP TRIGGER temp.refuse_payload")
	require.NoError(t, err)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.AdmitNotice(cancelled, e, ledgerAt)
	require.ErrorIs(t, err, store.ErrStore)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, [2]int{0, 0}, ledgerRows(t, s), "a cancelled admission writes nothing")
	assert.Equal(t, store.NoticeAdmission{CaptureID: e.CaptureID, State: "pending"}, admit(t, s, e), "nothing was left to make it a duplicate")
}

// TestAdmitDedupesTheCapture (A3): the same notice again, even from many
// goroutines at once, is admitted once.
func TestAdmitDedupesTheCapture(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	e := admissible(t)
	assert.Equal(t, store.NoticeAdmission{CaptureID: e.CaptureID, State: "pending"}, admit(t, s, e))
	assert.Equal(t, store.NoticeAdmission{CaptureID: e.CaptureID, State: "pending", Duplicate: true}, admit(t, s, e))
	assert.Equal(t, [2]int{1, 1}, ledgerRows(t, s))

	seen(t, s, "ha", store.GitHubEvent{PR: 51, Kind: "ci_failed", Key: headOID})
	imp := admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = 51 })
	assert.Equal(t, store.NoticeAdmission{CaptureID: imp.CaptureID, State: "imported", Imported: true}, admit(t, s, imp))
	assert.Equal(t, store.NoticeAdmission{CaptureID: imp.CaptureID, State: "imported", Duplicate: true}, admit(t, s, imp))

	racing := admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = 52 })
	var wg sync.WaitGroup
	results := make([]store.NoticeAdmission, 8)
	for i := range results {
		wg.Go(func() {
			got, err := s.AdmitNotice(t.Context(), racing, ledgerAt)
			assert.NoError(t, err)
			results[i] = got
		})
	}
	wg.Wait()
	fresh := 0
	for _, got := range results {
		assert.Equal(t, racing.CaptureID, got.CaptureID)
		if !got.Duplicate {
			fresh++
		}
	}
	assert.Equal(t, 1, fresh, "exactly one of the racing admissions is not a duplicate")
	assert.Equal(t, [2]int{3, 2}, ledgerRows(t, s))
}

// TestAdmitDedupesTheReceipt (A4): the same GitHub event observed again, or
// told in other words, is a duplicate of the first capture, whose payload
// stays as it was.
func TestAdmitDedupesTheReceipt(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	first := admissible(t)
	admit(t, s, first)
	stored := tableRows(t, store.DB(s), "notice_payloads")

	for name, change := range map[string]func(*store.NoticeEnvelope){
		"observed later": func(e *store.NoticeEnvelope) { e.ObservedAt = e.ObservedAt.Add(time.Hour) },
		"told otherwise": func(e *store.NoticeEnvelope) { e.Text = "#50 CI failed at 1e0fb1b (3 of 7 checks)" },
		"another worker": func(e *store.NoticeEnvelope) { e.Worker = "ha-coder-1" },
	} {
		again := admissible(t, change)
		require.NotEqual(t, first.CaptureID, again.CaptureID, name)
		assert.Equal(t, store.NoticeAdmission{CaptureID: first.CaptureID, State: "pending", Duplicate: true}, admit(t, s, again), name)
		_, err := s.PendingNotice(t.Context(), again.CaptureID)
		require.ErrorIs(t, err, store.ErrNoNotice, "%s: the second capture is not stored", name)
	}
	assert.Equal(t, [2]int{1, 1}, ledgerRows(t, s))
	assert.Equal(t, stored, tableRows(t, store.DB(s), "notice_payloads"), "the first capture's payload is byte for byte as admitted")
}

// TestAdmitImportsExactlySeen (A5): an event github_seen holds is recorded
// imported, with no payload. Anything differing in one byte, case
// included, or a prefix either way, is not that event.
func TestAdmitImportsExactlySeen(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	const review = "chatgpt-codex-connector 2026-11-16T08:59:00.123Z"
	long := headOID + "0123456789abcdef01234567"
	seen(t, s, "ha",
		store.GitHubEvent{PR: 50, Kind: "ci_failed", Key: headOID},
		store.GitHubEvent{PR: 50, Kind: "review", Key: review},
		store.GitHubEvent{PR: 50, Kind: "dirty", Key: long},
		store.GitHubEvent{PR: 50, Kind: "approved", Key: ""},
	)
	seen(t, s, "café", store.GitHubEvent{PR: 50, Kind: "ci_failed", Key: headOID})
	require.NoError(t, s.UpdateGitHub(t.Context(), store.GitHubUpdate{Project: "ha", Repo: "ownér/repo", Seen: []store.GitHubEvent{
		{PR: 50, Kind: "ci_failed", Key: headOID}, {PR: 50, Kind: "review", Key: "jöhn 2026-11-16T08:59:00Z"},
	}}, ledgerSeen))
	repo := func(r string) func(*store.NoticeEnvelope) {
		return func(e *store.NoticeEnvelope) { e.Receipt.Repo = r }
	}
	project := func(p string) func(*store.NoticeEnvelope) {
		return func(e *store.NoticeEnvelope) { e.Project = p }
	}
	kind := func(k string) func(*store.NoticeEnvelope) {
		return func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = k, k }
	}
	for name, tc := range map[string]struct {
		change   []func(*store.NoticeEnvelope)
		imported bool
	}{
		"the head commit seen":    {imported: true},
		"the review seen":         {change: []func(*store.NoticeEnvelope){kind("review"), withKey(review)}, imported: true},
		"the approval seen":       {change: []func(*store.NoticeEnvelope){kind("approved"), withKey("")}, imported: true},
		"one hex digit off":       {change: []func(*store.NoticeEnvelope){withKey(headOID[:39] + "e")}},
		"the oid in capitals":     {change: []func(*store.NoticeEnvelope){withKey(strings.ToUpper(headOID))}},
		"the login in capitals":   {change: []func(*store.NoticeEnvelope){kind("review"), withKey("ChatGPT-codex-connector 2026-11-16T08:59:00.123Z")}},
		"a seen key's extension":  {change: []func(*store.NoticeEnvelope){withKey(headOID[:39] + "f" + strings.Repeat("0", 24))}},
		"a prefix of a seen key":  {change: []func(*store.NoticeEnvelope){kind("dirty"), withKey(long[:40])}},
		"another kind":            {change: []func(*store.NoticeEnvelope){kind("ci_passed")}},
		"another PR":              {change: []func(*store.NoticeEnvelope){func(e *store.NoticeEnvelope) { e.Receipt.PR = 5 }}},
		"another project":         {change: []func(*store.NoticeEnvelope){project("HA")}},
		"the project in NFC":      {change: []func(*store.NoticeEnvelope){project("café")}, imported: true},
		"the project in NFD":      {change: []func(*store.NoticeEnvelope){project("cafe\u0301")}},
		"a LIKE wildcard":         {change: []func(*store.NoticeEnvelope){project("h_")}},
		"a LIKE match-all":        {change: []func(*store.NoticeEnvelope){project("%")}},
		"a GLOB match-all":        {change: []func(*store.NoticeEnvelope){project("*")}},
		"the repo in NFC":         {change: []func(*store.NoticeEnvelope){repo("ownér/repo")}, imported: true},
		"the repo in NFD":         {change: []func(*store.NoticeEnvelope){repo("owne\u0301r/repo")}},
		"the login in NFC":        {change: []func(*store.NoticeEnvelope){repo("ownér/repo"), kind("review"), withKey("jöhn 2026-11-16T08:59:00Z")}, imported: true},
		"the login in NFD":        {change: []func(*store.NoticeEnvelope){repo("ownér/repo"), kind("review"), withKey("jo\u0308hn 2026-11-16T08:59:00Z")}},
		"the repo in capitals":    {change: []func(*store.NoticeEnvelope){func(e *store.NoticeEnvelope) { e.Receipt.Repo = "Abanna/herdr-agentisan" }}},
		"the repo with a postfix": {change: []func(*store.NoticeEnvelope){func(e *store.NoticeEnvelope) { e.Receipt.Repo = ledgerRepo + "-x" }}},
	} {
		e := admissible(t, tc.change...)
		before := ledgerRows(t, s)
		got := admit(t, s, e)
		_, err := s.PendingNotice(t.Context(), e.CaptureID)
		if tc.imported {
			assert.Equal(t, store.NoticeAdmission{CaptureID: e.CaptureID, State: "imported", Imported: true}, got, name)
			assert.Equal(t, [2]int{before[0] + 1, before[1]}, ledgerRows(t, s), "%s: an import has no payload", name)
			require.ErrorIs(t, err, store.ErrNoNotice, name)
			continue
		}
		assert.Equal(t, store.NoticeAdmission{CaptureID: e.CaptureID, State: "pending"}, got, name)
		assert.Equal(t, [2]int{before[0] + 1, before[1] + 1}, ledgerRows(t, s), name)
		require.NoError(t, err, name)
	}
}

// TestAdmitChecksTheSuppliedID (A6), with deviation (d): only a valid
// GitHub notice with a project and the capture ID its content derives is
// admitted; anything else is ErrInvalid and writes nothing.
func TestAdmitChecksTheSuppliedID(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	right := admissible(t)
	digest := strings.TrimPrefix(right.CaptureID, "gh1:")
	with := func(change func(*store.NoticeEnvelope)) store.NoticeEnvelope {
		e, receipt := right, *right.Receipt
		e.Receipt = &receipt
		change(&e)
		return e
	}
	refused := func(name string, e store.NoticeEnvelope, at time.Time) {
		got, err := s.AdmitNotice(t.Context(), e, at)
		require.ErrorIs(t, err, store.ErrInvalid, name)
		assert.Equal(t, store.NoticeAdmission{}, got, name)
	}
	for name, e := range map[string]store.NoticeEnvelope{
		"another notice's ID": with(func(e *store.NoticeEnvelope) { e.Receipt.Key = strings.Repeat("b", 40) }),
		"no ID":               with(func(e *store.NoticeEnvelope) { e.CaptureID = "" }),
		"another version":     with(func(e *store.NoticeEnvelope) { e.CaptureID = "gh2:" + digest }),
		"an allocated ID":     with(func(e *store.NoticeEnvelope) { e.CaptureID = "nt1:01KAB3XG8CJ5V4WQ4QG3SZZ5PZ" }),
		"the digest in caps":  with(func(e *store.NoticeEnvelope) { e.CaptureID = "gh1:" + strings.ToUpper(digest) }),
		"the daemon's source": with(func(e *store.NoticeEnvelope) { e.Source = "daemon" }),
		"the team's source":   with(func(e *store.NoticeEnvelope) { e.Source = "team" }),
		"no project":          admissible(t, func(e *store.NoticeEnvelope) { e.Project = "" }),
		"an invalid envelope": with(func(e *store.NoticeEnvelope) { e.Text = "two\nlines" }),
	} {
		refused(name, e, ledgerAt)
	}
	for name, change := range map[string]func(*store.NoticeEnvelope){
		"no receipt":   func(e *store.NoticeEnvelope) { e.Receipt = nil },
		"PR 0":         func(e *store.NoticeEnvelope) { e.Receipt.PR = 0 },
		"PR -1":        func(e *store.NoticeEnvelope) { e.Receipt.PR = -1 },
		"PR 2^31":      func(e *store.NoticeEnvelope) { e.Receipt.PR = 1 << 31 },
		"a short oid":  func(e *store.NoticeEnvelope) { e.Receipt.Key = headOID[:7] },
		"an oid of 41": func(e *store.NoticeEnvelope) { e.Receipt.Key = headOID + "0" },
		"a review time not canonical": func(e *store.NoticeEnvelope) {
			e.Kind, e.Receipt.Kind, e.Receipt.Key = "review", "review", "alice 2026-11-16T08:59:00.10Z"
		},
		"a BOM leading the project": func(e *store.NoticeEnvelope) { e.Project = "\ufeffha" },
		"a source in capitals":      func(e *store.NoticeEnvelope) { e.Source = "GitHub" },
		"no source":                 func(e *store.NoticeEnvelope) { e.Source = "" },
		"an unknown kind":           func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "future", "future" },
		"a kind in capitals":        func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "CI_FAILED", "CI_FAILED" },
		"no kind":                   func(e *store.NoticeEnvelope) { e.Kind, e.Receipt.Kind = "", "" },
		"a receipt of another kind": func(e *store.NoticeEnvelope) { e.Receipt.Kind = "ci_passed" },
		"no text":                   func(e *store.NoticeEnvelope) { e.Text = "" },
		"no repo":                   func(e *store.NoticeEnvelope) { e.Receipt.Repo = "" },
		"a project of 257 bytes":    func(e *store.NoticeEnvelope) { e.Project = strings.Repeat("p", 257) },
		"a subject of 257 bytes":    func(e *store.NoticeEnvelope) { e.Subject = strings.Repeat("s", 257) },
		"a worker of 257 bytes":     func(e *store.NoticeEnvelope) { e.Worker = strings.Repeat("w", 257) },
		"a repo of 257 bytes":       func(e *store.NoticeEnvelope) { e.Receipt.Repo = "o/" + strings.Repeat("r", 255) },
		"a login of 129 bytes": func(e *store.NoticeEnvelope) {
			e.Kind, e.Receipt.Kind, e.Receipt.Key = "review", "review", strings.Repeat("l", 129)+" 2026-11-16T08:59:00Z"
		},
		"version 0":                 func(e *store.NoticeEnvelope) { e.Version = 0 },
		"version 2":                 func(e *store.NoticeEnvelope) { e.Version = 2 },
		"the largest version":       func(e *store.NoticeEnvelope) { e.Version = math.MaxUint64 },
		"receipt version 2":         func(e *store.NoticeEnvelope) { e.Receipt.Version = 2 },
		"observed at the zero time": func(e *store.NoticeEnvelope) { e.ObservedAt = time.Time{} },
		"an ID not UTF-8":           func(e *store.NoticeEnvelope) { e.CaptureID = "gh1:\xff" },
	} {
		refused(name, with(change), ledgerAt)
	}
	// Every field B0a.1 bounds is refused through AdmitNotice as the
	// capture ID refuses it: bytes that are not UTF-8, a truncated or
	// surrogate sequence, Latin-1, NUL, line breaks, and one byte too many.
	for _, bad := range []string{"\xff", "\xc3", "\xed\xa0\x80", "caf\xe9", "é\xff", "a\x00b", "a\r\nb", "a\rb", "a\nb", strings.Repeat("é", 256) + "x"} {
		for field, set := range map[string]func(*store.NoticeEnvelope){
			"project": func(e *store.NoticeEnvelope) { e.Project = bad },
			"subject": func(e *store.NoticeEnvelope) { e.Subject = bad },
			"worker":  func(e *store.NoticeEnvelope) { e.Worker = bad },
			"text":    func(e *store.NoticeEnvelope) { e.Text = bad },
			"repo":    func(e *store.NoticeEnvelope) { e.Receipt.Repo = "own/" + bad },
			"login": func(e *store.NoticeEnvelope) {
				e.Kind, e.Receipt.Kind, e.Receipt.Key = "review", "review", bad+" 2026-11-16T08:59:00Z"
			},
		} {
			refused(fmt.Sprintf("%s %q", field, bad), with(set), ledgerAt)
		}
	}
	last := time.Unix(0, math.MaxInt64)
	refused("admitted at the zero time", right, time.Time{})
	refused("admitted in 2263", right, time.Date(2263, 1, 1, 0, 0, 0, 0, time.UTC))
	refused("admitted a nanosecond past the last time held", right, last.Add(time.Nanosecond))
	assert.Equal(t, [2]int{0, 0}, ledgerRows(t, s))
	for i, at := range []time.Time{last, time.Unix(-1, 0)} {
		e := admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = int64(60 + i) })
		got, err := s.AdmitNotice(t.Context(), e, at)
		require.NoError(t, err, "admitted at %v", at)
		assert.Equal(t, "pending", got.State)
	}
	assert.Equal(t, "pending", admit(t, s, right).State, "the right notice is admitted")
}

// TestPendingNoticeRoundTrip (A7): a pending notice reads back as it was
// admitted, its time in UTC, and its content still derives its ID.
func TestPendingNoticeRoundTrip(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	zone := time.FixedZone("UTC-6", -6*3600)
	for _, e := range []store.NoticeEnvelope{
		admissible(t),
		admissible(t, func(e *store.NoticeEnvelope) {
			e.Kind, e.Receipt.Kind, e.Receipt.Key = "review", "review", "alice 2026-11-16T08:59:00.000000001Z"
			e.Subject, e.Worker, e.Text = "", "", "#50 review by alice at 1e0fb1b: commented, P1 ×2 — é 主体"
			e.ObservedAt = time.Unix(0, -1).In(zone)
		}),
		admissible(t, func(e *store.NoticeEnvelope) {
			e.Kind, e.Receipt.Kind, e.Receipt.Key, e.Receipt.PR = "merged", "merged", "", 1<<31-1
			e.ObservedAt = ledgerAt.In(zone)
		}),
		admissible(t, func(e *store.NoticeEnvelope) {
			e.Kind, e.Receipt.Kind, e.Receipt.Key = "dirty", "dirty", strings.Repeat("C", 64)
		}),
		admissible(t, func(e *store.NoticeEnvelope) {
			e.Project, e.Subject, e.Worker = "é-team'; DROP TABLE notice_payloads; --", "o/r#1 %_*[?]", "wörker \"quoted\""
			e.Text, e.Receipt.Repo = "\ufeffa BOM-led text, kept as given", "ownér/repö"
			e.Kind, e.Receipt.Kind, e.Receipt.Key = "review", "review", "jöhn-é 2026-11-16T08:59:00Z"
		}),
		admissible(t, func(e *store.NoticeEnvelope) {
			e.Project, e.Subject, e.Worker = strings.Repeat("p", 256), strings.Repeat("s", 256), strings.Repeat("w", 256)
			e.Text, e.Receipt.Repo = strings.Repeat("é", 256), "o/"+strings.Repeat("r", 254)
			e.Kind, e.Receipt.Kind, e.Receipt.Key = "review", "review", strings.Repeat("l", 128)+" 2026-11-16T08:59:00.123456789Z"
		}),
	} {
		admit(t, s, e)
		got, err := s.PendingNotice(t.Context(), e.CaptureID)
		require.NoError(t, err)
		assert.True(t, e.ObservedAt.Equal(got.ObservedAt), "%v read back as %v", e.ObservedAt, got.ObservedAt)
		assert.Equal(t, time.UTC, got.ObservedAt.Location())
		id, err := store.GitHubNoticeCaptureID(got)
		require.NoError(t, err)
		assert.Equal(t, e.CaptureID, id)
		e.ObservedAt, got.ObservedAt = time.Time{}, time.Time{}
		assert.Equal(t, e, got)
		_, err = s.PendingNotice(t.Context(), strings.ToUpper(e.CaptureID))
		require.ErrorIs(t, err, store.ErrNoNotice, "an ID differing in case is another")
	}
}

// TestPendingNoticeRefusesTampering (A8): a pending notice whose rows were
// edited by hand is ErrStore, whichever column was edited; a notice not
// pending, or unknown, is ErrNoNotice.
func TestPendingNoticeRefusesTampering(t *testing.T) {
	t.Parallel()
	// other differs only in its text, of the same length: written with its
	// own digest, only the capture ID tells the rows from what was admitted.
	other := admissible(t, func(e *store.NoticeEnvelope) { e.Text = "#50 CI failed at 1e0fb1b (3 of 7 checks)" })
	require.Len(t, other.Text, len(admissible(t).Text))
	otherDigest := strings.TrimPrefix(other.CaptureID, "gh1:")
	for name, tc := range map[string]struct {
		edit string
		args []any
		want error
	}{
		"the text":            {edit: "UPDATE notice_payloads SET text = 'x'", want: store.ErrStore},
		"the subject":         {edit: "UPDATE notice_payloads SET subject = 'abanna/herdr-agentisan#51'", want: store.ErrStore},
		"the worker":          {edit: "UPDATE notice_payloads SET worker = 'ha-coder-1'", want: store.ErrStore},
		"the time":            {edit: "UPDATE notice_payloads SET observed_at = observed_at + 1", want: store.ErrStore},
		"the digest":          {edit: "UPDATE notice_payloads SET digest = ?", args: []any{otherDigest}, want: store.ErrStore},
		"the size":            {edit: "UPDATE notice_payloads SET bytes = bytes + 1", want: store.ErrStore},
		"the receipt version": {edit: "UPDATE notice_payloads SET receipt_version = NULL", want: store.ErrStore},
		"another text, whole": {edit: "UPDATE notice_payloads SET text = ?, digest = ?", args: []any{other.Text, otherDigest}, want: store.ErrStore},
		"the key":             {edit: "UPDATE notice_identities SET receipt_key = ?", args: []any{strings.Repeat("d", 40)}, want: store.ErrStore},
		"the project":         {edit: "UPDATE notice_identities SET project = 'ha2'", want: store.ErrStore},
		"the PR":              {edit: "UPDATE notice_identities SET pr = 51", want: store.ErrStore},
		"the kind":            {edit: "UPDATE notice_identities SET kind = 'ci_passed'", want: store.ErrStore},
		"the source":          {edit: "UPDATE notice_identities SET source = 'daemon', repo = NULL, pr = NULL, receipt_key = NULL", want: store.ErrStore},
		"the payload gone":    {edit: "DELETE FROM notice_payloads", want: store.ErrStore},
		"text not UTF-8":      {edit: "UPDATE notice_payloads SET text = CAST(X'FF' AS TEXT)", want: store.ErrStore},
		"imported":            {edit: "UPDATE notice_identities SET state = 'imported'; DELETE FROM notice_payloads", want: store.ErrNoNotice},
		"posted":              {edit: "UPDATE notice_identities SET state = 'posted', settled_at = 1, post_seq = 1", want: store.ErrNoNotice},
		"dropped":             {edit: "UPDATE notice_identities SET state = 'dropped', settled_at = 1, drop_reason = 'muted'", want: store.ErrNoNotice},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			e := admissible(t)
			admit(t, s, e)
			_, err := store.DB(s).ExecContext(t.Context(), tc.edit, tc.args...)
			require.NoError(t, err)
			got, err := s.PendingNotice(t.Context(), e.CaptureID)
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, store.NoticeEnvelope{}, got)
		})
	}
	s := open(t, dbPath(t))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.PendingNotice(cancelled, admissible(t).CaptureID)
	require.ErrorIs(t, err, store.ErrStore, "a cancelled read")
	for _, id := range []string{admissible(t).CaptureID, "", "\xff", "gh1:%"} {
		got, err := s.PendingNotice(t.Context(), id)
		require.ErrorIs(t, err, store.ErrNoNotice, "never admitted: %q", id)
		assert.Equal(t, store.NoticeEnvelope{}, got)
	}
}

// TestLedgerErrorsCarryNoInput (A9): no error of the ledger's carries the
// notice it was given, its project, repo, key, text or time zone included.
func TestLedgerErrorsCarryNoInput(t *testing.T) {
	t.Parallel()
	const mark = "MARK"
	e := admissible(t, func(e *store.NoticeEnvelope) {
		e.Project, e.Subject, e.Worker, e.Text = "proj"+mark, "subj"+mark, "work"+mark, "text"+mark
		e.Kind, e.Receipt.Kind, e.Receipt.Repo, e.Receipt.Key = "review", "review", "own"+mark+"/repo"+mark, "login"+mark+" 2026-11-16T08:59:00Z"
	})
	s := open(t, dbPath(t))
	var errs []error
	try := func(e store.NoticeEnvelope, at time.Time) {
		_, err := s.AdmitNotice(t.Context(), e, at)
		require.Error(t, err)
		errs = append(errs, err)
	}
	invalid := e
	invalid.Text = "text" + mark + "\n"
	try(invalid, ledgerAt)
	wrong := e
	wrong.CaptureID = "gh1:" + mark
	try(wrong, ledgerAt)
	try(e, time.Date(2263, 1, 1, 0, 0, 0, 0, time.FixedZone("zone"+mark, 3600)))
	_, err := store.DB(s).ExecContext(t.Context(), "CREATE TEMP TRIGGER refuse_payload BEFORE INSERT ON main.notice_payloads BEGIN SELECT RAISE(ABORT, 'refused by the test'); END")
	require.NoError(t, err)
	try(e, ledgerAt)
	_, err = store.DB(s).ExecContext(t.Context(), "DROP TRIGGER temp.refuse_payload")
	require.NoError(t, err)

	admit(t, s, e)
	_, err = s.PendingNotice(t.Context(), "gh1:"+mark)
	errs = append(errs, err)
	_, err = store.DB(s).ExecContext(t.Context(), "UPDATE notice_payloads SET text = 'x'")
	require.NoError(t, err)
	_, err = s.PendingNotice(t.Context(), e.CaptureID)
	errs = append(errs, err)
	require.NoError(t, s.Close())
	try(e, ledgerAt)
	_, err = s.PendingNotice(t.Context(), e.CaptureID)
	errs = append(errs, err)

	for i, err := range errs {
		require.Error(t, err, i)
		assert.NotContains(t, err.Error(), mark, "error %d: %v", i, err)
	}
}

// TestAdmitStepOrder (A12): a notice is validated before it is deduped,
// and deduped before it is imported.
func TestAdmitStepOrder(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	first := admissible(t)
	admit(t, s, first)
	seen(t, s, "ha", store.GitHubEvent{PR: 50, Kind: "ci_failed", Key: headOID})

	later := admissible(t, func(e *store.NoticeEnvelope) { e.ObservedAt = e.ObservedAt.Add(time.Minute) })
	assert.Equal(t, store.NoticeAdmission{CaptureID: first.CaptureID, State: "pending", Duplicate: true}, admit(t, s, later),
		"a pending receipt github_seen also holds is a duplicate, not an import")

	wrongID := admissible(t, func(e *store.NoticeEnvelope) { e.ObservedAt = e.ObservedAt.Add(time.Minute) })
	wrongID.CaptureID = "gh1:" + strings.Repeat("0", 64)
	invalid := first
	invalid.Text = "two\nlines"
	for _, e := range []store.NoticeEnvelope{wrongID, invalid} {
		got, err := s.AdmitNotice(t.Context(), e, ledgerAt)
		require.ErrorIs(t, err, store.ErrInvalid, "an invalid notice matching a stored receipt is refused, not a duplicate")
		assert.Equal(t, store.NoticeAdmission{}, got)
	}
	assert.Equal(t, [2]int{1, 1}, ledgerRows(t, s))
}

// The ledger's two inserts, every column named, as a hand edit or a later
// slice would write them.
var (
	identityColumns = []string{"capture_id", "project", "source", "kind", "repo", "pr", "receipt_key", "state", "admitted_at", "settled_at", "post_seq", "drop_reason", "attempts"}
	insertIdentity  = `INSERT INTO notice_identities (capture_id, project, source, kind, repo, pr, receipt_key, state, admitted_at, settled_at, post_seq, drop_reason, attempts)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	payloadColumns = []string{"capture_id", "version", "receipt_version", "subject", "worker", "observed_at", "text", "bytes", "digest"}
	insertPayload  = `INSERT INTO notice_payloads (capture_id, version, receipt_version, subject, worker, observed_at, text, bytes, digest)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

// insertRow runs insert with row's value for each of columns, NULL for one
// row does not name; row names no other column.
func insertRow(t *testing.T, db *sql.DB, insert string, columns []string, row map[string]any) error {
	t.Helper()
	require.Subset(t, columns, slices.Collect(maps.Keys(row)), "a column the insert names")
	args := make([]any, len(columns))
	for i, c := range columns {
		args[i] = row[c]
	}
	_, err := db.ExecContext(t.Context(), insert, args...)
	return err
}

// constraintFailed asserts err is SQLite refusing a row by a constraint of
// the schema's, its column types included, not any other failure.
func constraintFailed(t *testing.T, err error, name string) {
	t.Helper()
	var e *sqlite.Error
	if assert.ErrorAs(t, err, &e, name) {
		assert.Equal(t, sqlite3.SQLITE_CONSTRAINT, e.Code()&0xff, "%s: %v", name, err)
	}
}

// TestLedgerSchemaChecks (A11): the schema itself refuses the rows the
// ledger must never hold, whoever writes them, and takes every row later
// slices will write: each source, each state, each drop reason.
func TestLedgerSchemaChecks(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	n := 0
	identity := func(over map[string]any) error {
		n++
		row := map[string]any{
			"capture_id": fmt.Sprintf("gh1:%064d", n), "project": "ha", "source": "github", "kind": "ci_failed",
			"repo": ledgerRepo, "pr": n, "receipt_key": headOID, "state": "pending", "admitted_at": 1, "attempts": 0,
		}
		maps.Copy(row, over)
		return insertRow(t, db, insertIdentity, identityColumns, row)
	}
	noReceipt := map[string]any{"repo": nil, "pr": nil, "receipt_key": nil}
	with := func(a map[string]any, b ...map[string]any) map[string]any {
		out := maps.Clone(a)
		for _, m := range b {
			maps.Copy(out, m)
		}
		return out
	}
	require.NoError(t, identity(map[string]any{"pr": 1}), "the first row has PR 1")
	for name, over := range map[string]map[string]any{
		"imported":             {"state": "imported"},
		"a daemon row":         with(noReceipt, map[string]any{"source": "daemon", "kind": "turn_ended"}),
		"another like it":      with(noReceipt, map[string]any{"source": "daemon", "kind": "turn_ended"}),
		"a team row":           with(noReceipt, map[string]any{"source": "team", "kind": "post"}),
		"posted":               {"state": "posted", "settled_at": 2, "post_seq": 7},
		"dropped, skipped":     {"state": "dropped", "settled_at": 2, "drop_reason": "skipped"},
		"dropped, muted":       {"state": "dropped", "settled_at": 2, "drop_reason": "muted"},
		"dropped, a repeat":    {"state": "dropped", "settled_at": 2, "drop_reason": "repeat"},
		"retried":              {"attempts": 3},
		"a kind of 64 bytes":   {"kind": strings.Repeat("é", 32)},
		"the largest PR":       {"pr": 1<<31 - 1},
		"PR 1 of another repo": {"pr": 1, "repo": "abanna/other"},
	} {
		require.NoError(t, identity(over), name)
	}
	for name, over := range map[string]map[string]any{
		"a NULL capture ID":                  {"capture_id": nil},
		"an empty capture ID":                {"capture_id": ""},
		"the first row's capture ID":         {"capture_id": fmt.Sprintf("gh1:%064d", 1), "pr": 99},
		"no project":                         {"project": nil},
		"an unknown source":                  {"source": "watch"},
		"an unknown state":                   {"state": "acked"},
		"no kind":                            {"kind": ""},
		"a kind of 65 bytes, 33 characters":  {"kind": strings.Repeat("é", 32) + "x"},
		"a GitHub row with no receipt":       noReceipt,
		"a daemon row with a receipt":        {"source": "daemon"},
		"a receipt with no PR":               {"pr": nil},
		"a receipt with no key":              {"receipt_key": nil},
		"a receipt with no repo":             {"repo": nil},
		"PR 0":                               {"pr": 0},
		"PR 2^31":                            {"pr": 1 << 31},
		"a PR as words":                      {"pr": "fifty"},
		"a pending row settled":              {"settled_at": 2},
		"an imported row settled":            {"state": "imported", "settled_at": 2},
		"a posted row unsettled":             {"state": "posted", "post_seq": 7},
		"a posted row with no post_seq":      {"state": "posted", "settled_at": 2},
		"a dropped row with no reason":       {"state": "dropped", "settled_at": 2},
		"a dropped row for no known reason":  {"state": "dropped", "settled_at": 2, "drop_reason": "lost"},
		"a dropped row with a post_seq":      {"state": "dropped", "settled_at": 2, "drop_reason": "muted", "post_seq": 7},
		"a pending row with a reason":        {"drop_reason": "muted"},
		"a pending row with a post_seq":      {"post_seq": 7},
		"negative attempts":                  {"attempts": -1},
		"no admission time":                  {"admitted_at": nil},
		"a second row with the same receipt": {"pr": 1},
	} {
		constraintFailed(t, identity(over), name)
	}

	require.NoError(t, identity(map[string]any{"capture_id": "gh1:with-payload"}))
	payload := func(over map[string]any) error {
		row := map[string]any{
			"capture_id": "gh1:with-payload", "version": 1, "receipt_version": 1, "subject": "", "worker": "",
			"observed_at": 1, "text": "x", "bytes": 1, "digest": strings.Repeat("0123456789abcdef", 4),
		}
		maps.Copy(row, over)
		return insertRow(t, db, insertPayload, payloadColumns, row)
	}
	for name, over := range map[string]map[string]any{
		"a payload with no identity":  {"capture_id": "gh1:nobody"},
		"a NULL capture ID":           {"capture_id": nil},
		"version 2":                   {"version": 2},
		"receipt version 2":           {"receipt_version": 2},
		"no text":                     {"text": ""},
		"no bytes":                    {"bytes": 0},
		"a digest of 63 characters":   {"digest": strings.Repeat("a", 63)},
		"a digest of 65 characters":   {"digest": strings.Repeat("a", 65)},
		"a digest in capitals":        {"digest": strings.Repeat("A", 64)},
		"a digest that is not hex":    {"digest": strings.Repeat("g", 64)},
		"a digest with a GLOB escape": {"digest": strings.Repeat("a", 63) + "]"},
	} {
		constraintFailed(t, payload(over), name)
	}
	require.NoError(t, payload(map[string]any{"receipt_version": nil}), "a payload with no receipt, as another source's")
	constraintFailed(t, payload(nil), "a second payload for one identity")
	_, err := db.ExecContext(t.Context(), "DELETE FROM notice_identities WHERE capture_id = 'gh1:with-payload'")
	constraintFailed(t, err, "an identity deleted from under its payload")

	// A store-wide count of one state, its state bound as B0a.3's quotas
	// bind it, walks only that state's rows (deviation c).
	plan := queryRows(t, db, "EXPLAIN QUERY PLAN SELECT count(*) FROM notice_identities WHERE state = ?", "pending")
	assert.Contains(t, fmt.Sprint(plan), "USING COVERING INDEX notice_identities_store_state (state=?)")
}

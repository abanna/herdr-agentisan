package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// assertNoticeTotals asserts 0010's running totals equal a recount of
// notice_identities: each project's, less the zero rows a delete or a move
// keeps (M5), none below 0, and the store's in its one row.
func assertNoticeTotals(t *testing.T, db *sql.DB) {
	t.Helper()
	assert.Equal(t,
		queryRows(t, db, "SELECT project, count(*) FROM notice_identities GROUP BY project ORDER BY project"),
		queryRows(t, db, "SELECT project, identities FROM notice_counts WHERE identities <> 0 ORDER BY project"),
		"each project's total is a recount of its identities")
	assert.Empty(t, queryRows(t, db, "SELECT project, identities FROM notice_counts WHERE identities < 0"), "no project's total is below 0")
	assert.Equal(t,
		queryRows(t, db, "SELECT 1, count(*) FROM notice_identities"),
		queryRows(t, db, "SELECT id, identities FROM notice_store_counts"),
		"the store's total is one row, a recount of every identity")
}

// noticeTotals is each project's total, its zero rows included, and the
// store's.
func noticeTotals(t *testing.T, db *sql.DB) (map[string]int64, int64) {
	t.Helper()
	var all int64
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT identities FROM notice_store_counts WHERE id = 1").Scan(&all))
	return byProject(t, queryRows(t, db, "SELECT project, identities FROM notice_counts")), all
}

// recounted is each project's identities and the store's, counted.
func recounted(t *testing.T, db *sql.DB) (map[string]int64, int64) {
	t.Helper()
	var all int64
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM notice_identities").Scan(&all))
	return byProject(t, queryRows(t, db, "SELECT project, count(*) FROM notice_identities GROUP BY project")), all
}

// byProject maps rows of a project and a number.
func byProject(t *testing.T, rows [][]any) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, row := range rows {
		project, ok := row[0].(string)
		require.True(t, ok, "a project: %v", row[0])
		n, ok := row[1].(int64)
		require.True(t, ok, "a number: %v", row[1])
		out[project] = n
	}
	return out
}

// importNotice imports a GitHub notice on pr in project ha through
// github_seen and AdmitNotice, as #163 does: an identity with no payload.
func importNotice(t *testing.T, s *store.Store, pr int) store.NoticeEnvelope {
	t.Helper()
	seen(t, s, "ha", store.GitHubEvent{PR: pr, Kind: "ci_failed", Key: headOID})
	e := admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = int64(pr) })
	require.True(t, admit(t, s, e).Imported, "PR %d's notice is imported", pr)
	return e
}

// replacement is one of K6's and K7's REPLACEs.
type replacement struct {
	name  string
	query string
	args  []any
	// identities is how many identities the store holds after it.
	identities int
}

// replacements imports four notices in project ha and admits one pending,
// then returns the three REPLACEs K6 and K7 run, in order. Each replaces
// one import, an identity with no payload: the only rows a REPLACE can
// delete, since a payload's foreign key is RESTRICT. Each replaced row is
// in project ha.
func replacements(t *testing.T, s *store.Store) []replacement {
	t.Helper()
	admit(t, s, admissible(t))
	imports := make([]store.NoticeEnvelope, 4)
	for i := range imports {
		imports[i] = importNotice(t, s, 60+i)
	}
	require.Equal(t, [2]int{5, 1}, ledgerRows(t, s))
	identity := func(captureID, project string, pr int) []any {
		return []any{captureID, project, "github", "ci_failed", ledgerRepo, pr, headOID, "imported", 1, nil, nil, nil, 0}
	}
	insertOrReplace := strings.Replace(insertIdentity, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
	return []replacement{
		{"INSERT OR REPLACE of an existing capture ID into another project", insertOrReplace, identity(imports[0].CaptureID, "hb", 60), 5},
		{"INSERT OR REPLACE colliding on the receipt", insertOrReplace, identity("gh1:replaces-a-receipt", "ha", 61), 5},
		{"UPDATE OR REPLACE colliding on the capture ID", "UPDATE OR REPLACE notice_identities SET capture_id = ? WHERE capture_id = ?", []any{imports[3].CaptureID, imports[2].CaptureID}, 4},
	}
}

// TestReplaceThroughTheStoreKeepsTheTotals (K6): on the store's own
// connection, which sets recursive_triggers, a REPLACE's hidden delete fires
// notice_counts_delete, so each REPLACE leaves the totals exact.
func TestReplaceThroughTheStoreKeepsTheTotals(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	for _, r := range replacements(t, s) {
		_, err := db.ExecContext(t.Context(), r.query, r.args...)
		require.NoError(t, err, r.name)
		require.Equal(t, r.identities, ledgerRows(t, s)[0], "%s replaced a row", r.name)
		assertNoticeTotals(t, db)
	}
	projects, _ := noticeTotals(t, db)
	assert.Equal(t, map[string]int64{"ha": 3, "hb": 1}, projects)
}

// recount is docs/architecture.md's repair of drifted totals, word for
// word, run on one connection.
const recount = `BEGIN IMMEDIATE;
DELETE FROM notice_counts;
INSERT INTO notice_counts (project, identities)
    SELECT project, count(*) FROM notice_identities GROUP BY project;
DELETE FROM notice_store_counts;
INSERT INTO notice_store_counts (id, identities)
    SELECT 1, count(*) FROM notice_identities;
COMMIT;`

// TestReplaceWithoutRecursiveTriggersOverCounts (K7) pins the limit 0010's
// comment and the docs state: on a connection without recursive_triggers, a
// sqlite3 shell's say, a REPLACE's hidden delete fires no trigger, so the
// replaced row's project total and the store's stay one too high for each
// replaced row. The docs' recount then repairs them.
func TestReplaceWithoutRecursiveTriggersOverCounts(t *testing.T) {
	t.Parallel()
	path := dbPath(t)
	s := open(t, path)
	fixes := replacements(t, s)

	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	conn, err := other.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close() //nolint:errcheck // closed with the test
	for pragma, want := range map[string]int{"busy_timeout": 5000, "foreign_keys": 1, "recursive_triggers": 0} {
		var got int
		require.NoError(t, conn.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got))
		require.Equal(t, want, got, "the other connection's %s", pragma)
	}

	db := store.DB(s)
	for i, r := range fixes {
		_, err := conn.ExecContext(t.Context(), r.query, r.args...)
		require.NoError(t, err, r.name)
		require.Equal(t, r.identities, ledgerRows(t, s)[0], "%s replaced a row", r.name)
		high := int64(i + 1)
		want, all := recounted(t, db)
		want["ha"] += high
		projects, total := noticeTotals(t, db)
		assert.Equal(t, want, projects, "after %s, ha's total is %d too high and no other is off", r.name, high)
		assert.Equal(t, all+high, total, "after %s, the store's total is %d too high", r.name, high)
	}

	_, err = conn.ExecContext(t.Context(), recount)
	require.NoError(t, err)
	assertNoticeTotals(t, db)
}

// daemonIdentity is a daemon-source identity in the empty project, as B0a.4
// may admit the ◆ spaces' notices: no receipt and no payload.
var daemonIdentity = map[string]any{
	"capture_id": "daemon:1", "project": "", "source": "daemon", "kind": "turn_ended",
	"state": "pending", "admitted_at": 1, "attempts": 0,
}

// TestNoticeCountsFollowEveryWrite (K1): the triggers keep each project's
// total and the store's equal to a recount through every write the ledger
// takes: an admission, an import, a daemon row in the empty project, a
// delete and three moves, into a project with no identities, into one with
// some, and of a project's last row. The project left empty keeps its row,
// at 0 (M5).
func TestNoticeCountsFollowEveryWrite(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	assertNoticeTotals(t, db)

	pending := admissible(t)
	require.Equal(t, "pending", admit(t, s, pending).State)
	assertNoticeTotals(t, db)
	toNew := importNotice(t, s, 51)
	assertNoticeTotals(t, db)
	deleted := importNotice(t, s, 52)
	last := importNotice(t, s, 53)
	require.Equal(t, "pending", admit(t, s, admissible(t, func(e *store.NoticeEnvelope) { e.Project, e.Receipt.PR = "hb", 60 })).State)
	assertNoticeTotals(t, db)
	require.NoError(t, insertRow(t, db, insertIdentity, identityColumns, daemonIdentity), "a daemon row in project ''")
	assertNoticeTotals(t, db)
	projects, total := noticeTotals(t, db)
	require.Equal(t, map[string]int64{"": 1, "ha": 4, "hb": 1}, projects)
	require.Equal(t, int64(6), total)

	move := "UPDATE notice_identities SET project = ? WHERE capture_id = ?"
	for _, step := range []struct {
		name  string
		query string
		args  []any
	}{
		{"the delete of an identity with no payload", "DELETE FROM notice_identities WHERE capture_id = ?", []any{deleted.CaptureID}},
		{"a move into a project with no identities yet", move, []any{"hc", toNew.CaptureID}},
		{"a move into a project that already has some", move, []any{"hb", pending.CaptureID}},
		{"a move of a project's last row out", move, []any{"hc", last.CaptureID}},
	} {
		res, err := db.ExecContext(t.Context(), step.query, step.args...)
		require.NoError(t, err, step.name)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "%s wrote one row", step.name)
		assertNoticeTotals(t, db)
	}
	projects, total = noticeTotals(t, db)
	assert.Equal(t, map[string]int64{"": 1, "ha": 0, "hb": 2, "hc": 2}, projects, "ha, left empty, keeps its row at exactly 0")
	assert.Equal(t, int64(5), total)
}

// TestNoticeCountsRollBackWithTheAdmission (K2): the totals move in the
// admission's own transaction, so an admission whose payload cannot be
// written, as A2's, leaves both totals as they were: a counted project's
// and a new one's alike.
func TestNoticeCountsRollBackWithTheAdmission(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	admit(t, s, admissible(t))
	projects, total := noticeTotals(t, db)
	require.Equal(t, map[string]int64{"ha": 1}, projects)
	require.Equal(t, int64(1), total)
	_, err := db.ExecContext(t.Context(), "CREATE TEMP TRIGGER refuse_payload BEFORE INSERT ON main.notice_payloads BEGIN SELECT RAISE(ABORT, 'refused by the test'); END")
	require.NoError(t, err)

	for name, e := range map[string]store.NoticeEnvelope{
		"a counted project": admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = 51 }),
		"a new project":     admissible(t, func(e *store.NoticeEnvelope) { e.Project = "hz" }),
	} {
		_, err := s.AdmitNotice(t.Context(), e, ledgerAt)
		require.ErrorIs(t, err, store.ErrStore, name)
		got, all := noticeTotals(t, db)
		assert.Equal(t, projects, got, "%s: no project's total moved", name)
		assert.Equal(t, total, all, "%s: the store's total did not move", name)
	}
	assert.Equal(t, [2]int{1, 1}, ledgerRows(t, s))
	assertNoticeTotals(t, db)
}

// TestNoticeCountsSchemaChecks (K5): 0010's tables refuse, whoever writes
// them, the totals that must never be: a second store row, a total below 0,
// none, or one not a whole number. Each refusal names the one constraint
// SQLite must report, and breaks that one alone: every project's row has a
// key of its own, so a row the schema wrongly admitted could not turn the
// next into a duplicate.
func TestNoticeCountsSchemaChecks(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	_, err := db.ExecContext(t.Context(), "INSERT INTO notice_counts (project, identities) VALUES ('p', 0)")
	require.NoError(t, err, "a project's first row")

	check := func(expr string) string { return "CHECK constraint failed: " + expr }
	type refusal struct {
		query string
		by    string
	}
	for name, tc := range map[string]refusal{
		"a store row with id 2":      {"INSERT INTO notice_store_counts (id, identities) VALUES (2, 0)", check("id = 1")},
		"a second store row, id 1":   {"INSERT INTO notice_store_counts (id, identities) VALUES (1, 0)", "UNIQUE constraint failed: notice_store_counts.id"},
		"a store total below 0":      {"UPDATE notice_store_counts SET identities = -1 WHERE id = 1", check("identities >= 0")},
		"a NULL store total":         {"UPDATE notice_store_counts SET identities = NULL WHERE id = 1", "NOT NULL constraint failed: notice_store_counts.identities"},
		"a store total in text":      {"UPDATE notice_store_counts SET identities = 'x' WHERE id = 1", "cannot store TEXT value in INTEGER column notice_store_counts.identities"},
		"a project's total below 0":  {"INSERT INTO notice_counts (project, identities) VALUES ('p-below', -1)", check("identities >= 0")},
		"a NULL project total":       {"INSERT INTO notice_counts (project, identities) VALUES ('p-null', NULL)", "NOT NULL constraint failed: notice_counts.identities"},
		"a project total in text":    {"INSERT INTO notice_counts (project, identities) VALUES ('p-text', 'x')", "cannot store TEXT value in INTEGER column notice_counts.identities"},
		"a second row for a project": {"INSERT INTO notice_counts (project, identities) VALUES ('p', 0)", "UNIQUE constraint failed: notice_counts.project"},
	} {
		_, err := db.ExecContext(t.Context(), tc.query)
		constraintFailed(t, err, name)
		assert.ErrorContains(t, err, tc.by, name)
	}
}

package store_test

import (
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// TestUpgradeFromTheFirstSchema is the live state.db's path: a database at
// schema 1, with workers, handoffs and focus history in it, opened by this
// build is migrated in place to the newest schema. Every row survives, the
// inbox is there and takes a message, and the board a post. The worker has no terminal (A35),
// so it binds nothing until the team poll judges it, and its pending
// handoff, which has no generation, cannot complete: it can only fail.
func TestUpgradeFromTheFirstSchema(t *testing.T) {
	t.Parallel()
	first, err := os.ReadFile("migrations/0001_init.sql")
	require.NoError(t, err)
	path := dbPath(t)
	v1, err := store.OpenWith(t.Context(), path, fstest.MapFS{"0001_init.sql": {Data: first}})
	require.NoError(t, err)
	require.Equal(t, 1, v1.UserVersion())
	_, err = store.DB(v1).ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at) VALUES ('w', 'agentisan', 'coders', 'w14:p2', 2, 'active', 0)")
	require.NoError(t, err)
	_, err = store.DB(v1).ExecContext(t.Context(), `INSERT INTO handoffs (id, logical_id, old_pane, new_pane, state, created_at, updated_at) VALUES
	(1, 'w', 'w14:p1', 'w14:p2', 'acknowledged', 1, 2), (2, 'w', 'w14:p2', 'w14:p3', 'pending', 3, 3)`)
	require.NoError(t, err)
	require.NoError(t, v1.RecordFocus(t.Context(), "w14:p2", t1))
	require.NoError(t, v1.Close())

	s := open(t, path)
	assert.Equal(t, store.SchemaVersion(), s.UserVersion())
	assert.Equal(t, 10, s.UserVersion())
	row, err := s.WorkerRow(t.Context(), "w")
	require.NoError(t, err)
	assert.Nil(t, row.Routing, "a worker bound before 0005 was not launched by routing")
	ws, err := s.Workers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []store.Worker{{LogicalID: "w", Project: "agentisan", Group: "coders", PaneID: "w14:p2", Generation: 2, Status: "active", HandoffPending: true}}, ws)
	var handoffs int
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(), "SELECT count(*) FROM handoffs").Scan(&handoffs))
	assert.Equal(t, 2, handoffs)
	require.ErrorIs(t, s.CompleteHandoff(t.Context(), 2, t1), store.ErrNotBound, "begun with no generation to complete onto")
	require.NoError(t, s.FailHandoff(t.Context(), 2, t1))
	focus, err := s.RecentFocus(t.Context(), store.FocusCap)
	require.NoError(t, err)
	require.Len(t, focus, 1)
	assert.Equal(t, int64(2), send(t, s, worker("w"), boss, "after the upgrade").Generation)
	board, err := s.BoardStatus(t.Context(), "agentisan", store.ReaderBoss)
	require.NoError(t, err)
	assert.Equal(t, store.BoardState{}, board, "0007's board starts empty")
	assert.Equal(t, int64(1), post(t, s, teamPost("agentisan", "w", "after the upgrade")).Seq)
}

// TestUpgradeTakesEachProviderFromItsRouting is the live state.db's path
// into 0008: a row routing launched gets its decision's provider, the one it
// runs, and a row it did not launch has none, since nothing records which
// agent it was launched as (A66).
func TestUpgradeTakesEachProviderFromItsRouting(t *testing.T) {
	t.Parallel()
	seven := fstest.MapFS{}
	for _, name := range []string{
		"0001_init.sql", "0002_messages.sql", "0003_worker_identity.sql", "0004_github_notices.sql",
		"0005_worker_routing.sql", "0006_github_seeded_kinds.sql", "0007_board.sql",
	} {
		raw, err := os.ReadFile(filepath.Join("migrations", name)) // #nosec G304 -- this package's own migrations
		require.NoError(t, err)
		seven[name] = &fstest.MapFile{Data: raw}
	}
	path := dbPath(t)
	v7, err := store.OpenWith(t.Context(), path, seven)
	require.NoError(t, err)
	require.Equal(t, 7, v7.UserVersion())
	_, err = store.DB(v7).ExecContext(t.Context(), `INSERT INTO workers (logical_id, project, grp, pane_id, terminal_id, generation, status, created_at,
	routed_at, routed_role, routed_provider, routed_binding, routed_model_id, routed_effort, assignment_digest, policy_version) VALUES
	('ha-review-1', 'ha', 'review', 'w14:p3', 'term_r', 0, 'active', 0, 1, 'code_review', 'codex', 'codex_strong', 'gpt-6.1-sol', 'high', '', 'v1-0123456789ab'),
	('ha-coder-1', 'ha', 'coder', 'w14:p4', 'term_c', 0, 'active', 0, 1, 'coder', 'claude', 'claude_mid', 'claude-sonnet-5-5', 'high', '', 'v1-0123456789ab');
INSERT INTO workers (logical_id, project, grp, pane_id, terminal_id, generation, status, created_at) VALUES
	('ha-qa-1', 'ha', 'qa', 'w14:p5', 'term_q', 0, 'active', 0)`)
	require.NoError(t, err)
	require.NoError(t, v7.Close())

	s := open(t, path)
	for id, want := range map[string]string{"ha-review-1": "codex", "ha-coder-1": "claude", "ha-qa-1": ""} {
		row, err := s.WorkerRow(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, want, row.Provider, id)
	}
	live, err := s.Workers(t.Context())
	require.NoError(t, err)
	providers := map[string]string{}
	for _, w := range live {
		providers[w.LogicalID] = w.Provider
	}
	assert.Equal(t, map[string]string{"ha-coder-1": "claude", "ha-qa-1": "", "ha-review-1": "codex"}, providers)
}

// TestUpgradeKeepsTheGitHubSeed is the live state.db's path into 0006: a
// repo the GitHub notices seeded at schema 5 stays seeded, with its open
// PRs and events, and no kind of its own marked, so the reviews' first
// collection still seeds them (A45).
func TestUpgradeKeepsTheGitHubSeed(t *testing.T) {
	t.Parallel()
	five := fstest.MapFS{}
	for _, name := range []string{"0001_init.sql", "0002_messages.sql", "0003_worker_identity.sql", "0004_github_notices.sql", "0005_worker_routing.sql"} {
		raw, err := os.ReadFile(filepath.Join("migrations", name)) // #nosec G304 -- this package's own migrations
		require.NoError(t, err)
		five[name] = &fstest.MapFile{Data: raw}
	}
	path := dbPath(t)
	v5, err := store.OpenWith(t.Context(), path, five)
	require.NoError(t, err)
	require.Equal(t, 5, v5.UserVersion())
	_, err = store.DB(v5).ExecContext(t.Context(), `INSERT INTO github_seeded VALUES ('agentisan', 'nerds-run/agentisan-skills', 1, 1);
INSERT INTO github_open VALUES ('agentisan', 'nerds-run/agentisan-skills', 1006, 1, 1);
INSERT INTO github_seen VALUES ('agentisan', 'nerds-run/agentisan-skills', 1006, 'ci_passed', 'afa239d', 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, v5.Close())

	s := open(t, path)
	got, err := s.GitHub(t.Context(), "agentisan", "nerds-run/agentisan-skills")
	require.NoError(t, err)
	assert.Equal(t, store.GitHubState{
		Seeded: true, Kinds: map[string]bool{}, Open: []int{1006},
		Seen: map[store.GitHubEvent]bool{{PR: 1006, Kind: "ci_passed", Key: "afa239d"}: true},
	}, got)
}

// TestUpgradeFrom8 (A10) is the live state.db's path into 0009: a database
// at schema 8 with rows in every table migrates to 9 with every row and
// every object it had unchanged, and an empty ledger, which then imports
// what github_seen already held and admits the rest.
func TestUpgradeFrom8(t *testing.T) {
	t.Parallel()
	eight := fstest.MapFS{}
	for _, name := range []string{
		"0001_init.sql", "0002_messages.sql", "0003_worker_identity.sql", "0004_github_notices.sql",
		"0005_worker_routing.sql", "0006_github_seeded_kinds.sql", "0007_board.sql", "0008_worker_provider.sql",
	} {
		raw, err := os.ReadFile(filepath.Join("migrations", name)) // #nosec G304 -- this package's own migrations
		require.NoError(t, err)
		eight[name] = &fstest.MapFile{Data: raw}
	}
	path := dbPath(t)
	v8, err := store.OpenWith(t.Context(), path, eight)
	require.NoError(t, err)
	require.Equal(t, 8, v8.UserVersion())
	ctx := t.Context()
	require.NoError(t, v8.RecordFocus(ctx, "w14:p2", t1))
	seedWorker(t, v8, "w", "w14:p2", 1)
	_, err = store.DB(v8).ExecContext(ctx, "INSERT INTO handoffs (logical_id, old_pane, new_pane, state, created_at, updated_at) VALUES ('w', 'w14:p1', 'w14:p2', 'acknowledged', 1, 2)")
	require.NoError(t, err)
	send(t, v8, worker("w"), boss, "before 0009")
	post(t, v8, teamPost("ha", "w", "before 0009"))
	_, err = v8.MarkSeen(ctx, "ha", store.ReaderUser, 1)
	require.NoError(t, err)
	require.NoError(t, v8.UpdateGitHub(ctx, store.GitHubUpdate{
		Project: "ha", Repo: ledgerRepo, Collected: true, SeedKinds: []string{"review"}, Open: []int{50},
		Seen: []store.GitHubEvent{{PR: 50, Kind: "ci_failed", Key: headOID}},
	}, ledgerSeen))
	before := otherTables(t, store.DB(v8))
	for table, rows := range before {
		require.NotEmpty(t, rows, "%s has a row before the upgrade", table)
	}
	schema := func(db *sql.DB) [][]any {
		return queryRows(t, db, "SELECT type, name, tbl_name, sql FROM sqlite_schema WHERE tbl_name NOT IN ('notice_identities', 'notice_payloads', 'notice_counts', 'notice_store_counts') ORDER BY type, name")
	}
	objectsBefore := schema(store.DB(v8))
	require.NoError(t, v8.Close())

	s := open(t, path)
	assert.Equal(t, 10, s.UserVersion())
	assert.Equal(t, before, otherTables(t, store.DB(s)), "every row of schema 8 survives as it was")
	assert.Equal(t, objectsBefore, schema(store.DB(s)), "every object of schema 8 is as it was")
	assert.Equal(t, [2]int{0, 0}, ledgerRows(t, s), "the ledger starts empty")
	var check string
	require.NoError(t, store.DB(s).QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check))
	assert.Equal(t, "ok", check)
	assert.Empty(t, queryRows(t, store.DB(s), "PRAGMA foreign_key_check"))

	told := admissible(t)
	assert.Equal(t, store.NoticeAdmission{CaptureID: told.CaptureID, State: "imported", Imported: true}, admit(t, s, told),
		"an event told before the upgrade is imported, never told again")
	fresh := admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = 51 })
	assert.Equal(t, "pending", admit(t, s, fresh).State)
	got, err := s.PendingNotice(ctx, fresh.CaptureID)
	require.NoError(t, err)
	assert.Equal(t, fresh.Text, got.Text)
}

// TestUpgradeFrom7 is the live state.db's own path: it is at schema 7, so
// one Open applies 0008 and then 0009. An event github_seen held at 7 is
// then imported, never told again, and an unseen one admits as pending.
func TestUpgradeFrom7(t *testing.T) {
	t.Parallel()
	seven := fstest.MapFS{}
	for _, name := range []string{
		"0001_init.sql", "0002_messages.sql", "0003_worker_identity.sql", "0004_github_notices.sql",
		"0005_worker_routing.sql", "0006_github_seeded_kinds.sql", "0007_board.sql",
	} {
		raw, err := os.ReadFile(filepath.Join("migrations", name)) // #nosec G304 -- this package's own migrations
		require.NoError(t, err)
		seven[name] = &fstest.MapFile{Data: raw}
	}
	path := dbPath(t)
	v7, err := store.OpenWith(t.Context(), path, seven)
	require.NoError(t, err)
	require.Equal(t, 7, v7.UserVersion())
	require.NoError(t, v7.UpdateGitHub(t.Context(), store.GitHubUpdate{
		Project: "ha", Repo: ledgerRepo, Collected: true, Open: []int{50},
		Seen: []store.GitHubEvent{{PR: 50, Kind: "ci_failed", Key: headOID}, {PR: 50, Kind: "review", Key: " 2026-11-16T08:59:00Z"}},
	}, ledgerSeen))
	require.NoError(t, v7.Close())

	s := open(t, path)
	assert.Equal(t, 10, s.UserVersion(), "one Open applies 0008, 0009 and 0010")
	told := admissible(t)
	assert.Equal(t, store.NoticeAdmission{CaptureID: told.CaptureID, State: "imported", Imported: true}, admit(t, s, told))
	fresh := admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = 51 })
	assert.Equal(t, store.NoticeAdmission{CaptureID: fresh.CaptureID, State: "pending"}, admit(t, s, fresh))
	got, err := s.PendingNotice(t.Context(), fresh.CaptureID)
	require.NoError(t, err)
	assert.Equal(t, fresh.Text, got.Text)
	assert.Equal(t, [2]int{2, 1}, ledgerRows(t, s))
}

// TestUpgradeFrom9 (K3) is the path into 0010: a database at schema 9 whose
// ledger holds GitHub rows in each state, the pending one with its payload,
// and a daemon row in the empty project, migrates to 10 with its totals
// backfilled from those rows and every row and object of 9 unchanged. The
// rows are raw SQL, not AdmitNotice, so the test holds once the quotas make
// AdmitNotice read the totals, which do not exist at 9.
func TestUpgradeFrom9(t *testing.T) {
	t.Parallel()
	nine := fstest.MapFS{}
	for _, name := range []string{
		"0001_init.sql", "0002_messages.sql", "0003_worker_identity.sql", "0004_github_notices.sql",
		"0005_worker_routing.sql", "0006_github_seeded_kinds.sql", "0007_board.sql", "0008_worker_provider.sql",
		"0009_notice_ledger.sql",
	} {
		raw, err := os.ReadFile(filepath.Join("migrations", name)) // #nosec G304 -- this package's own migrations
		require.NoError(t, err)
		nine[name] = &fstest.MapFile{Data: raw}
	}
	path := dbPath(t)
	v9, err := store.OpenWith(t.Context(), path, nine)
	require.NoError(t, err)
	require.Equal(t, 9, v9.UserVersion())
	ctx := t.Context()
	db9 := store.DB(v9)
	github := func(id, state string, pr int, over map[string]any) map[string]any {
		row := map[string]any{
			"capture_id": id, "project": "ha", "source": "github", "kind": "ci_failed",
			"repo": ledgerRepo, "pr": pr, "receipt_key": headOID, "state": state, "admitted_at": 1, "attempts": 0,
		}
		maps.Copy(row, over)
		return row
	}
	for name, row := range map[string]map[string]any{
		"pending":  github("gh1:pending", "pending", 71, nil),
		"imported": github("gh1:imported", "imported", 72, nil),
		"posted":   github("gh1:posted", "posted", 73, map[string]any{"settled_at": 2, "post_seq": 7}),
		"dropped":  github("gh1:dropped", "dropped", 74, map[string]any{"settled_at": 2, "drop_reason": "muted"}),
		"daemon":   daemonIdentity,
	} {
		require.NoError(t, insertRow(t, db9, insertIdentity, identityColumns, row), name)
	}
	require.NoError(t, insertRow(t, db9, insertPayload, payloadColumns, map[string]any{
		"capture_id": "gh1:pending", "version": 1, "receipt_version": 1, "subject": ledgerRepo + "#71", "worker": "ha-coder-1",
		"observed_at": 1, "text": "#71 CI failed", "bytes": 13, "digest": strings.Repeat("0123456789abcdef", 4),
	}))
	ledger := func(db *sql.DB) [][][]any {
		return [][][]any{queryRows(t, db, "SELECT * FROM notice_identities ORDER BY rowid"), tableRows(t, db, "notice_payloads")}
	}
	// 0010's objects: its two tables, the totals' key's index, and its three
	// triggers, which are on notice_identities.
	schema := func(db *sql.DB) [][]any {
		return queryRows(t, db, `SELECT type, name, tbl_name, sql FROM sqlite_schema
WHERE tbl_name NOT IN ('notice_counts', 'notice_store_counts')
AND name NOT IN ('notice_counts_insert', 'notice_counts_delete', 'notice_counts_move') ORDER BY type, name`)
	}
	ledgerBefore, othersBefore, objectsBefore := ledger(db9), otherTables(t, db9), schema(db9)
	require.Len(t, ledgerBefore[0], 5)
	require.Len(t, ledgerBefore[1], 1)
	require.NoError(t, v9.Close())

	s := open(t, path)
	db := store.DB(s)
	assert.Equal(t, 10, s.UserVersion())
	assertNoticeTotals(t, db)
	projects, total := noticeTotals(t, db)
	assert.Equal(t, map[string]int64{"": 1, "ha": 4}, projects, "every state counts")
	assert.Equal(t, int64(5), total)
	assert.Equal(t, ledgerBefore, ledger(db), "every ledger row of schema 9 survives as it was")
	assert.Equal(t, othersBefore, otherTables(t, db), "every other row of schema 9 survives as it was")
	assert.Equal(t, objectsBefore, schema(db), "every object of schema 9 is as it was")
	var check string
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check))
	assert.Equal(t, "ok", check)
	assert.Empty(t, queryRows(t, db, "PRAGMA foreign_key_check"))

	require.Equal(t, "pending", admit(t, s, admissible(t)).State)
	projects, total = noticeTotals(t, db)
	assert.Equal(t, map[string]int64{"": 1, "ha": 5}, projects, "the next admission moves its project's total by one")
	assert.Equal(t, int64(6), total, "and the store's")
	assertNoticeTotals(t, db)
}

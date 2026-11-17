package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
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
	assert.Equal(t, 9, s.UserVersion())
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
		return queryRows(t, db, "SELECT type, name, tbl_name, sql FROM sqlite_schema WHERE tbl_name NOT IN ('notice_identities', 'notice_payloads') ORDER BY type, name")
	}
	objectsBefore := schema(store.DB(v8))
	require.NoError(t, v8.Close())

	s := open(t, path)
	assert.Equal(t, 9, s.UserVersion())
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
	assert.Equal(t, 9, s.UserVersion(), "one Open applies 0008, then 0009")
	told := admissible(t)
	assert.Equal(t, store.NoticeAdmission{CaptureID: told.CaptureID, State: "imported", Imported: true}, admit(t, s, told))
	fresh := admissible(t, func(e *store.NoticeEnvelope) { e.Receipt.PR = 51 })
	assert.Equal(t, store.NoticeAdmission{CaptureID: fresh.CaptureID, State: "pending"}, admit(t, s, fresh))
	got, err := s.PendingNotice(t.Context(), fresh.CaptureID)
	require.NoError(t, err)
	assert.Equal(t, fresh.Text, got.Text)
	assert.Equal(t, [2]int{2, 1}, ledgerRows(t, s))
}

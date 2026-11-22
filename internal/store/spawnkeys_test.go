package store_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// retryKey is the retry key tests' key, 32 lowercase hex characters, and
// spawnAt when its request begins.
var (
	retryKey = strings.Repeat("3f9c0d5e", 4)
	spawnAt  = time.Date(2026, 11, 17, 20, 0, 41, 123456789, time.UTC)
)

// spawnKey is K's request, with every field set, changed by each change.
func spawnKey(change ...func(*store.SpawnKey)) store.SpawnKey {
	k := store.SpawnKey{
		Key: retryKey, Project: "agentisan", Group: "coders", Count: 2,
		Model: "claude-opus-5-5", Effort: "high", Provider: "claude", TaskDigest: strings.Repeat("0123456789abcdef", 4),
	}
	for _, c := range change {
		c(&k)
	}
	return k
}

// link binds key's planned id to its workers row at gen, by raw SQL, as
// the bind's own transaction will.
func link(t *testing.T, s *store.Store, key, id string, gen int64) {
	t.Helper()
	res, err := store.DB(s).ExecContext(t.Context(),
		"UPDATE spawn_request_workers SET bound_id = logical_id, generation = ? WHERE retry_key = ? AND logical_id = ?", gen, key, id)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "%s is planned under the key", id)
}

// spawnRows counts the rows of both retry key tables.
func spawnRows(t *testing.T, s *store.Store) [2]int {
	t.Helper()
	var n [2]int
	require.NoError(t, store.DB(s).QueryRowContext(t.Context(),
		"SELECT (SELECT count(*) FROM spawn_requests), (SELECT count(*) FROM spawn_request_workers)").Scan(&n[0], &n[1]))
	return n
}

// TestSpawnKeyMigrationUpgradesThePreviousStore (A1): a store at the schema
// just before the retry keys' migration, with workers and messages rows,
// migrates with those rows unchanged and both new tables empty. The new API
// then works on the old rows, and the answer's json_valid CHECK, the first
// JSON function in any migration, runs under modernc's SQLite.
func TestSpawnKeyMigrationUpgradesThePreviousStore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	entries, err := os.ReadDir("migrations")
	require.NoError(t, err)
	previous, found := fstest.MapFS{}, false
	for _, e := range entries { // sorted by name
		if strings.HasSuffix(e.Name(), "_spawn_retry_keys.sql") {
			found = true
			break
		}
		raw, err := os.ReadFile(filepath.Join("migrations", e.Name())) // #nosec G304 -- this package's own migrations
		require.NoError(t, err)
		previous[e.Name()] = &fstest.MapFile{Data: raw}
	}
	require.True(t, found, "the retry keys' migration is embedded")
	path := dbPath(t)
	old, err := store.OpenWith(ctx, path, previous)
	require.NoError(t, err)
	require.Equal(t, len(previous), old.UserVersion())
	seedWorker(t, old, "ha-coder-3", "w14:p3", 1)
	sendBefore12(t, old, "ha-coder-3", "your task")
	workers, messages := tableRows(t, store.DB(old), "workers"), tableRows(t, store.DB(old), "messages")
	require.NoError(t, old.Close())

	s := open(t, path)
	db := store.DB(s)
	assert.Equal(t, store.SchemaVersion(), s.UserVersion())
	assert.Equal(t, workers, tableRows(t, db, "workers"), "the workers rows survive as they were")
	assert.Equal(t, messages, tableRows(t, db, "messages"), "the messages survive as they were")
	assert.Equal(t, [2]int{0, 0}, spawnRows(t, s), "both new tables start empty")

	require.NoError(t, s.BeginSpawn(ctx, spawnKey(), []string{"ha-coder-3", "ha-coder-4"}, spawnAt))
	link(t, s, retryKey, "ha-coder-3", 1)
	rec, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	row, err := s.WorkerRow(ctx, "ha-coder-3")
	require.NoError(t, err)
	assert.Equal(t, &row, rec.Planned[0].Row, "a row bound before the upgrade")
	assert.Equal(t, 1, rec.Planned[0].Briefs, "a brief queued before the upgrade")
	dropped, err := s.DropSpawn(ctx, retryKey)
	require.NoError(t, err)
	assert.False(t, dropped, "a record with a bound id is kept")

	_, err = db.ExecContext(ctx, `UPDATE spawn_requests SET state = 'done', outcome = 'ok', answer = '{"workers":[]}', finished_at = created_at WHERE retry_key = ?`, retryKey)
	require.NoError(t, err, "a JSON answer is stored")
	_, err = db.ExecContext(ctx, "UPDATE spawn_requests SET answer = 'not json' WHERE retry_key = ?", retryKey)
	constraintFailed(t, err, "an answer that is not JSON")
	assert.ErrorContains(t, err, "json_valid(answer)")
	rec, err = s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	assert.Equal(t, store.SpawnAnswer{Code: "ok", Result: []byte(`{"workers":[]}`)}, rec.Answer)
}

// spawnRequestColumns are spawn_requests' columns, for raw inserts.
var (
	spawnRequestColumns = []string{"retry_key", "project", "grp", "count", "model", "effort", "provider", "task_digest", "state", "outcome", "message", "answer", "created_at", "finished_at"}
	insertSpawnRequest  = `INSERT INTO spawn_requests (retry_key, project, grp, count, model, effort, provider, task_digest, state, outcome, message, answer, created_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	spawnLinkColumns = []string{"retry_key", "seq", "logical_id", "bound_id", "generation"}
	insertSpawnLink  = "INSERT INTO spawn_request_workers (retry_key, seq, logical_id, bound_id, generation) VALUES (?, ?, ?, ?, ?)"
)

// checkFailed is the error SQLite gives for a CHECK, by the start of its
// text.
func checkFailed(expr string) string { return "CHECK constraint failed: " + expr }

// TestSpawnRequestsChecks (A2): spawn_requests itself refuses, on raw
// inserts, each row a record must never be, the NUL tails included, and
// stores every code a spawn can answer. Each refusal breaks one constraint,
// named by the start of its text.
func TestSpawnRequestsChecks(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	n := 0
	request := func(over map[string]any) error {
		n++
		row := map[string]any{
			"retry_key": fmt.Sprintf("%032x", n), "project": "ha", "grp": "coder", "count": 1,
			"model": "", "effort": "", "provider": "", "task_digest": "", "state": "running", "created_at": 10,
		}
		maps.Copy(row, over)
		return insertRow(t, store.DB(s), insertSpawnRequest, spawnRequestColumns, row)
	}
	done := func(outcome string, over map[string]any) map[string]any {
		row := map[string]any{"state": "done", "finished_at": 10, "outcome": outcome, "message": "m"}
		if outcome == "ok" {
			row["message"], row["answer"] = nil, "{}"
		}
		maps.Copy(row, over)
		return row
	}
	hex64 := strings.Repeat("0123456789abcdef", 4)
	stored := map[string]map[string]any{
		"ok":                       done("ok", map[string]any{"answer": `{"workers":[{"id":"ha-coder-3"}]}`}),
		"effort high, a digest":    {"effort": "high", "task_digest": hex64, "provider": "codex", "model": strings.Repeat("m", 128)},
		"the largest count":        {"count": 64},
		"finished as it began":     done("spawn_failed", map[string]any{"finished_at": 10}),
		"a message of 16 KiB":      done("spawn_failed", map[string]any{"message": strings.Repeat("é", 8192)}),
		"an answer of 64 KiB":      done("ok", map[string]any{"answer": `"` + strings.Repeat("a", 65534) + `"`}),
		"an outcome of 32 letters": done(strings.Repeat("a", 32), nil),
	}
	for _, code := range []string{
		"spawn_failed", "herdr_failed", "store_failed", "failed", "pane_changed", "herdr_gone",
		"too_large", "bad_request", "timeout", "handoff_pending", "retry_key_mismatch", "spawn_unsettled",
	} {
		stored[code] = done(code, nil)
	}
	for name, over := range stored {
		require.NoError(t, request(over), name)
	}

	var (
		key     = checkFailed("length(CAST(retry_key AS BLOB)) = 32")
		effort  = checkFailed("length(CAST(effort AS BLOB)) = length(effort)")
		digest  = checkFailed("task_digest = ''")
		outcome = checkFailed("outcome IS NULL")
		message = checkFailed("message IS NULL")
		answer  = checkFailed("answer IS NULL")
		ended   = checkFailed("(state = 'done') = (finished_at IS NOT NULL)")
		decided = checkFailed("(state = 'done') = (outcome IS NOT NULL)")
		okBody  = checkFailed("(answer IS NOT NULL) = (outcome IS 'ok')")
		refused = checkFailed("(message IS NOT NULL) = (outcome IS NOT NULL AND outcome IS NOT 'ok')")
	)
	type refusal struct {
		over map[string]any
		by   string
	}
	for name, tc := range map[string]refusal{
		"a key holding a NUL":                {map[string]any{"retry_key": "0123456789abcde\x000123456789abcdef"}, key},
		"a key in upper case":                {map[string]any{"retry_key": strings.ToUpper(retryKey)}, key},
		"a key of 31 characters":             {map[string]any{"retry_key": retryKey[:31]}, key},
		"the first row's key":                {map[string]any{"retry_key": fmt.Sprintf("%032x", 1)}, "UNIQUE constraint failed: spawn_requests.retry_key"},
		"count 0":                            {map[string]any{"count": 0}, checkFailed("count BETWEEN 1 AND 64")},
		"count 65":                           {map[string]any{"count": 65}, checkFailed("count BETWEEN 1 AND 64")},
		"provider x":                         {map[string]any{"provider": "x"}, checkFailed("provider IN ('', 'claude', 'codex')")},
		"an empty project":                   {map[string]any{"project": ""}, checkFailed("project <> ''")},
		"an empty group":                     {map[string]any{"grp": ""}, checkFailed("grp <> ''")},
		"a model of 129 bytes":               {map[string]any{"model": strings.Repeat("m", 129)}, checkFailed("length(CAST(model AS BLOB)) <= 128")},
		"an effort with a NUL tail":          {map[string]any{"effort": "high\x00X"}, effort},
		"an effort in upper case":            {map[string]any{"effort": "High"}, effort},
		"an effort of 17 letters":            {map[string]any{"effort": strings.Repeat("e", 17)}, effort},
		"a digest with a NUL tail":           {map[string]any{"task_digest": hex64 + "\x00X"}, digest},
		"a NUL inside a 64-character digest": {map[string]any{"task_digest": hex64[:10] + "\x00" + hex64[11:]}, digest},
		"a digest in upper case":             {map[string]any{"task_digest": strings.ToUpper(hex64)}, digest},
		"a key with a NUL tail":              {map[string]any{"retry_key": retryKey + "\x00X"}, key},
		"a key with a g":                     {map[string]any{"retry_key": retryKey[:31] + "g"}, key},
		"an effort with a digit":             {map[string]any{"effort": "high2"}, effort},
		"a digest with a g":                  {map[string]any{"task_digest": hex64[:63] + "g"}, digest},
		"an empty outcome":                   {done("", nil), outcome},
		"an outcome with a digit":            {done("spawn_failed2", nil), outcome},
		"a running row with an outcome":      {map[string]any{"outcome": "spawn_failed", "message": "m"}, decided},
		"an unknown state":                   {map[string]any{"state": "settled"}, checkFailed("state IN ('running', 'done')")},
		"an outcome with a NUL tail":         {done("spawn_failed\x00X", nil), outcome},
		"an outcome in upper case":           {done("SPAWN_FAILED", nil), outcome},
		"an outcome of 33 letters":           {done(strings.Repeat("a", 33), nil), outcome},
		"done without finished_at":           {done("spawn_failed", map[string]any{"finished_at": nil}), ended},
		"running with finished_at":           {map[string]any{"finished_at": 10}, ended},
		"finished before it began":           {done("spawn_failed", map[string]any{"finished_at": 9}), checkFailed("finished_at IS NULL OR finished_at >= created_at")},
		"done without an outcome":            {map[string]any{"state": "done", "finished_at": 10}, decided},
		"ok with a message":                  {done("ok", map[string]any{"message": "m"}), refused},
		"ok without an answer":               {done("ok", map[string]any{"answer": nil}), okBody},
		"a refusal with an answer":           {done("spawn_failed", map[string]any{"answer": "{}"}), okBody},
		"a refusal without a message":        {done("spawn_failed", map[string]any{"message": nil}), refused},
		"a refusal with an empty message":    {done("spawn_failed", map[string]any{"message": ""}), message},
		"a message over 16 KiB":              {done("spawn_failed", map[string]any{"message": strings.Repeat("m", 16385)}), message},
		"a running row with a message":       {map[string]any{"message": "m"}, refused},
		"an answer that is not JSON":         {done("ok", map[string]any{"answer": "not json"}), answer},
		"an answer over 64 KiB":              {done("ok", map[string]any{"answer": `"` + strings.Repeat("a", 65535) + `"`}), answer},
	} {
		err := request(tc.over)
		constraintFailed(t, err, name)
		assert.ErrorContains(t, err, tc.by, name)
	}
}

// TestSpawnRequestWorkersChecks (A3): spawn_request_workers refuses a
// repeated id or seq, a seq outside 1 to 64, and a link that names no
// workers row, another id than its own, or no generation.
func TestSpawnRequestWorkersChecks(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	db := store.DB(s)
	require.NoError(t, s.BeginSpawn(t.Context(), spawnKey(func(k *store.SpawnKey) { k.Count = 1 }), []string{"ha-coder-1"}, spawnAt))
	seedWorker(t, s, "ha-coder-2", "w14:p2", 0)
	seedWorker(t, s, "ha-coder-3", "w14:p3", 0)
	planned := func(over map[string]any) error {
		row := map[string]any{"retry_key": retryKey, "seq": 9, "logical_id": "ha-coder-9"}
		maps.Copy(row, over)
		return insertRow(t, db, insertSpawnLink, spawnLinkColumns, row)
	}
	require.NoError(t, planned(map[string]any{"seq": 2, "logical_id": "ha-coder-2", "bound_id": "ha-coder-2", "generation": 0}), "a bound link")

	generation := checkFailed("(bound_id IS NULL) = (generation IS NULL)")
	for name, tc := range map[string]struct {
		over map[string]any
		by   string
	}{
		"a duplicate id":                  {map[string]any{"logical_id": "ha-coder-1"}, "UNIQUE constraint failed: spawn_request_workers.retry_key, spawn_request_workers.logical_id"},
		"a repeated seq":                  {map[string]any{"seq": 1}, "UNIQUE constraint failed: spawn_request_workers.retry_key, spawn_request_workers.seq"},
		"seq 0":                           {map[string]any{"seq": 0}, checkFailed("seq BETWEEN 1 AND 64")},
		"seq 65":                          {map[string]any{"seq": 65}, checkFailed("seq BETWEEN 1 AND 64")},
		"an empty id":                     {map[string]any{"logical_id": ""}, checkFailed("logical_id <> ''")},
		"a key with no record":            {map[string]any{"retry_key": strings.Repeat("0", 32)}, "FOREIGN KEY constraint failed"},
		"a bound_id with no workers row":  {map[string]any{"bound_id": "ha-coder-9", "generation": 0}, "FOREIGN KEY constraint failed"},
		"a bound_id unlike logical_id":    {map[string]any{"bound_id": "ha-coder-3", "generation": 0}, checkFailed("bound_id IS NULL OR bound_id = logical_id")},
		"a bound_id without a generation": {map[string]any{"logical_id": "ha-coder-3", "bound_id": "ha-coder-3"}, generation},
		"a generation without a bound_id": {map[string]any{"generation": 0}, generation},
		"a negative generation":           {map[string]any{"logical_id": "ha-coder-3", "bound_id": "ha-coder-3", "generation": -1}, checkFailed("generation IS NULL OR generation >= 0")},
	} {
		err := planned(tc.over)
		constraintFailed(t, err, name)
		assert.ErrorContains(t, err, tc.by, name)
	}
}

// TestBeginSpawnRecordsTheRequest (A4): SpawnRecord reads back every field
// BeginSpawn wrote, CreatedAt to the nanosecond, the planned ids in planned
// order, running and none bound, at the schema's upper bounds too; and a
// done record's answer, written here by raw SQL. A begin whose context is
// cancelled writes nothing.
func TestBeginSpawnRecordsTheRequest(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	k := spawnKey()
	require.NoError(t, s.BeginSpawn(ctx, k, []string{"ha-coder-4", "ha-coder-3"}, spawnAt))
	rec, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	assert.Equal(t, store.SpawnRecord{
		SpawnKey: k, State: "running", CreatedAt: spawnAt,
		Planned: []store.SpawnLink{{LogicalID: "ha-coder-4"}, {LogicalID: "ha-coder-3"}},
	}, rec)

	bare := spawnKey(func(k *store.SpawnKey) {
		k.Key, k.Count, k.Model, k.Effort, k.Provider, k.TaskDigest = strings.Repeat("a", 32), 1, "", "", "", ""
	})
	require.NoError(t, s.BeginSpawn(ctx, bare, []string{"ha-coder-5"}, spawnAt))
	_, err = store.DB(s).ExecContext(ctx, "UPDATE spawn_requests SET state = 'done', outcome = 'spawn_failed', message = 'no agent', finished_at = created_at + 5 WHERE retry_key = ?", bare.Key)
	require.NoError(t, err)
	rec, err = s.SpawnRecord(ctx, bare.Key)
	require.NoError(t, err)
	assert.Equal(t, store.SpawnRecord{
		SpawnKey: bare, State: "done", Answer: store.SpawnAnswer{Code: "spawn_failed", Message: "no agent"},
		CreatedAt: spawnAt, FinishedAt: spawnAt.Add(5), Planned: []store.SpawnLink{{LogicalID: "ha-coder-5"}},
	}, rec, "a request with no model, effort, provider or packet")

	// The largest request the schema holds, its unchecked text kept byte
	// for byte as asked: a NUL, a byte that is not UTF-8 and non-ASCII
	// letters in the project, group and model, and 64 ids, one of 256
	// bytes, the rest non-ASCII.
	largest := spawnKey(func(k *store.SpawnKey) {
		k.Key, k.Count, k.Provider = strings.Repeat("d", 32), 64, store.ProviderCodex
		k.Project, k.Group = "projé\x00\xff", "codérs\t\xfe"
		k.Model, k.Effort = strings.Repeat("m", 124)+"\x00\xffé", strings.Repeat("e", 16)
	})
	ids := make([]store.SpawnLink, 64)
	planned := make([]string, 64)
	for i := range planned {
		planned[i] = fmt.Sprintf("ha-codér-%d", 64-i)
		ids[i] = store.SpawnLink{LogicalID: planned[i]}
	}
	planned[0], ids[0].LogicalID = strings.Repeat("i", 256), strings.Repeat("i", 256)
	require.NoError(t, s.BeginSpawn(ctx, largest, planned, spawnAt))
	rec, err = s.SpawnRecord(ctx, largest.Key)
	require.NoError(t, err)
	assert.Equal(t, store.SpawnRecord{SpawnKey: largest, State: "running", CreatedAt: spawnAt, Planned: ids}, rec)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err = s.BeginSpawn(cancelled, spawnKey(func(k *store.SpawnKey) { k.Key = strings.Repeat("e", 32) }), []string{"a", "b"}, spawnAt)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, store.ErrStore)
	_, err = s.SpawnRecord(ctx, strings.Repeat("e", 32))
	require.ErrorIs(t, err, store.ErrNoSpawnKey, "a cancelled begin writes nothing")
	_, err = s.SpawnRecord(ctx, strings.Repeat("b", 32))
	require.ErrorIs(t, err, store.ErrNoSpawnKey)
	_, err = s.SpawnRecord(ctx, strings.ToUpper(retryKey))
	require.ErrorIs(t, err, store.ErrInvalid)
}

// TestBeginSpawnRefusesAKnownKey (A5): a second BeginSpawn of K, with
// another request, plan and time, is ErrSpawnKeyExists, and the first
// record and its planned rows are as they were. Of two begins of one key
// at once, one records it and the other is refused.
func TestBeginSpawnRefusesAKnownKey(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	require.NoError(t, s.BeginSpawn(ctx, spawnKey(), []string{"ha-coder-3", "ha-coder-4"}, spawnAt))
	first, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)

	other := spawnKey(func(k *store.SpawnKey) { k.Group, k.Count, k.Model = "qa", 1, "" })
	err = s.BeginSpawn(ctx, other, []string{"ha-qa-1"}, spawnAt.Add(time.Hour))
	require.ErrorIs(t, err, store.ErrSpawnKeyExists)
	got, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	assert.Equal(t, first, got)
	assert.Equal(t, [2]int{1, 2}, spawnRows(t, s), "no planned row was added or lost")

	// Two begins of one new key at once: exactly one records it.
	key := strings.Repeat("f", 32)
	errs := make(chan error, 2)
	for _, id := range []string{"ha-coder-5", "ha-coder-6"} {
		go func() {
			errs <- s.BeginSpawn(ctx, spawnKey(func(k *store.SpawnKey) { k.Key, k.Count = key, 1 }), []string{id}, spawnAt)
		}()
	}
	results := []error{<-errs, <-errs}
	var won, refused int
	for _, err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, store.ErrSpawnKeyExists):
			refused++
		}
	}
	assert.Equal(t, [2]int{1, 1}, [2]int{won, refused}, "one wins and one is refused: %v", results)
	assert.Equal(t, [2]int{2, 3}, spawnRows(t, s), "one record and one planned row more")
}

// TestBeginSpawnIsOneTransaction: a planned id the store refuses after the
// request row is written fails the whole request, as ErrStore and not as a
// known key, and leaves no row in either table.
func TestBeginSpawnIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	_, err := store.DB(s).ExecContext(t.Context(), `CREATE TRIGGER refuse_second BEFORE INSERT ON spawn_request_workers
WHEN NEW.seq = 2 BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	require.NoError(t, err)
	err = s.BeginSpawn(t.Context(), spawnKey(), []string{"ha-coder-1", "ha-coder-2"}, spawnAt)
	require.ErrorIs(t, err, store.ErrStore)
	require.NotErrorIs(t, err, store.ErrSpawnKeyExists)
	require.ErrorContains(t, err, "injected")
	assert.Equal(t, [2]int{}, spawnRows(t, s), "nothing of the request is kept")
}

// TestBeginSpawnRefusesABadPlan (A6): a request or plan the schema would
// refuse is ErrInvalid before anything is written, and the error names the
// field, never its value.
func TestBeginSpawnRefusesABadPlan(t *testing.T) {
	t.Parallel()
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("ha-coder-%d", i+1)
		}
		return out
	}
	for name, tc := range map[string]struct {
		change   func(*store.SpawnKey)
		planned  []string // nil: the count's ids
		zeroTime bool
		value    string // a value the error must not carry
	}{
		"a plan longer than the count":  {planned: ids(3)},
		"a plan shorter than it":        {planned: ids(1)},
		"no plan":                       {planned: []string{}},
		"a duplicate id":                {planned: []string{"ha-coder-1", "ha-coder-1"}, value: "ha-coder-1"},
		"an empty id":                   {planned: []string{"ha-coder-1", ""}},
		"an id with a control":          {planned: []string{"ha-coder-1", "ha-coder\n2"}},
		"a key of 31 characters":        {change: func(k *store.SpawnKey) { k.Key = retryKey[:31] }, value: retryKey[:31]},
		"a key of 33 characters":        {change: func(k *store.SpawnKey) { k.Key = retryKey + "0" }, value: retryKey},
		"a key in upper case":           {change: func(k *store.SpawnKey) { k.Key = strings.ToUpper(retryKey) }, value: strings.ToUpper(retryKey)},
		"a key with a non-hex letter":   {change: func(k *store.SpawnKey) { k.Key = "g" + retryKey[1:] }, value: retryKey[1:]},
		"a key holding a NUL":           {change: func(k *store.SpawnKey) { k.Key = "\x00" + retryKey[1:] }, value: retryKey[1:]},
		"a key with a byte not UTF-8":   {change: func(k *store.SpawnKey) { k.Key = "\xff" + retryKey[1:] }, value: retryKey[1:]},
		"a key with a non-ASCII letter": {change: func(k *store.SpawnKey) { k.Key = "é" + retryKey[2:] }, value: retryKey[2:]},
		"no key":                        {change: func(k *store.SpawnKey) { k.Key = "" }},
		"an id holding a NUL":           {planned: []string{"ha-coder-1", "ha-coder\x002"}},
		"an id that is not UTF-8":       {planned: []string{"ha-coder-1", "ha-coder-\xff"}},
		"an id of 257 bytes":            {planned: []string{"ha-coder-1", strings.Repeat("i", 257)}, value: strings.Repeat("i", 257)},
		"count -1":                      {change: func(k *store.SpawnKey) { k.Count = -1 }, planned: []string{}},
		"a digest holding a NUL":        {change: func(k *store.SpawnKey) { k.TaskDigest = "\x00" + k.TaskDigest[1:] }},
		"count 0":                       {change: func(k *store.SpawnKey) { k.Count = 0 }},
		"count 65":                      {change: func(k *store.SpawnKey) { k.Count = 65 }, planned: ids(65)},
		"no project":                    {change: func(k *store.SpawnKey) { k.Project = "" }},
		"no group":                      {change: func(k *store.SpawnKey) { k.Group = "" }},
		"a model of 129 bytes":          {change: func(k *store.SpawnKey) { k.Model = strings.Repeat("m", 129) }, value: strings.Repeat("m", 129)},
		"an effort in upper case":       {change: func(k *store.SpawnKey) { k.Effort = "High" }, value: "High"},
		"an effort with a NUL tail":     {change: func(k *store.SpawnKey) { k.Effort = "high\x00X" }},
		"an effort of 17 letters":       {change: func(k *store.SpawnKey) { k.Effort = strings.Repeat("e", 17) }, value: strings.Repeat("e", 17)},
		"another provider":              {change: func(k *store.SpawnKey) { k.Provider = "gemini" }, value: "gemini"},
		"a digest of 63 characters":     {change: func(k *store.SpawnKey) { k.TaskDigest = k.TaskDigest[:63] }, value: k63()},
		"a digest in upper case":        {change: func(k *store.SpawnKey) { k.TaskDigest = strings.ToUpper(k.TaskDigest) }, value: strings.ToUpper(k63())},
		"a time before 1678":            {zeroTime: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t, dbPath(t))
			k := spawnKey()
			if tc.change != nil {
				tc.change(&k)
			}
			planned := tc.planned
			if planned == nil {
				planned = ids(k.Count)
			}
			at := spawnAt
			if tc.zeroTime {
				at = time.Time{}
			}
			err := s.BeginSpawn(t.Context(), k, planned, at)
			require.ErrorIs(t, err, store.ErrInvalid)
			if tc.value != "" {
				assert.NotContains(t, err.Error(), tc.value, "the error never carries the value")
			}
			assert.Equal(t, [2]int{0, 0}, spawnRows(t, s), "nothing was written")
		})
	}
}

// k63 is the first 63 characters of spawnKey's task digest.
func k63() string { return spawnKey().TaskDigest[:63] }

// fullWorker is id's worker with every field BindWorker writes set, each
// override, digest, provider and time to a value of its own, so a column
// read into the wrong field shows. It is routed a second before it is
// bound at spawnAt.
func fullWorker(id string) store.Worker {
	pane := "w14:p" + id[len(id)-1:]
	return store.Worker{
		LogicalID: id, Project: "agentisan", Group: "coders", PaneID: pane, TerminalID: terminal(pane), Provider: store.ProviderClaude,
		Routing: &store.Routing{
			Role: "coder", Provider: "codex", Binding: "claude_mid", ModelID: "claude-sonnet-5-5", Effort: "high",
			OverrideBinding: "claude_top", OverrideBy: "user", OverrideEffort: "xhigh",
			AssignmentDigest: strings.Repeat("ad", 32), PolicyVersion: "v1-0123456789ab", RoutedAt: spawnAt.Add(-time.Second),
		},
	}
}

// zeroFields names each field of v that holds its zero value, walking
// embedded structs and non-nil pointers, but the fields named in skip.
func zeroFields(v reflect.Value, skip ...string) []string {
	var zero []string
	for i := range v.NumField() {
		f, fv := v.Type().Field(i), v.Field(i)
		switch {
		case slices.Contains(skip, f.Name):
		case f.Anonymous:
			zero = append(zero, zeroFields(fv, skip...)...)
		case fv.Kind() == reflect.Pointer && !fv.IsNil():
			zero = append(zero, zeroFields(fv.Elem(), skip...)...)
		case fv.IsZero():
			zero = append(zero, f.Name)
		}
	}
	return zero
}

// TestSpawnLinkRowIsWorkerRow: a link's Row is WorkerRow's, field for field,
// for a worker bound twice and retired with every field WorkerRow reads
// set. A field the fixture leaves zero fails, so a workers column added
// later fails here until fullWorker sets it and the link reads it.
func TestSpawnLinkRowIsWorkerRow(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	w := fullWorker("ha-coder-2")
	for range 2 {
		gen, err := s.BindWorker(ctx, w, spawnAt)
		require.NoError(t, err)
		w.Generation = gen
		require.NoError(t, s.SetWorkerStatus(ctx, w, store.StatusRetired, spawnAt.Add(time.Minute)))
	}
	require.NoError(t, s.BeginSpawn(ctx, spawnKey(func(k *store.SpawnKey) { k.Count = 1 }), []string{w.LogicalID}, spawnAt))
	link(t, s, retryKey, w.LogicalID, w.Generation)
	row, err := s.WorkerRow(ctx, w.LogicalID)
	require.NoError(t, err)
	// Workers reads the shell and the pending handoff; WorkerRow never does.
	require.Empty(t, zeroFields(reflect.ValueOf(row), "ShellPID", "ShellStart", "HandoffPending"), "fields the fixture leaves zero")
	assert.NotEqual(t, row.Provider, row.Routing.Provider, "the launch and routing providers differ, so a swap of the two columns shows")
	assert.NotEqual(t, row.CreatedAt, row.Routing.RoutedAt, "the bind and routing times differ, so a swap of the two shows")
	rec, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	require.Len(t, rec.Planned, 1)
	assert.Equal(t, &row, rec.Planned[0].Row)
}

// TestSpawnRecordShowsEachWorkerNow (A7): each planned id's link shows its
// workers row now, as WorkerRow reads it, while the row is still at the
// linked generation: active, retired or lost. A never bound id has none,
// and one bound again since at generation 1 has none (superseded).
func TestSpawnRecordShowsEachWorkerNow(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	bind := func(id string) store.Worker {
		w := fullWorker(id)
		gen, err := s.BindWorker(ctx, w, spawnAt)
		require.NoError(t, err)
		w.Generation = gen
		return w
	}
	require.NoError(t, s.SetWorkerStatus(ctx, bind("ha-coder-2"), store.StatusActive, spawnAt))
	require.NoError(t, s.SetWorkerStatus(ctx, bind("ha-coder-3"), store.StatusRetired, spawnAt.Add(time.Minute)))
	require.NoError(t, s.MarkWorkerLost(ctx, bind("ha-coder-4")))
	require.NoError(t, s.SetWorkerStatus(ctx, bind("ha-coder-5"), store.StatusRetired, spawnAt))
	require.EqualValues(t, 1, bind("ha-coder-5").Generation, "ha-coder-5 is bound again")

	planned := []string{"ha-coder-1", "ha-coder-2", "ha-coder-3", "ha-coder-4", "ha-coder-5"}
	require.NoError(t, s.BeginSpawn(ctx, spawnKey(func(k *store.SpawnKey) { k.Count = 5 }), planned, spawnAt))
	for _, id := range planned[1:] {
		link(t, s, retryKey, id, 0)
	}
	rec, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	require.Equal(t, planned, linkIDs(rec))
	assert.Equal(t, store.SpawnLink{LogicalID: "ha-coder-1"}, rec.Planned[0], "never bound")
	for i, status := range map[int]string{1: store.StatusActive, 2: store.StatusRetired, 3: store.StatusLost} {
		row, err := s.WorkerRow(ctx, planned[i])
		require.NoError(t, err)
		require.Equal(t, status, row.Status)
		assert.Equal(t, store.SpawnLink{LogicalID: planned[i], Bound: true, Row: &row}, rec.Planned[i], status)
	}
	assert.Equal(t, store.SpawnLink{LogicalID: "ha-coder-5", Bound: true}, rec.Planned[4], "superseded: its row is at generation 1")
}

// linkIDs is rec's planned ids, in order.
func linkIDs(rec store.SpawnRecord) []string {
	out := []string{}
	for _, l := range rec.Planned {
		out = append(out, l.LogicalID)
	}
	return out
}

// TestSpawnRecordCountsBriefs (A8): a link's briefs are the messages to its
// worker at the linked generation from a boss or a pane, in every state. A
// worker's or the daemon's message is no brief, nor one to another
// generation. Each cell is a worker of its own, so each is judged alone.
func TestSpawnRecordCountsBriefs(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	// Each worker is bound at generation 1, and linked there; gen is its
	// message's generation.
	type cell struct {
		sender, state string
		gen           int64
		want          int
	}
	cells := map[string]cell{"from a worker": {"worker", "queued", 1, 0}, "from the daemon": {"daemon", "queued", 1, 0}, "to generation 0": {"boss", "queued", 0, 0}}
	for _, sender := range []string{"boss", "pane"} {
		for _, state := range []string{"queued", "dispatching", "delivered", "failed"} {
			cells[sender+" "+state] = cell{sender, state, 1, 1}
		}
	}
	names := slices.Sorted(maps.Keys(cells))
	planned := make([]string, len(names))
	for i, name := range names {
		id := fmt.Sprintf("ha-coder-%d", i+1)
		planned[i] = id
		seedWorker(t, s, id, fmt.Sprintf("w14:p%d", i+1), 1)
		c := cells[name]
		var senderGen any
		if c.sender == "worker" {
			senderGen = 0
		}
		_, err := store.DB(s).ExecContext(ctx, `INSERT INTO messages (id, recipient_kind, recipient, project, generation,
	sender_kind, sender, sender_generation, body, state, created_at, updated_at)
VALUES (?, 'worker', ?, 'agentisan', ?, ?, 'ha-sender', ?, 'a brief', ?, 1, 1)`,
			fmt.Sprintf("%026d", i), id, c.gen, c.sender, senderGen, c.state)
		require.NoError(t, err, name)
	}
	require.NoError(t, s.BeginSpawn(ctx, spawnKey(func(k *store.SpawnKey) { k.Count = len(planned) }), planned, spawnAt))
	for _, id := range planned {
		link(t, s, retryKey, id, 1)
	}
	rec, err := s.SpawnRecord(ctx, retryKey)
	require.NoError(t, err)
	for i, name := range names {
		assert.Equal(t, cells[name].want, rec.Planned[i].Briefs, name)
	}
}

// TestSpawnKeysHaveNoProductionCaller (A9): the retry keys are dormant but
// for what part B wires, each name in the one file it may use it in: the
// daemon's prune line, its one use, in Options.prune's registry. No other
// non-test file in internal/ or cmd/ but spawnkeys.go uses their names or
// names their tables, and no Go string names a table of theirs with
// REPLACE.
func TestSpawnKeysHaveNoProductionCaller(t *testing.T) {
	t.Parallel()
	const own = "internal/store/spawnkeys.go"
	spawnNames := []string{
		"SpawnKeyRetention", "SpawnKey", "SpawnAnswer", "SpawnRecord", "SpawnLink",
		"BeginSpawn", "DropSpawn", "BindSpawned", "FinishSpawn", "PruneSpawnKeys",
		"ErrNoSpawnKey", "ErrSpawnKeyExists", "ErrSpawnKeyChanged",
	}
	wired := map[string][]string{
		"internal/daemon/daemon.go": {"PruneSpawnKeys"},
	}
	names, tables := map[string][]string{}, map[string][]string{}
	var replaces []string
	parsed := 0
	for _, dir := range []string{"internal", "cmd"} {
		n, tb, r, files := productionUses(t, filepath.Join("..", ".."), dir, spawnNames, []string{"spawn_requests", "spawn_request_workers"})
		maps.Copy(names, n)
		maps.Copy(tables, tb)
		replaces = append(replaces, r...)
		parsed += files
	}
	assert.Greater(t, parsed, 100, "the scan reached the module's packages")
	used := func(file string) []string {
		found := map[string]bool{}
		for _, use := range names[file] {
			found[use[strings.LastIndexByte(use, ' ')+1:]] = true
		}
		return slices.Sorted(maps.Keys(found))
	}
	assert.Equal(t, slices.Sorted(slices.Values(spawnNames)), used(own), "the scan finds every name in %s", own)
	assert.NotEmpty(t, tables[own], "the scan finds the retry keys' SQL")
	delete(names, own)
	delete(tables, own)
	daemonUses := names["internal/daemon/daemon.go"]
	if assert.Len(t, daemonUses, 1, "daemon.go names PruneSpawnKeys once") {
		assert.True(t, inMethod(t, "Options", "prune", daemonUses[0]), "its one use is Options.prune's registry: %s", daemonUses[0])
	}
	for file, want := range wired {
		assert.Equal(t, want, used(file), "%s uses exactly the names part B wires there", file)
		delete(names, file)
	}
	assert.Empty(t, names, "the retry keys are dormant until worker spawn takes a key (NERD-5254)")
	assert.Empty(t, tables, "only spawnkeys.go names their tables in SQL")
	assert.Empty(t, replaces, "no Go string names a retry key table with REPLACE in it")
}

// inMethod reports whether use, a productionUses entry ("path:line:col
// name"), lies in recv's method name in its file.
func inMethod(t *testing.T, recv, name, use string) bool {
	t.Helper()
	parts := strings.SplitN(strings.Fields(use)[0], ":", 3)
	require.Len(t, parts, 3, use)
	line, err := strconv.Atoi(parts[1])
	require.NoError(t, err)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, parts[0], nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		if id, ok := fn.Recv.List[0].Type.(*ast.Ident); ok && id.Name == recv {
			return fset.Position(fn.Pos()).Line <= line && line <= fset.Position(fn.End()).Line
		}
	}
	return false
}

// TestDropSpawnOnlyWhenNothingBound (A10): DropSpawn deletes a running
// record none of whose ids is bound, its planned rows with it, and says so;
// it keeps a running record with an id bound, and a done one, and says
// that.
func TestDropSpawnOnlyWhenNothingBound(t *testing.T) {
	t.Parallel()
	s := open(t, dbPath(t))
	ctx := t.Context()
	bound, finished := strings.Repeat("b", 32), strings.Repeat("c", 32)
	for _, key := range []string{retryKey, bound, finished} {
		require.NoError(t, s.BeginSpawn(ctx, spawnKey(func(k *store.SpawnKey) { k.Key = key }), []string{"ha-coder-1", "ha-coder-2"}, spawnAt))
	}
	seedWorker(t, s, "ha-coder-2", "w14:p2", 0)
	link(t, s, bound, "ha-coder-2", 0)
	_, err := store.DB(s).ExecContext(ctx, "UPDATE spawn_requests SET state = 'done', outcome = 'spawn_failed', message = 'm', finished_at = created_at WHERE retry_key = ?", finished)
	require.NoError(t, err)
	kept := map[string]store.SpawnRecord{}
	for _, key := range []string{bound, finished} {
		kept[key], err = s.SpawnRecord(ctx, key)
		require.NoError(t, err)
	}

	dropped, err := s.DropSpawn(ctx, retryKey)
	require.NoError(t, err)
	assert.True(t, dropped, "a running record with none bound")
	_, err = s.SpawnRecord(ctx, retryKey)
	require.ErrorIs(t, err, store.ErrNoSpawnKey)
	assert.Equal(t, [2]int{2, 4}, spawnRows(t, s), "its planned rows went with it")

	for key, want := range kept {
		dropped, err := s.DropSpawn(ctx, key)
		require.NoError(t, err)
		assert.False(t, dropped, key)
		got, err := s.SpawnRecord(ctx, key)
		require.NoError(t, err)
		assert.Equal(t, want, got, key)
	}
	dropped, err = s.DropSpawn(ctx, retryKey)
	require.NoError(t, err)
	assert.False(t, dropped, "a key with no record")
	_, err = s.DropSpawn(ctx, "not a key")
	require.ErrorIs(t, err, store.ErrInvalid)
}

// TestA73NamesEveryPart: A73's head lists every part retry-safe spawn is
// built in, in order, and counts none, since a part may split. Only the
// last makes the keys live, so the CLI's --retry-key is in F's entry alone.
func TestA73NamesEveryPart(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "adr", "0001", "A73.md"))
	require.NoError(t, err)
	_, list, ok := strings.Cut(string(raw), "Built in")
	require.True(t, ok, "A73's head says what it is built in")
	assert.True(t, strings.HasPrefix(list, " parts,"), "A73 counts no parts")
	var names []string
	for _, line := range strings.Split(list, "\n")[1:] {
		entry, ok := strings.CutPrefix(line, "- ")
		if !ok {
			break
		}
		name, _, _ := strings.Cut(entry, ",")
		names = append(names, name)
		assert.Equal(t, name == "F", strings.Contains(entry, "--retry-key"), "%s's entry and --retry-key", name)
	}
	assert.Equal(t, []string{"A", "B", "C-read", "C-write", "D", "E", "F"}, names)
}

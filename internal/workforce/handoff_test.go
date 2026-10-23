package workforce_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// handoffOf is the handoff token pane carries now; "" when none.
func (f *fixture) handoffOf(t *testing.T, pane string) string {
	t.Helper()
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	p := f.h.pane(pane)
	require.NotNil(t, p, pane)
	return p.tokens["handoff"]
}

// TestHandoffReady: by logical id, a hand-started worker's tab label, a pane
// id and a stale pane id, the worker's agent pane is marked with hready.sh's
// value, as source agentisan, for herdr's longest TTL; a mark again changes
// nothing but is written again; --clear takes it off with no TTL.
func TestHandoffReady(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	hand := f.h.addTab(f.ws, "ha-qa-3", "claude", "idle")
	other := f.worker(t, "ha-coder-2", "coder")
	stale := f.worker(t, "ha-review-1", "review")
	f.h.mu.Lock()
	f.h.alias["w3:p1"] = stale.PaneID
	f.h.mu.Unlock()

	tests := []struct {
		target string
		want   workforce.HandoffReadyResult
	}{
		{w.ID, workforce.HandoffReadyResult{ID: w.ID, Bound: true, Project: "herdr-agentisan", Group: "coder", PaneID: w.PaneID}},
		{"ha-qa-3", workforce.HandoffReadyResult{ID: hand, Project: "herdr-agentisan", Group: "qa", PaneID: hand}},
		{other.PaneID, workforce.HandoffReadyResult{ID: other.ID, Bound: true, Project: "herdr-agentisan", Group: "coder", PaneID: other.PaneID}},
		{"w3:p1", workforce.HandoffReadyResult{ID: stale.ID, Bound: true, Project: "herdr-agentisan", Group: "review", PaneID: stale.PaneID}},
	}
	for _, tc := range tests {
		tc.want.Ready, tc.want.Changed = true, true
		res, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: tc.target})
		require.NoError(t, err, tc.target)
		assert.Equal(t, tc.want, res, tc.target)
		assert.Equal(t, "⟳ READY TO RESTART", f.handoffOf(t, tc.want.PaneID), tc.target)
	}
	writes := f.h.params("pane.report_metadata")
	require.Len(t, writes, len(tests))
	assert.Equal(t, map[string]any{
		"pane_id": w.PaneID, "source": "agentisan", "tokens": map[string]any{"handoff": "⟳ READY TO RESTART"}, "ttl_ms": float64(86_400_000),
	}, writes[0])

	res, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: w.ID})
	require.NoError(t, err)
	assert.True(t, res.Ready)
	assert.False(t, res.Changed, "the pane carried the mark already")
	assert.Len(t, f.h.params("pane.report_metadata"), len(tests)+1, "written again all the same, which renews its TTL")

	res, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: w.ID, Clear: true})
	require.NoError(t, err)
	assert.Equal(t, workforce.HandoffReadyResult{ID: w.ID, Bound: true, Project: "herdr-agentisan", Group: "coder", PaneID: w.PaneID, Changed: true}, res)
	assert.Empty(t, f.handoffOf(t, w.PaneID))
	writes = f.h.params("pane.report_metadata")
	assert.Equal(t, map[string]any{"pane_id": w.PaneID, "source": "agentisan", "tokens": map[string]any{"handoff": ""}}, writes[len(writes)-1], "a clear has no TTL")

	res, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: w.ID, Clear: true})
	require.NoError(t, err)
	assert.False(t, res.Ready)
	assert.False(t, res.Changed, "nothing to clear")
	assert.Equal(t, "⟳ READY TO RESTART", f.handoffOf(t, other.PaneID), "only the target's mark is cleared")
}

// TestHandoffReadyReplacesAnotherWritersMark: a mark hready.sh left, with
// the same value or another, is written over as agentisan's, which is the
// cutover (A33); its value unchanged is Changed false.
func TestHandoffReadyReplacesAnotherWritersMark(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	same := f.worker(t, "ha-coder-1", "coder")
	other := f.worker(t, "ha-coder-2", "coder")
	f.h.setPane(same.PaneID, func(p *fakePane) { p.tokens = map[string]string{"handoff": "⟳ READY TO RESTART", "ctx": "71"} })
	f.h.setPane(other.PaneID, func(p *fakePane) { p.tokens = map[string]string{"handoff": "ready"} })

	res, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: same.ID})
	require.NoError(t, err)
	assert.False(t, res.Changed)
	res, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: other.ID})
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, "⟳ READY TO RESTART", f.handoffOf(t, other.PaneID))
	for _, w := range f.h.params("pane.report_metadata") {
		assert.Equal(t, "agentisan", w["source"])
	}
	f.h.mu.Lock()
	assert.Equal(t, "71", f.h.pane(same.PaneID).tokens["ctx"], "no other key is touched")
	f.h.mu.Unlock()
}

// TestHandoffReadyFindsTheCaller: with no target the mark goes on the
// caller's confirmed pane; with none listed, or one listed with no terminal,
// on the pane of the live row HERDR_AGENTISAN_WORKER names; with neither, it
// is a bad request.
func TestHandoffReadyFindsTheCaller(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	w2 := f.worker(t, "ha-coder-2", "coder")

	res, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{CallerPane: w.PaneID, Worker: w2.ID})
	require.NoError(t, err)
	assert.Equal(t, w.PaneID, res.PaneID, "the confirmed pane comes first")
	res, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Worker: w2.ID})
	require.NoError(t, err)
	assert.Equal(t, w2.PaneID, res.PaneID)
	res, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{CallerPane: "w10:p99", Worker: w.ID, Clear: true})
	require.NoError(t, err)
	assert.Equal(t, w.PaneID, res.PaneID, "a caller pane herdr no longer lists falls back to the variable")
	assert.True(t, res.Changed)
	blank := f.h.addTab(f.ws, "ha-coder-3", "claude", "idle")
	f.h.setPane(blank, func(p *fakePane) { p.terminal = "" })
	res, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{CallerPane: blank, Worker: w2.ID, Clear: true})
	require.NoError(t, err)
	assert.Equal(t, w2.PaneID, res.PaneID, "a caller pane listed with no terminal falls back to the variable, as send's sender does")

	for name, req := range map[string]workforce.HandoffReadyRequest{
		"nothing":                    {},
		"a gone pane and no worker":  {CallerPane: "w10:p99"},
		"a worker with no live row":  {Worker: "ha-coder-9"},
		"a pane id as the worker id": {Worker: w.PaneID},
	} {
		_, err := f.svc.HandoffReady(t.Context(), req)
		require.ErrorIs(t, err, workforce.ErrBadRequest, name)
	}
}

// TestHandoffReadyTrustsTheRowOverTheTab: a live row decides which pane is
// its worker's, whatever tab is labelled with its id; a row bound to no pane,
// lost, with no terminal or one herdr lists nowhere, marks nothing and is
// left as it was, even when a tab is labelled with its id.
func TestHandoffReadyTrustsTheRowOverTheTab(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	bound := f.h.addTab(f.ws, "ha-coder-3", "claude", "idle")
	f.bindRow(t, "ha-coder-4", "coder", bound)
	labelled := f.h.addTab(f.ws, "ha-coder-4", "claude", "idle")

	res, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: "ha-coder-4"})
	require.NoError(t, err)
	assert.Equal(t, bound, res.PaneID)
	assert.Empty(t, f.handoffOf(t, labelled))

	lost := f.worker(t, "ha-coder-1", "coder")
	f.markLost(t, lost.ID)
	f.bindRow(t, "ha-coder-2", "coder", "w10:p98")
	f.exec(t, "UPDATE workers SET terminal_id = '' WHERE logical_id = 'ha-coder-2'")
	f.bindRow(t, "ha-qa-1", "qa", "w10:p99")
	f.exec(t, "UPDATE workers SET terminal_id = 'term_closed' WHERE logical_id = 'ha-qa-1'")
	before := len(f.h.params("pane.report_metadata"))
	for _, id := range []string{lost.ID, "ha-coder-2", "ha-qa-1"} {
		_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: id})
		require.ErrorIs(t, err, workforce.ErrUnknownWorker, id)
		_, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Worker: id})
		require.ErrorIs(t, err, workforce.ErrUnknownWorker, "by the variable: %s", id)
	}
	assert.Len(t, f.h.params("pane.report_metadata"), before)
	assert.Equal(t, store.StatusLost, f.row(t, lost.ID).Status, "the row is left as it was")
	assert.Equal(t, store.StatusActive, f.row(t, "ha-qa-1").Status)
	assert.Empty(t, f.handoffOf(t, lost.PaneID))
}

// TestHandoffReadyMarksOnlyATeamsWorkers: the boss's tab, a shell, a pane in
// no team's group and names nobody holds, the boss's tab label among them,
// are refused and nothing is written; a ◆ space's agent is the unnamed
// project's worker, ◆ boss is the boss's, and an agent herdr knows only by
// name, as a held one, is a worker.
func TestHandoffReadyMarksOnlyATeamsWorkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.worker(t, "ha-coder-1", "coder")
	shell := f.h.addTab(f.ws, "ha-coder-2", "", "unknown")
	split := f.h.addPane(f.boss, "claude", "idle")
	notes := f.h.addTab(f.ws, "notes", "claude", "idle")
	theirs := f.h.addTab(f.h.addWorkspace("elsewhere"), "ha-coder-6", "claude", "idle")
	spacesBoss := f.h.addTab(f.h.addWorkspace("◆ boss"), "1", "claude", "working")
	spacesCoder := f.h.addTab(f.h.addWorkspace("◆ coders"), "1", "claude", "idle")

	for name, target := range map[string]string{
		"the boss's pane":           f.boss,
		"a split in the boss's tab": split,
		"◆ boss":                    spacesBoss,
		"a shell by tab label":      "ha-coder-2",
		"a shell by pane id":        shell,
		"a tab that is no worker's": notes,
		"another workspace's pane":  theirs,
	} {
		_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: target})
		require.ErrorIs(t, err, workforce.ErrNotAWorker, name)
	}
	_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{CallerPane: f.boss})
	require.ErrorIs(t, err, workforce.ErrNotAWorker, "the boss marking itself")
	for _, name := range []string{"ha-coder-9", "HA-CODER-1", "ha-cöder-1", "ha-coder-1\n", "ha-coder-\xff", "-x", " ", "ha-coder-1\x00", "ha-coder-6", "ha-boss"} {
		_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: name})
		require.ErrorIs(t, err, workforce.ErrUnknownWorker, "%q", name)
	}
	assert.Empty(t, f.h.params("pane.report_metadata"))
	assert.Empty(t, f.h.params("pane.get"), "nothing is proven for a refusal")

	res, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: spacesCoder})
	require.NoError(t, err)
	assert.Equal(t, workforce.HandoffReadyResult{ID: spacesCoder, Project: "", Group: "coders", PaneID: spacesCoder, Ready: true, Changed: true}, res)

	held := f.h.addTab(f.ws, "ha-coder-5", "claude", "idle")
	f.h.setPane(held, func(p *fakePane) { p.agent = "" })
	res, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: "ha-coder-5"})
	require.NoError(t, err, "an agent herdr knows only by name, as a held one, is no shell")
	assert.Equal(t, held, res.PaneID)
}

// TestHandoffReadyRechecksThePaneBeforeWriting: a pane that shows another
// terminal by the time pane.get asks, one gone meanwhile and one herdr lists
// with no terminal are ErrPaneChanged, and nothing is written, since retire
// --after-handoff would trust a mark on a reissued pane id.
func TestHandoffReadyRechecksThePaneBeforeWriting(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(f *fakeHerdr, pane string){
		"another terminal": func(f *fakeHerdr, pane string) { f.pane(pane).terminal = "term_new" },
		"gone":             func(f *fakeHerdr, pane string) { f.dropPane(pane) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			f.h.mu.Lock()
			f.h.onCall = func(method string) {
				if method == "pane.get" {
					change(f.h, w.PaneID)
				}
			}
			f.h.mu.Unlock()
			_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: w.PaneID})
			require.ErrorIs(t, err, workforce.ErrPaneChanged)
			assert.Empty(t, f.h.params("pane.report_metadata"))
		})
	}

	f := newFixture(t, teamConfig)
	pane := f.h.addTab(f.ws, "ha-coder-3", "claude", "idle")
	f.h.setPane(pane, func(p *fakePane) { p.terminal = "" })
	_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: pane})
	require.ErrorIs(t, err, workforce.ErrPaneChanged, "no terminal proves the pane")
	assert.Empty(t, f.h.params("pane.report_metadata"))

	w := f.worker(t, "ha-coder-1", "coder")
	f.h.mu.Lock()
	f.h.fail["pane.report_metadata"] = "pane_not_found"
	f.h.mu.Unlock()
	_, err = f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: w.ID})
	require.ErrorIs(t, err, workforce.ErrPaneChanged, "a pane closed between pane.get and the write")
	assert.NotErrorIs(t, err, workforce.ErrHerdr)
}

// TestHandoffReadyTwiceAtOnce: marks of one worker at once are serialized
// by the workforce lock, so exactly one reads the pane unmarked and says it
// changed it.
func TestHandoffReadyTwiceAtOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	w := f.worker(t, "ha-coder-1", "coder")
	const n = 8
	changed := make(chan bool, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			res, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: w.ID})
			assert.NoError(t, err)
			changed <- res.Changed
		})
	}
	wg.Wait()
	close(changed)
	count := 0
	for c := range changed {
		if c {
			count++
		}
	}
	assert.Equal(t, 1, count)
	assert.Len(t, f.h.params("pane.report_metadata"), n)
}

// TestHandoffReadyWhenHerdrFails: each herdr call that fails, and a stale
// id herdr cannot resolve for a reason other than no such pane, is ErrHerdr;
// a configuration that does not resolve is ErrConfig.
func TestHandoffReadyWhenHerdrFails(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"pane.list", "workspace.list", "tab.list", "agent.list", "pane.get", "pane.report_metadata"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, teamConfig)
			w := f.worker(t, "ha-coder-1", "coder")
			f.h.mu.Lock()
			f.h.fail[method] = "internal_error"
			f.h.mu.Unlock()
			_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: w.ID})
			require.ErrorIs(t, err, workforce.ErrHerdr)
			assert.Empty(t, f.handoffOf(t, w.PaneID))
		})
	}

	f := newFixture(t, teamConfig)
	f.h.mu.Lock()
	f.h.fail["pane.process_info"] = "internal_error"
	f.h.mu.Unlock()
	_, err := f.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: "w3:p1"})
	require.ErrorIs(t, err, workforce.ErrHerdr, "a stale id herdr failed to resolve is not no such worker")

	broken := newFixture(t, "schema_version = 1\n[projects.herdr-agentisan]\nrepo = \"REPO\"\nlayout = \"grid\"\n")
	_, err = broken.svc.HandoffReady(t.Context(), workforce.HandoffReadyRequest{Target: "ha-coder-1"})
	require.ErrorIs(t, err, workforce.ErrConfig)
}

package daemon_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// moveParams are pane.zoom's and pane.focus's.
type moveParams struct {
	PaneID string `json:"pane_id"`
	Mode   string `json:"mode"`
}

// list answers pane.list from f.panes, f.mu held.
func (f *fakeHerdr) list() herdrtest.Reply {
	if code := f.fail["pane.list"]; code != "" {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: code}}
	}
	panes := []map[string]any{}
	for _, p := range f.panes {
		panes = append(panes, map[string]any{"pane_id": p, "workspace_id": "w1", "focused": p == f.focused})
	}
	return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": panes}}
}

// move answers pane.zoom and pane.focus as herdr 0.9.3 does. Both focus their
// target, pane.zoom whatever its mode (apply_pane_zoom,
// src/app/actions.rs:822). A move that changes focus queues one pane_focused,
// which herdr streams after answering (src/server/headless.rs:467), unless
// the test makes it eager.
func (f *fakeHerdr) move(r herdrtest.Request) herdrtest.Reply {
	var p moveParams
	_ = json.Unmarshal(r.Params, &p)
	f.mu.Lock()
	hang := f.hang
	f.mu.Unlock()
	if hang != nil && r.Method == "pane.zoom" {
		<-hang
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if code := f.fail[r.Method]; code != "" {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: code}}
	}
	if !slices.Contains(f.panes, p.PaneID) {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane not found"}}
	}
	f.focusLocked(p.PaneID)
	if f.eager {
		f.flushLocked()
	}
	if r.Method == "pane.focus" {
		return herdrtest.Reply{Result: map[string]any{"type": "pane_info", "pane": map[string]any{"pane_id": p.PaneID, "workspace_id": "w1", "focused": true}}}
	}
	reason := any(nil)
	if p.Mode == "off" && !f.zoomed[p.PaneID] {
		reason = "already_unzoomed"
	}
	f.zoomed[p.PaneID] = p.Mode == "on"
	return herdrtest.Reply{Result: map[string]any{"type": "pane_zoom", "zoom": map[string]any{
		"pane_id": p.PaneID, "focused_pane_id": p.PaneID, "zoomed": f.zoomed[p.PaneID], "changed": reason == nil, "reason": reason,
	}}}
}

func (f *fakeHerdr) focusLocked(pane string) {
	if pane != f.focused {
		f.focused = pane
		f.outbox = append(f.outbox, herdrtest.PaneFocused(pane, "w1"))
	}
}

func (f *fakeHerdr) flushLocked() {
	for _, ev := range f.outbox {
		f.live.lines <- ev
	}
	f.outbox = nil
}

// flush streams the focus events herdr owes the daemon.
func (f *fakeHerdr) flush() { f.set((*fakeHerdr).flushLocked) }

// user moves focus to pane as a click or a dashboard jump (zoomed) would.
func (f *fakeHerdr) user(pane string, zoom bool) {
	f.set(func(f *fakeHerdr) {
		f.zoomed[pane] = zoom
		f.focusLocked(pane)
		f.flushLocked()
	})
}

// set changes the fake's state under its lock.
func (f *fakeHerdr) set(change func(f *fakeHerdr)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// moves is every pane.zoom and pane.focus herdr was asked for, in order.
func (f *fakeHerdr) moves() []string {
	out := []string{}
	for _, r := range f.srv.Requests() {
		var p moveParams
		_ = json.Unmarshal(r.Params, &p)
		switch r.Method {
		case "pane.zoom":
			out = append(out, "zoom "+p.Mode+" "+p.PaneID)
		case "pane.focus":
			out = append(out, "focus "+p.PaneID)
		}
	}
	return out
}

// calls counts the requests for method herdr got.
func (f *fakeHerdr) calls(method string) int {
	n := 0
	for _, r := range f.srv.Requests() {
		if r.Method == method {
			n++
		}
	}
	return n
}

// plant writes focus rows, oldest first, into a state.db the daemon will
// open: raw inserts, so a row repeating the one before it lands too.
func plant(t *testing.T, dir string, h *fakeHerdr, panes ...string) {
	t.Helper()
	p := paths(t, dir, h)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	st, err := store.Open(t.Context(), p.DB)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	raw, err := sql.Open("sqlite", p.DB)
	require.NoError(t, err)
	defer raw.Close() //nolint:errcheck // test setup
	for i, pane := range panes {
		_, err := raw.ExecContext(t.Context(), "INSERT INTO focus (pane_id, focused_at) VALUES (?, ?)", pane, i+1)
		require.NoError(t, err)
	}
}

// backDaemon runs a daemon on a herdr listing panes with focus on focused
// ("" for none), and waits until the daemon follows focus. The history it
// starts with is planted, then focused.
func backDaemon(t *testing.T, o func(*daemon.Options), focused string, panes []string, planted ...string) (*fakeHerdr, string) {
	t.Helper()
	h := newHerdr(t, 22)
	h.set(func(f *fakeHerdr) { f.panes, f.zoomed, f.focused = panes, map[string]bool{}, focused })
	dir := shortDir(t)
	if len(planted) > 0 {
		plant(t, dir, h, planted...)
	}
	op := opts(t, dir, h)
	if o != nil {
		o(&op)
	}
	run(t, op)
	h.nextStream(t)
	require.Eventually(t, func() bool { return h.calls("pane.list") > 0 }, ready, 10*time.Millisecond)
	want := slices.Clone(planted)
	if focused != "" && (len(want) == 0 || want[len(want)-1] != focused) {
		want = append(want, focused)
	}
	slices.Reverse(want)
	if len(want) > 0 {
		focusIs(t, dir, h, want...)
	}
	return h, dir
}

func back(t *testing.T, dir string, h *fakeHerdr) (daemon.BackResult, error) {
	t.Helper()
	return daemon.Back(t.Context(), socketPath(t, dir, h.path))
}

// newest waits until the newest focus row is pane: the daemon has seen herdr
// announce the move, since the walker observes before the row is written.
func newest(t *testing.T, dir string, h *fakeHerdr, pane string) {
	t.Helper()
	db := paths(t, dir, h).DB
	require.Eventually(t, func() bool {
		got := query(t, db, "SELECT pane_id FROM focus ORDER BY seq DESC LIMIT 1")
		return len(got) == 1 && got[0] == pane
	}, ready, 10*time.Millisecond, "the daemon never recorded focus on %s", pane)
}

// jumps opens a history: focus starts on panes[0], then the user jumps to
// each later pane in turn, zooming it as the dashboard does.
func jumps(t *testing.T, panes ...string) (*fakeHerdr, string) {
	t.Helper()
	h, dir := backDaemon(t, nil, panes[0], panes)
	for _, p := range panes[1:] {
		h.user(p, true)
		newest(t, dir, h, p)
	}
	return h, dir
}

// TestBackAfterAJumpReturnsAndUnzooms is the item's done-when: after a jump,
// Back returns to the previous pane and un-zooms the one it leaves. The
// order is herdr's: pane.zoom focuses its target, so un-zooming after the
// focus would pull the user back.
func TestBackAfterAJumpReturnsAndUnzooms(t *testing.T) {
	t.Parallel()
	h, dir := jumps(t, "w1:a", "w1:b")

	res, err := back(t, dir, h)
	require.NoError(t, err)
	assert.Equal(t, daemon.BackResult{To: "w1:a", From: "w1:b"}, res)
	assert.Equal(t, []string{"zoom off w1:b", "focus w1:a"}, h.moves())
	h.set(func(f *fakeHerdr) {
		assert.Equal(t, "w1:a", f.focused)
		assert.False(t, f.zoomed["w1:b"], "the pane left is un-zoomed")
	})
	h.flush()
	focusIs(t, dir, h, "w1:a", "w1:b", "w1:a")
}

// TestBackWalksTheHistory: after jumps A, B, C, D, Backs go to C, B and A,
// however late herdr announces Back's own moves, then refuse. Each Back
// leaves its focus in the history, which the walk does not follow.
func TestBackWalksTheHistory(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		eager, late bool
	}{
		"events after each answer":       {},
		"events only after the last one": {late: true},
		"events before each answer":      {eager: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, dir := jumps(t, "w1:a", "w1:b", "w1:c", "w1:d")
			h.set(func(f *fakeHerdr) { f.eager = tc.eager })

			for _, want := range []daemon.BackResult{{To: "w1:c", From: "w1:d"}, {To: "w1:b", From: "w1:c"}, {To: "w1:a", From: "w1:b"}} {
				res, err := back(t, dir, h)
				require.NoError(t, err)
				require.Equal(t, want, res)
				if !tc.late {
					h.flush()
					newest(t, dir, h, want.To)
				}
			}
			h.flush()
			focusIs(t, dir, h, "w1:a", "w1:b", "w1:c", "w1:d", "w1:c", "w1:b", "w1:a")
			_, err := back(t, dir, h)
			require.ErrorIs(t, err, daemon.ErrNoHistory, "nothing is older than A")
			assert.Equal(t, []string{"zoom off w1:d", "focus w1:c", "zoom off w1:c", "focus w1:b", "zoom off w1:b", "focus w1:a"}, h.moves())
		})
	}
}

// TestBackWalksAFullHistory: the walk follows the history as it stood when
// it began, so the rows each Back adds, and the oldest they push out of the
// 32 kept, do not cut it short.
func TestBackWalksAFullHistory(t *testing.T) {
	t.Parallel()
	var panes []string
	for i := range store.FocusCap {
		panes = append(panes, fmt.Sprintf("w1:p%02d", i))
	}
	h, dir := backDaemon(t, nil, panes[len(panes)-1], panes, panes...)
	for i := len(panes) - 2; i >= 0; i-- {
		res, err := back(t, dir, h)
		require.NoError(t, err)
		require.Equal(t, panes[i], res.To)
		h.flush()
		newest(t, dir, h, res.To)
	}
	_, err := back(t, dir, h)
	require.ErrorIs(t, err, daemon.ErrNoHistory)
}

// TestBackDoublePressWalksInTurn: two Backs at once, as a double press of the
// key sends, run one after the other, so one goes to C and the other to B.
func TestBackDoublePressWalksInTurn(t *testing.T) {
	t.Parallel()
	h, dir := jumps(t, "w1:a", "w1:b", "w1:c", "w1:d")
	got := make(chan string, 2)
	for range 2 {
		go func() {
			res, err := back(t, dir, h)
			assert.NoError(t, err)
			got <- res.To
		}()
	}
	to := []string{<-got, <-got}
	slices.Sort(to)
	assert.Equal(t, []string{"w1:b", "w1:c"}, to)
}

// TestBackEndsTheWalkOnAnotherFocus: any focus Back did not cause ends the
// walk, and the next Back starts again from the newest history.
func TestBackEndsTheWalkOnAnotherFocus(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		backs   int
		then    func(h *fakeHerdr)
		history []string // the history to wait for, newest first; nil when herdr announces nothing
		want    []daemon.BackResult
	}{
		// History A B C D C B E: back from E is B.
		"a dashboard jump": {
			backs: 2, then: func(h *fakeHerdr) { h.user("w1:e", true) },
			history: []string{"w1:e", "w1:b", "w1:c", "w1:d", "w1:c", "w1:b", "w1:a"},
			want:    []daemon.BackResult{{To: "w1:b", From: "w1:e"}},
		},
		// pane.list shows the walk's own pane again; only the events tell.
		"the user leaves and comes back": {
			backs: 1, then: func(h *fakeHerdr) {
				h.user("w1:e", false)
				h.user("w1:c", false)
			},
			history: []string{"w1:c", "w1:e", "w1:c", "w1:d", "w1:c", "w1:b", "w1:a"},
			want:    []daemon.BackResult{{To: "w1:e", From: "w1:c"}},
		},
		// Back runs before herdr announces the move: pane.list tells, and the
		// late announcement does not end the walk this Back starts.
		"a move herdr has not announced": {
			backs: 1, then: func(h *fakeHerdr) { h.set(func(f *fakeHerdr) { f.focusLocked("w1:e") }) },
			want: []daemon.BackResult{{To: "w1:c", From: "w1:e"}, {To: "w1:d", From: "w1:c"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, dir := jumps(t, "w1:a", "w1:b", "w1:c", "w1:d")
			h.set(func(f *fakeHerdr) { f.panes = append(f.panes, "w1:e") })
			for range tc.backs {
				res, err := back(t, dir, h)
				require.NoError(t, err)
				h.flush()
				newest(t, dir, h, res.To)
			}
			tc.then(h)
			if tc.history != nil {
				focusIs(t, dir, h, tc.history...)
			}

			for _, want := range tc.want {
				res, err := back(t, dir, h)
				require.NoError(t, err)
				require.Equal(t, want, res)
				h.flush()
				newest(t, dir, h, want.To)
			}
		})
	}
}

// TestBackSkips: Back passes over a pane herdr no longer lists, the pane the
// user is on, and a row repeating the one before it.
func TestBackSkips(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		planted []string
		panes   []string
		focused string
		then    func(h *fakeHerdr)
		want    []daemon.BackResult
	}{
		"a closed pane": {
			planted: []string{"w1:a", "w1:b", "w1:c", "w1:d"}, focused: "w1:d",
			panes: []string{"w1:a", "w1:b", "w1:d"},
			want:  []daemon.BackResult{{To: "w1:b", From: "w1:d"}, {To: "w1:a", From: "w1:b"}},
		},
		// D closed, and herdr moved focus to C without the daemon hearing.
		"the current pane": {
			planted: []string{"w1:a", "w1:b", "w1:c", "w1:d"}, focused: "w1:d",
			panes: []string{"w1:a", "w1:b", "w1:c", "w1:d"},
			then: func(h *fakeHerdr) {
				h.set(func(f *fakeHerdr) { f.panes, f.focused = f.panes[:3], "w1:c" })
			},
			want: []daemon.BackResult{{To: "w1:b", From: "w1:c"}, {To: "w1:a", From: "w1:b"}},
		},
		// herdr reports no focused pane: nothing to un-zoom.
		"no pane focused": {
			planted: []string{"w1:a", "w1:b"}, panes: []string{"w1:a", "w1:b"}, focused: "w1:b",
			then: func(h *fakeHerdr) { h.set(func(f *fakeHerdr) { f.focused = "" }) },
			want: []daemon.BackResult{{To: "w1:b"}, {To: "w1:a", From: "w1:b"}},
		},
		"repeated rows": {
			planted: []string{"w1:a", "w1:b", "w1:b", "w1:c", "w1:c"}, focused: "w1:c",
			panes: []string{"w1:a", "w1:b", "w1:c"},
			want:  []daemon.BackResult{{To: "w1:b", From: "w1:c"}, {To: "w1:a", From: "w1:b"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, dir := backDaemon(t, nil, tc.focused, tc.panes, tc.planted...)
			if tc.then != nil {
				tc.then(h)
			}
			for _, want := range tc.want {
				res, err := back(t, dir, h)
				require.NoError(t, err)
				require.Equal(t, want, res)
				h.flush()
				newest(t, dir, h, want.To)
			}
			_, err := back(t, dir, h)
			require.ErrorIs(t, err, daemon.ErrNoHistory)
		})
	}
}

// TestBackRefuses: with nowhere older to go, Back answers no_history and
// never moves anything. When the history alone shows it, herdr is not even
// asked for its panes.
func TestBackRefuses(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		planted []string
		panes   []string
		focused string
		backs   int // Backs that succeed first
		lists   int // pane.list calls the refusal costs
	}{
		"an empty history":           {panes: []string{"w1:a"}},
		"a single row":               {panes: []string{"w1:a"}, focused: "w1:a"},
		"one pane twice":             {planted: []string{"w1:a", "w1:a"}, panes: []string{"w1:a"}, focused: "w1:a"},
		"everything older is closed": {planted: []string{"w1:a", "w1:b"}, panes: []string{"w1:b"}, focused: "w1:b", lists: 1},
		"the walk is at its oldest":  {planted: []string{"w1:a", "w1:b"}, panes: []string{"w1:a", "w1:b"}, focused: "w1:b", backs: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, dir := backDaemon(t, nil, tc.focused, tc.panes, tc.planted...)
			for range tc.backs {
				res, err := back(t, dir, h)
				require.NoError(t, err)
				h.flush()
				newest(t, dir, h, res.To)
			}
			lists, moves := h.calls("pane.list"), len(h.moves())

			_, err := back(t, dir, h)
			require.ErrorIs(t, err, daemon.ErrNoHistory)
			var re *daemon.RequestError
			require.ErrorAs(t, err, &re)
			assert.Equal(t, "no_history", re.Code)
			assert.Len(t, h.moves(), moves, "a refused Back moves nothing")
			assert.Equal(t, lists+tc.lists, h.calls("pane.list"), "herdr is asked only when the history alone cannot decide")
		})
	}
}

// TestBackSurfacesHerdrErrors: a herdr call that fails, or does not answer
// within the op's budget, stops Back where it is with herdr_failed, which
// reaches the client inside its own timeout. The walk does not move, so
// once herdr recovers Back goes where it would have.
func TestBackSurfacesHerdrErrors(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		fail  map[string]string
		hang  bool
		moves []string
	}{
		"pane.list fails":                 {fail: map[string]string{"pane.list": "server_unavailable"}, moves: []string{}},
		"un-zoom fails":                   {fail: map[string]string{"pane.zoom": "server_unavailable"}, moves: []string{"zoom off w1:c"}},
		"focus fails":                     {fail: map[string]string{"pane.focus": "server_unavailable"}, moves: []string{"zoom off w1:c", "focus w1:b"}},
		"the target closed before focus":  {fail: map[string]string{"pane.focus": "pane_not_found"}, moves: []string{"zoom off w1:c", "focus w1:b"}},
		"herdr never answers the un-zoom": {hang: true, moves: []string{"zoom off w1:c"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			budget := func(o *daemon.Options) { o.BackBudget = 300 * time.Millisecond }
			h, dir := backDaemon(t, budget, "w1:c", []string{"w1:a", "w1:b", "w1:c"}, "w1:a", "w1:b", "w1:c")
			if tc.hang {
				release := make(chan struct{})
				t.Cleanup(func() { close(release) })
				h.set(func(f *fakeHerdr) { f.hang = release })
			}
			h.set(func(f *fakeHerdr) { f.fail = tc.fail })

			start := time.Now()
			_, err := back(t, dir, h)
			require.ErrorIs(t, err, daemon.ErrHerdrCall)
			assert.Less(t, time.Since(start), 4*time.Second, "answered within the budget, not the client's timeout")
			var re *daemon.RequestError
			require.ErrorAs(t, err, &re)
			assert.Equal(t, "herdr_failed", re.Code)
			assert.Equal(t, tc.moves, h.moves())

			h.set(func(f *fakeHerdr) { f.fail, f.hang, f.focused = nil, nil, "w1:c" })
			res, err := back(t, dir, h)
			require.NoError(t, err)
			assert.Equal(t, daemon.BackResult{To: "w1:b", From: "w1:c"}, res)
		})
	}
}

// TestBackRefusalCodes: each failure has its code, and a client branches on
// the code with errors.Is. A history the daemon cannot read is state_failed,
// and herdr is not asked anything.
func TestBackRefusalCodes(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} })
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	require.NoError(t, st.Close())

	_, err = daemon.BackOver(t.Context(), herdr.Client{SocketPath: srv.Path}, st)
	require.ErrorIs(t, err, store.ErrStore)
	assert.Equal(t, "state_failed", daemon.BackCode(err))
	assert.Empty(t, srv.Requests())
	assert.Equal(t, "no_history", daemon.BackCode(daemon.ErrNoHistory))
	assert.Equal(t, "herdr_failed", daemon.BackCode(daemon.ErrHerdrCall))

	for code, want := range map[string]error{"no_history": daemon.ErrNoHistory, "herdr_failed": daemon.ErrHerdrCall, "state_failed": nil} {
		err := error(&daemon.RequestError{Code: code, Message: "m"})
		require.ErrorIs(t, err, daemon.ErrRequest)
		for _, s := range []error{daemon.ErrNoHistory, daemon.ErrHerdrCall} {
			assert.Equal(t, s == want, errors.Is(err, s), "%s is %v", code, s)
		}
	}
}

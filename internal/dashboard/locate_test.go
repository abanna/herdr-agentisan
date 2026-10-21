package dashboard_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// pid is the dashboard's process id in these tests: a fake one, so nothing
// depends on the test binary's own.
const pid = 4242

// paneHerdr is a herdr whose pane.list lists panes, in order, and whose
// pane.process_info answers from procs; a pane with no entry there is
// closed by the time it is asked.
type paneHerdr struct {
	mu    sync.Mutex
	panes []map[string]any
	procs map[string]map[string]any
	srv   *herdrtest.Server
}

func newPaneHerdr(t *testing.T) *paneHerdr {
	t.Helper()
	h := &paneHerdr{panes: []map[string]any{}, procs: map[string]map[string]any{}}
	h.srv = herdrtest.Start(t, h.serve)
	return h
}

func (h *paneHerdr) serve(r herdrtest.Request) herdrtest.Reply {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Method {
	case "pane.list":
		return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": slices.Clone(h.panes)}}
	case "pane.process_info":
		var p struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(r.Params, &p)
		info, ok := h.procs[p.PaneID]
		if !ok {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane " + p.PaneID + " not found"}}
		}
		return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": info}}
	}
	return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "invalid_request", Message: "unknown method " + r.Method}}
}

// pane lists pane id in workspace ws, run by a shell of pid shell with a
// foreground job of pid job, made of the processes fg.
func (h *paneHerdr) pane(id, ws string, shell, job int, fg ...int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.panes = append(h.panes, map[string]any{"pane_id": id, "workspace_id": ws, "focused": false})
	procs := []map[string]any{}
	for _, p := range fg {
		procs = append(procs, map[string]any{"pid": p, "name": "proc"})
	}
	h.procs[id] = map[string]any{"pane_id": id, "shell_pid": shell, "foreground_process_group_id": job, "foreground_processes": procs}
}

// move moves pane id into workspace ws.
func (h *paneHerdr) move(id, ws string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.panes {
		if p["pane_id"] == id {
			p["workspace_id"] = ws
		}
	}
}

// asked is every pane.process_info's pane, in order, after the pane.list
// count.
func (h *paneHerdr) asked() (lists int, panes []string) {
	for _, r := range h.srv.Requests() {
		switch r.Method {
		case "pane.list":
			lists++
		case "pane.process_info":
			var p struct {
				PaneID string `json:"pane_id"`
			}
			_ = json.Unmarshal(r.Params, &p)
			panes = append(panes, p.PaneID)
		}
	}
	return lists, panes
}

func (h *paneHerdr) locator(paneID, workspaceID string) *dashboard.Locator {
	return &dashboard.Locator{Herdr: herdr.Client{SocketPath: h.srv.Path}, PID: pid, PaneID: paneID, WorkspaceID: workspaceID}
}

// TestLocatorFindsItsPane is A24's self-location: the dashboard's pane is the
// one that runs its pid, whatever HERDR_PANE_ID and HERDR_WORKSPACE_ID say,
// and the pane it is in now decides the workspace.
func TestLocatorFindsItsPane(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		layout          func(h *paneHerdr)
		paneID, envWS   string
		want            string
		wantAsked       []string
		wantNoPlaceLine bool
	}{
		// The settled case: the pane the environment names runs the pid,
		// and herdr is asked twice in all.
		"the pane HERDR_PANE_ID names runs it as its shell": {
			layout: func(h *paneHerdr) { h.pane("w1:p1", "w1", 11, 11); h.pane("w2:p1", "w2", pid, pid) },
			paneID: "w2:p1", envWS: "w2", want: "w2", wantAsked: []string{"w2:p1"},
		},
		// Run by hand from a shell in the pane: the dashboard is the pane's
		// foreground job, not its shell.
		"as the pane's foreground job": {
			layout: func(h *paneHerdr) { h.pane("w1:p1", "w1", 11, 11); h.pane("w2:p1", "w2", 22, pid, pid) },
			paneID: "w2:p1", want: "w2", wantAsked: []string{"w2:p1"},
		},
		"as one of the pane's foreground processes": {
			layout: func(h *paneHerdr) { h.pane("w2:p1", "w2", 22, 23, 23, pid) },
			paneID: "w2:p1", want: "w2", wantAsked: []string{"w2:p1"},
		},
		// Moved to another workspace since its shell started: the
		// environment's workspace is stale and is not consulted.
		"a moved pane, its environment's workspace stale": {
			layout: func(h *paneHerdr) { h.pane("w1:p1", "w1", 11, 11); h.pane("w2:p1", "w3", pid, pid) },
			paneID: "w2:p1", envWS: "w2", want: "w3", wantAsked: []string{"w2:p1"},
		},
		// herdr no longer lists the pane the environment names: every pane
		// is asked.
		"HERDR_PANE_ID gone: every pane is asked": {
			layout: func(h *paneHerdr) { h.pane("w1:p1", "w1", 11, 11); h.pane("w3:p4", "w3", pid, pid) },
			paneID: "w2:p1", envWS: "w2", want: "w3", wantAsked: []string{"w1:p1", "w3:p4"},
		},
		// After a live handoff herdr reissues ids: the one the environment
		// names is another pane now, whose processes lack the pid.
		"HERDR_PANE_ID reissued to another pane is not trusted": {
			layout: func(h *paneHerdr) { h.pane("w2:p1", "w2", 11, 11); h.pane("w5:p2", "w5", pid, pid) },
			paneID: "w2:p1", envWS: "w2", want: "w5", wantAsked: []string{"w2:p1", "w5:p2"},
		},
		"no HERDR_PANE_ID": {
			layout: func(h *paneHerdr) { h.pane("w1:p1", "w1", 11, 11); h.pane("w2:p1", "w2", pid, pid) },
			want:   "w2", wantAsked: []string{"w1:p1", "w2:p1"},
		},
		"a pane that closed after the list is passed over": {
			layout: func(h *paneHerdr) {
				h.pane("w1:p1", "w1", 11, 11)
				h.pane("w2:p1", "w2", pid, pid)
				delete(h.procs, "w1:p1")
			},
			want: "w2", wantAsked: []string{"w1:p1", "w2:p1"},
		},
		// No pane runs it: no place, and no fallback to the environment.
		"no pane runs it": {
			layout: func(h *paneHerdr) { h.pane("w1:p1", "w1", 11, 11) },
			paneID: "w1:p1", envWS: "w1", wantNoPlaceLine: true, wantAsked: []string{"w1:p1"},
		},
		"no panes at all": {
			layout: func(*paneHerdr) {}, paneID: "w1:p1", envWS: "w1", wantNoPlaceLine: true,
		},
		// Two panes claiming the pid when the first asked does not is no
		// answer to trust.
		"two other panes run it": {
			layout: func(h *paneHerdr) {
				h.pane("w1:p1", "w1", 11, 11)
				h.pane("w2:p1", "w2", pid, pid)
				h.pane("w3:p1", "w3", 33, pid)
			},
			paneID: "w1:p1", wantNoPlaceLine: true, wantAsked: []string{"w1:p1", "w2:p1", "w3:p1"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPaneHerdr(t)
			tc.layout(h)
			p, err := h.locator(tc.paneID, tc.envWS).Locate(t.Context())
			lists, asked := h.asked()
			assert.Equal(t, 1, lists)
			assert.Equal(t, tc.wantAsked, asked)
			if tc.wantNoPlaceLine {
				require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
				assert.Equal(t, "this dashboard runs in no herdr pane; pass --project", err.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, p.WorkspaceID)
			assert.Empty(t, p.Note, "herdr confirmed the place")
		})
	}
}

// TestLocatorUnknownPids: herdr reports a pid it does not know as null,
// which reads as 0, and its pids are 32 bits; a dashboard pid that is no
// process's, or that only a narrowing would make equal, matches no pane.
func TestLocatorUnknownPids(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		dashboard int
		pane      int // the pane's shell, job and foreground process
	}{
		"no pid, a pane herdr knows no pids of": {dashboard: 0, pane: 0},
		"a negative pid, a pane at the top pid": {dashboard: -1, pane: 1<<32 - 1},
		"a pid past 32 bits, a pane at its low": {dashboard: 1<<32 + 11, pane: 11},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPaneHerdr(t)
			h.pane("w2:p1", "w2", tc.pane, tc.pane, tc.pane)
			l := h.locator("w2:p1", "w2")
			l.PID = tc.dashboard
			_, err := l.Locate(t.Context())
			require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
		})
	}
}

// TestLocatorPaneIDIsExact: HERDR_PANE_ID is a first guess only when it is
// byte for byte a pane herdr lists; any other spelling is a pane herdr no
// longer lists, and every pane is asked.
func TestLocatorPaneIDIsExact(t *testing.T) {
	t.Parallel()
	for name, paneID := range map[string]string{
		"another case":     "W2:P1",
		"a trailing space": "w2:p1 ",
		"a newline":        "w2:p1\n",
		"invalid UTF-8":    "w2:p1\xff",
		"non-ASCII":        "w2:p1é",
		"option-like":      "--w2:p1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPaneHerdr(t)
			h.pane("w1:p1", "w1", 11, 11)
			h.pane("w2:p1", "w2", pid, pid)
			p, err := h.locator(paneID, "").Locate(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "w2", p.WorkspaceID)
			_, asked := h.asked()
			assert.Equal(t, []string{"w1:p1", "w2:p1"}, asked, "not the fast path")
		})
	}
}

// TestLocatorIsSafeConcurrently: one Locator asked from many goroutines
// finds the same pane each time (run under -race).
func TestLocatorIsSafeConcurrently(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w1:p1", "w1", 11, 11)
	h.pane("w2:p1", "w2", pid, pid)
	l := h.locator("", "")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			p, err := l.Locate(t.Context())
			assert.NoError(t, err)
			assert.Equal(t, "w2", p.WorkspaceID)
		})
	}
	wg.Wait()
}

// TestLocatorAsksTheLastPaneFirst: once found, the dashboard's pane is the
// first asked next time, whatever HERDR_PANE_ID says, so a settled dashboard
// costs herdr two calls a search; a pane that stopped running it is not.
func TestLocatorAsksTheLastPaneFirst(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w1:p1", "w1", 11, 11)
	h.pane("w2:p1", "w2", pid, pid)
	l := h.locator("w9:p9", "")
	_, err := l.Locate(t.Context())
	require.NoError(t, err)
	h.move("w2:p1", "w7")
	p, err := l.Locate(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "w7", p.WorkspaceID)
	lists, asked := h.asked()
	assert.Equal(t, 2, lists)
	assert.Equal(t, []string{"w1:p1", "w2:p1", "w2:p1"}, asked)
}

// TestLocatorWithoutHerdr: when herdr cannot be asked the place is
// HERDR_WORKSPACE_ID's, with a note saying so; without it there is none.
func TestLocatorWithoutHerdr(t *testing.T) {
	t.Parallel()
	gone := filepath.Join(shortDir(t), "gone.sock")

	l := &dashboard.Locator{Herdr: herdr.Client{SocketPath: gone}, PID: pid, PaneID: "w2:p1", WorkspaceID: "w2"}
	p, err := l.Locate(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "w2", p.WorkspaceID)
	assert.Equal(t, "herdr not answering: the team of HERDR_WORKSPACE_ID (w2)", p.Note)

	l = &dashboard.Locator{Herdr: herdr.Client{SocketPath: gone}, PID: pid, PaneID: "w2:p1"}
	_, err = l.Locate(t.Context())
	require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
	require.ErrorIs(t, err, herdr.ErrUnavailable)
	assert.Contains(t, err.Error(), "HERDR_WORKSPACE_ID is unset; pass --project")

	// herdr wedged after its pane list: a pane's process info never comes.
	srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
		if r.Method == "pane.list" {
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []map[string]any{{"pane_id": "w2:p1", "workspace_id": "w2", "focused": false}}}}
		}
		return herdrtest.Reply{Silent: true}
	})
	l = &dashboard.Locator{Herdr: herdr.Client{SocketPath: srv.Path, Timeout: 200 * time.Millisecond}, PID: pid, WorkspaceID: "w3"}
	p, err = l.Locate(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "w3", p.WorkspaceID)
	assert.NotEmpty(t, p.Note)
}

// fakeTeams is a daemon socket answering the snapshot op: the teams it knows
// by project and by workspace, as the daemon's index would.
type fakeTeams struct {
	mu        sync.Mutex
	teamOf    map[string]string // workspace -> project
	projects  map[string]bool
	selectors []daemon.Selector
	sock      string
}

func newFakeTeams(t *testing.T, teamOf map[string]string) *fakeTeams {
	t.Helper()
	f := &fakeTeams{teamOf: teamOf, projects: map[string]bool{}, sock: filepath.Join(shortDir(t), "d.sock")}
	for _, p := range teamOf {
		f.projects[p] = true
	}
	ln, err := net.Listen("unix", f.sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			_, _ = conn.Write([]byte(f.answer(line)))
			_ = conn.Close()
		}
	}()
	return f
}

func (f *fakeTeams) answer(line []byte) string {
	var req struct {
		Args daemon.Selector `json:"args"`
	}
	_ = json.Unmarshal(line, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectors = append(f.selectors, req.Args)
	team, ok := req.Args.Project, f.projects[req.Args.Project]
	if req.Args.Project == "" {
		team, ok = f.teamOf[req.Args.WorkspaceID]
	}
	if !ok {
		return `{"ok":false,"error":{"code":"no_team","message":"no such team"}}` + "\n"
	}
	return fmt.Sprintf(`{"ok":true,"data":{"v":1,"at":"2026-10-10T09:30:00Z","team":%q,"groups":[]}}`+"\n", team)
}

// reindex replaces which team each workspace belongs to.
func (f *fakeTeams) reindex(teamOf map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teamOf, f.projects = teamOf, map[string]bool{}
	for _, p := range teamOf {
		f.projects[p] = true
	}
}

func (f *fakeTeams) asked() []daemon.Selector {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.selectors)
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestTeamSourceShowsItsWorkspacesTeam: the dashboard asks the daemon for
// the team of the workspace it found itself in, and the daemon maps it; it
// finds itself on the first poll and every ten seconds after, so the polls
// between never call herdr.
func TestTeamSourceShowsItsWorkspacesTeam(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w1:p1", "w1", 11, 11)
	h.pane("w2:p1", "w2", pid, pid)
	d := newFakeTeams(t, map[string]string{"w1": "alpha", "w2": "beta", "w3": "gamma"})
	c := &fakeClock{now: at}
	src := &dashboard.TeamSource{Socket: d.sock, Locator: h.locator("w2:p1", "w2"), Now: c.Now}

	for range 10 {
		s, err := src.Snapshot(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "beta", s.Team)
		c.advance(time.Second - time.Nanosecond)
	}
	lists, _ := h.asked()
	assert.Equal(t, 1, lists, "ten polls inside ten seconds find the place once")

	h.move("w2:p1", "w3")
	c.advance(time.Second)
	s, err := src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "gamma", s.Team, "found again after ten seconds, in the workspace it was moved to")
	lists, asked := h.asked()
	assert.Equal(t, 2, lists)
	assert.Equal(t, []string{"w2:p1", "w2:p1"}, asked)
	assert.Empty(t, src.Note())
	for _, sel := range d.asked() {
		assert.Empty(t, sel.Project, "the dashboard names a workspace, never a team")
	}
}

// TestTeamSourceIsSafeConcurrently: the dashboard's polls never overlap, but
// a TeamSource asked from many goroutines still serves its one team and
// searches once inside RelocateEvery (run under -race).
func TestTeamSourceIsSafeConcurrently(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w2:p1", "w2", pid, pid)
	d := newFakeTeams(t, map[string]string{"w2": "beta"})
	src := &dashboard.TeamSource{Socket: d.sock, Locator: h.locator("w2:p1", ""), Now: (&fakeClock{now: at}).Now}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s, err := src.Snapshot(t.Context())
			assert.NoError(t, err)
			assert.Equal(t, "beta", s.Team)
		})
	}
	wg.Wait()
	lists, _ := h.asked()
	assert.Equal(t, 1, lists)
}

// TestTeamSourceOutsideEveryTeam: in a workspace no team holds there is no
// board, and the footer says so, with no fallback to another team. A
// no_team answer for a place not searched since its team was last served
// searches at once: after a live handoff herdr reissues ids, and the
// workspace found before is gone. One that stays outside every team is
// searched again only every ten seconds.
func TestTeamSourceOutsideEveryTeam(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w2:p1", "w2", pid, pid)
	d := newFakeTeams(t, map[string]string{"w1": "alpha", "w2": "beta"})
	c := &fakeClock{now: at}
	src := &dashboard.TeamSource{Socket: d.sock, Locator: h.locator("w2:p1", ""), Now: c.Now}
	s, err := src.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "beta", s.Team)

	d.reindex(map[string]string{"w1": "alpha", "w5": "beta"})
	h.move("w2:p1", "w5")
	c.advance(time.Second)
	s, err = src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "beta", s.Team, "w2 is no team's now: searched at once, and found in w5")
	lists, _ := h.asked()
	assert.Equal(t, 2, lists)

	h.move("w2:p1", "w9")
	c.advance(10 * time.Second)
	_, err = src.Snapshot(t.Context())
	require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
	require.ErrorIs(t, err, daemon.ErrNoTeam)
	assert.Equal(t, "this workspace (w9) is not a team's; pass --project", err.Error())
	lists, _ = h.asked()
	assert.Equal(t, 3, lists, "moved into w9: found at the next search, and not searched again at once")

	for range 9 {
		c.advance(time.Second)
		_, err = src.Snapshot(t.Context())
		require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
	}
	lists, _ = h.asked()
	assert.Equal(t, 3, lists, "a place outside every team is not searched every poll")

	h.move("w2:p1", "w1")
	c.advance(time.Second)
	s, err = src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "alpha", s.Team, "the next search, ten seconds on, finds the team")
}

// TestTeamSourceInNoPane: no pane runs the dashboard: no board, and the
// footer says so until a search, every ten seconds, finds one.
func TestTeamSourceInNoPane(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w2:p1", "w2", 11, 11)
	d := newFakeTeams(t, map[string]string{"w2": "beta"})
	c := &fakeClock{now: at}
	src := &dashboard.TeamSource{Socket: d.sock, Locator: h.locator("w2:p1", "w2"), Now: c.Now}

	for range 3 {
		_, err := src.Snapshot(t.Context())
		require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
		assert.Equal(t, "this dashboard runs in no herdr pane; pass --project", err.Error())
		c.advance(time.Second)
	}
	lists, _ := h.asked()
	assert.Equal(t, 1, lists)
	assert.Empty(t, d.asked(), "with no place the daemon is not asked")

	h.pane("w3:p1", "w2", pid, pid)
	c.advance(10 * time.Second)
	s, err := src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "beta", s.Team)
}

// TestTeamSourceSearchesAtOnceAndFindsNoPane: a no_team that searches at
// once, and finds no pane running the dashboard any more, is that footer; the
// next search is due Every after it.
func TestTeamSourceSearchesAtOnceAndFindsNoPane(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w2:p1", "w2", pid, pid)
	d := newFakeTeams(t, map[string]string{"w2": "beta"})
	c := &fakeClock{now: at}
	src := &dashboard.TeamSource{Socket: d.sock, Locator: h.locator("w2:p1", ""), Now: c.Now, Every: 3 * time.Second}
	_, err := src.Snapshot(t.Context())
	require.NoError(t, err)

	d.reindex(map[string]string{})
	h.mu.Lock()
	delete(h.procs, "w2:p1")
	h.mu.Unlock()
	c.advance(time.Second)
	_, err = src.Snapshot(t.Context())
	require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
	assert.Equal(t, "this dashboard runs in no herdr pane; pass --project", err.Error())

	h.pane("w3:p1", "w3", pid, pid)
	d.reindex(map[string]string{"w3": "gamma"})
	c.advance(2 * time.Second)
	_, err = src.Snapshot(t.Context())
	require.ErrorIs(t, err, dashboard.ErrNoTeamHere, "the next search is three seconds after the last")
	c.advance(time.Second)
	s, err := src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "gamma", s.Team)
}

// TestTeamSourceEveryDefaults: an Every of zero or less is RelocateEvery,
// due at exactly ten seconds and not a nanosecond before.
func TestTeamSourceEveryDefaults(t *testing.T) {
	t.Parallel()
	for name, every := range map[string]time.Duration{"zero": 0, "negative": -time.Second} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPaneHerdr(t)
			h.pane("w2:p1", "w2", pid, pid)
			d := newFakeTeams(t, map[string]string{"w2": "beta"})
			c := &fakeClock{now: at}
			src := &dashboard.TeamSource{Socket: d.sock, Locator: h.locator("w2:p1", ""), Now: c.Now, Every: every}
			for i, step := range []struct {
				by    time.Duration
				lists int
			}{{0, 1}, {dashboard.RelocateEvery - time.Nanosecond, 1}, {time.Nanosecond, 2}} {
				c.advance(step.by)
				_, err := src.Snapshot(t.Context())
				require.NoError(t, err)
				lists, _ := h.asked()
				assert.Equal(t, step.lists, lists, "poll %d: found at the start and again at exactly RelocateEvery", i)
			}
		})
	}
}

// TestTeamSourceForAProject: --project shows that team, asked by name, and
// never asks herdr where the dashboard is; one herdr shows no team of is no
// board.
func TestTeamSourceForAProject(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w2:p1", "w2", pid, pid)
	d := newFakeTeams(t, map[string]string{"w1": "alpha", "w2": "beta"})

	src := &dashboard.TeamSource{Socket: d.sock, Project: "alpha"}
	s, err := src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "alpha", s.Team)
	assert.Equal(t, []daemon.Selector{{Project: "alpha"}}, d.asked())

	src = &dashboard.TeamSource{Socket: d.sock, Project: "delta"}
	_, err = src.Snapshot(t.Context())
	require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
	assert.Equal(t, "herdr shows no team delta", err.Error())
	assert.Empty(t, h.srv.Requests())
}

// TestTeamSourceWithoutHerdr: herdr not answering, the team is
// HERDR_WORKSPACE_ID's, and the note says the place is a guess.
func TestTeamSourceWithoutHerdr(t *testing.T) {
	t.Parallel()
	d := newFakeTeams(t, map[string]string{"w2": "beta"})
	l := &dashboard.Locator{Herdr: herdr.Client{SocketPath: filepath.Join(shortDir(t), "gone.sock")}, PID: pid, WorkspaceID: "w2"}
	src := &dashboard.TeamSource{Socket: d.sock, Locator: l}

	s, err := src.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "beta", s.Team)
	assert.Equal(t, "herdr not answering: the team of HERDR_WORKSPACE_ID (w2)", src.Note())
}

// TestTeamSourceKeepsTheDaemonsLines: a daemon that is not running reads as
// it always has; it is not a place with no team.
func TestTeamSourceKeepsTheDaemonsLines(t *testing.T) {
	t.Parallel()
	h := newPaneHerdr(t)
	h.pane("w2:p1", "w2", pid, pid)
	src := &dashboard.TeamSource{Socket: filepath.Join(shortDir(t), "none.sock"), Locator: h.locator("w2:p1", "")}
	_, err := src.Snapshot(t.Context())
	require.ErrorIs(t, err, daemon.ErrUnavailable)
	assert.NotErrorIs(t, err, dashboard.ErrNoTeamHere)
	assert.Contains(t, err.Error(), "daemon not running: ")
}

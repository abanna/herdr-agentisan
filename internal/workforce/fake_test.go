package workforce_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// fakeHerdr is a herdr server with state: workspaces, tabs, panes and the
// agents named in them, answering the lists the workforce reads the way
// herdr 0.9.3 does. A knob makes a method fail.
type fakeHerdr struct {
	t   *testing.T
	srv *herdrtest.Server

	mu         sync.Mutex
	workspaces []*fakeWS
	tabs       []*fakeTab
	panes      []*fakePane
	next       int
	// fail answers a method with an error code.
	fail map[string]string
}

type fakeWS struct{ id, label string }

type fakeTab struct{ id, ws, label string }

type fakePane struct {
	id, ws, tab, terminal string
	agent, name, status   string
	tokens                map[string]string
}

func newFake(t *testing.T) *fakeHerdr {
	t.Helper()
	f := &fakeHerdr{t: t, fail: map[string]string{}}
	f.srv = herdrtest.Start(t, f.handle)
	return f
}

// client dials the fake.
func (f *fakeHerdr) client() herdr.Client { return herdr.Client{SocketPath: f.srv.Path} }

// addWorkspace adds a workspace and returns its id.
func (f *fakeHerdr) addWorkspace(label string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("w%d", len(f.workspaces)+10)
	f.workspaces = append(f.workspaces, &fakeWS{id: id, label: label})
	return id
}

// addTab adds a tab with one pane running agent ("" for a shell) and
// returns the pane's id.
func (f *fakeHerdr) addTab(ws, label, agent, status string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 1
	for _, t := range f.tabs {
		if t.ws == ws {
			n++
		}
	}
	tab := &fakeTab{id: fmt.Sprintf("%s:t%d", ws, n), ws: ws, label: label}
	f.tabs = append(f.tabs, tab)
	f.next++
	p := &fakePane{id: fmt.Sprintf("%s:p%d", ws, f.next), ws: ws, tab: tab.id, terminal: fmt.Sprintf("term%d", f.next), agent: agent, status: status}
	if agent != "" {
		p.name = label
	}
	f.panes = append(f.panes, p)
	return p.id
}

// addPane adds a pane running agent ("" for a shell) to the tab of pane
// beside, and returns its id.
func (f *fakeHerdr) addPane(beside, agent, status string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.pane(beside)
	f.next++
	p := &fakePane{id: fmt.Sprintf("%s:p%d", b.ws, f.next), ws: b.ws, tab: b.tab, terminal: fmt.Sprintf("term%d", f.next), agent: agent, status: status}
	f.panes = append(f.panes, p)
	return p.id
}

// setPane changes a pane under the lock.
func (f *fakeHerdr) setPane(id string, change func(*fakePane)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.pane(id))
}

func (f *fakeHerdr) pane(id string) *fakePane {
	for _, p := range f.panes {
		if p.id == id {
			return p
		}
	}
	return nil
}

func errReply(code string) herdrtest.Reply {
	return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: code}}
}

func (f *fakeHerdr) paneInfo(p *fakePane) map[string]any {
	out := map[string]any{
		"pane_id": p.id, "terminal_id": p.terminal, "workspace_id": p.ws, "tab_id": p.tab,
		"focused": false, "agent_status": p.status, "revision": 1, "tokens": p.tokens,
	}
	if p.agent != "" {
		out["agent"] = p.agent
	}
	return out
}

func (f *fakeHerdr) handle(r herdrtest.Request) herdrtest.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code, ok := f.fail[r.Method]; ok {
		return errReply(code)
	}
	switch r.Method {
	case "pane.list":
		out := []map[string]any{}
		for _, p := range f.panes {
			out = append(out, f.paneInfo(p))
		}
		return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": out}}
	case "workspace.list":
		out := []map[string]any{}
		for _, w := range f.workspaces {
			out = append(out, map[string]any{"workspace_id": w.id, "label": w.label})
		}
		return herdrtest.Reply{Result: map[string]any{"type": "workspace_list", "workspaces": out}}
	case "tab.list":
		out := []map[string]any{}
		for _, t := range f.tabs {
			out = append(out, map[string]any{"tab_id": t.id, "workspace_id": t.ws, "label": t.label})
		}
		return herdrtest.Reply{Result: map[string]any{"type": "tab_list", "tabs": out}}
	case "agent.list":
		out := []map[string]any{}
		for _, p := range f.panes {
			if p.name != "" || p.agent != "" {
				a := f.paneInfo(p)
				a["name"], a["launch_pending"], a["interactive_ready"] = p.name, false, p.agent != ""
				out = append(out, a)
			}
		}
		return herdrtest.Reply{Result: map[string]any{"type": "agent_list", "agents": out}}
	}
	return errReply("unknown_method")
}

// fixture is a workforce service over a fake herdr, a fresh state.db and a
// configuration with one tabs team, herdr-agentisan (short ha), whose
// workspace holds the boss's tab.
type fixture struct {
	h    *fakeHerdr
	db   string
	svc  *workforce.Service
	ws   string
	boss string
}

// teamConfig is the default configuration: one tabs team with three groups.
const teamConfig = `schema_version = 1
[projects.herdr-agentisan]
repo = "REPO"
short = "ha"
layout = "tabs"

[projects.herdr-agentisan.groups.coder]
workers = 2
slots = 3

[projects.herdr-agentisan.groups.qa]
workers = 1
slots = 1

[projects.herdr-agentisan.groups.review]
workers = 0
slots = 1
`

func newFixture(t *testing.T, config string) *fixture {
	t.Helper()
	repo, dir := t.TempDir(), t.TempDir()
	marker := filepath.Join(repo, filepath.FromSlash(settings.BundleMarker))
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o750))
	require.NoError(t, os.WriteFile(marker, []byte("1\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte(strings.ReplaceAll(config, "REPO", repo)), 0o600))
	db := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	h := newFake(t)
	f := &fixture{h: h, db: db}
	f.ws = h.addWorkspace("herdr-agentisan")
	f.boss = h.addTab(f.ws, "ha-boss", "claude", "working")
	f.svc = &workforce.Service{
		Herdr: h.client(), Store: st, ConfigDir: dir,
		LookupEnv: func(string) (string, bool) { return "", false },
		Idle:      &team.IdleClock{},
	}
	return f
}

// bindRow writes a live workers row binding logicalID, of group, to pane,
// as a spawn records its worker.
func (f *fixture) bindRow(t *testing.T, logicalID, group, pane string) {
	t.Helper()
	raw, err := sql.Open("sqlite", f.db)
	require.NoError(t, err)
	defer raw.Close() //nolint:errcheck // test cleanup
	_, err = raw.ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at) VALUES (?, 'herdr-agentisan', ?, ?, 0, 'active', 0)",
		logicalID, group, pane)
	require.NoError(t, err)
}

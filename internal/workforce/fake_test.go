package workforce_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// fakeHerdr is a herdr server with state: workspaces, tabs, panes and the
// agents named in them, changed by the workforce's calls the way herdr
// 0.9.3 changes them. Knobs make a pane's shell busy, an agent slow, blocked
// or gone, a prompt late, a method fail, or a stale pane id resolve.
type fakeHerdr struct {
	t   *testing.T
	srv *herdrtest.Server

	mu         sync.Mutex
	workspaces []*fakeWS
	tabs       []*fakeTab
	panes      []*fakePane
	next       int
	// busy is how many agent.start calls each new pane answers busy.
	busy int
	// readyAfter is how many agent.get polls each agent takes to be ready.
	readyAfter int
	// startStatus, when set, is the status an agent reaches instead of
	// idle and ready: "blocked", "exited", "unknown" (and ready, as codex
	// is), or "kind:<k>" for another kind.
	startStatus string
	// startTerminal, when set, is the terminal agent.start answers for;
	// getTerminal, the one agent.get answers for once the agent settles.
	startTerminal, getTerminal string
	// promptAfter is how many wait_for_output calls see no prompt; the
	// screen shows "~/repo ❯ " after that.
	promptAfter int
	// screen, when set, is what every pane's screen shows.
	screen string
	// fgProgram is true while a new pane's shell has a program (op's
	// password prompt) at its foreground; it clears when keys are sent,
	// unless stuckFg says the keys do not answer it.
	fgProgram, stuckFg bool
	// fail answers a method with an error code, once failAfter of its calls
	// have been answered.
	fail      map[string]string
	failAfter map[string]int
	calls     map[string]int
	// onCall, when set, runs before each call is answered, under the lock.
	onCall func(method string)
	// keys records each pane.send_keys.
	keys map[string][][]string
	// waits counts wait_for_output calls per pane.
	waits map[string]int
	// badGet answers agent.get with a result the schema does not allow.
	badGet bool
	// alias maps a pane id herdr no longer lists, one frozen in a shell's
	// HERDR_PANE_ID, to the pane pane.process_info resolves it to.
	alias map[string]string
}

type fakeWS struct{ id, label string }

type fakeTab struct{ id, ws, label string }

type fakePane struct {
	id, ws, tab, terminal string
	agent, name, status   string
	tokens                map[string]string
	busy, polls           int
	pending, ready        bool
	kind                  string
	fg                    bool
}

func newFake(t *testing.T) *fakeHerdr {
	t.Helper()
	f := &fakeHerdr{t: t, fail: map[string]string{}, failAfter: map[string]int{}, calls: map[string]int{}, keys: map[string][][]string{}, waits: map[string]int{}, alias: map[string]string{}}
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
	tab := f.newTab(ws, label)
	p := f.newPane(ws, tab.id)
	p.agent, p.status = agent, status
	if agent != "" {
		p.name = label
	}
	return p.id
}

// addPane adds a pane running agent ("" for a shell) to the tab of pane
// beside, and returns its id.
func (f *fakeHerdr) addPane(beside, agent, status string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.pane(beside)
	p := f.newPane(b.ws, b.tab)
	p.agent, p.status = agent, status
	return p.id
}

func (f *fakeHerdr) newTab(ws, label string) *fakeTab {
	n := 1
	for _, t := range f.tabs {
		if t.ws == ws {
			n++
		}
	}
	tab := &fakeTab{id: fmt.Sprintf("%s:t%d", ws, n), ws: ws, label: label}
	f.tabs = append(f.tabs, tab)
	return tab
}

func (f *fakeHerdr) newPane(ws, tab string) *fakePane {
	f.next++
	p := &fakePane{id: fmt.Sprintf("%s:p%d", ws, f.next), ws: ws, tab: tab, terminal: fmt.Sprintf("term%d", f.next), status: "unknown", busy: f.busy, fg: f.fgProgram}
	f.panes = append(f.panes, p)
	return p
}

// setPane changes a pane under the lock.
func (f *fakeHerdr) setPane(id string, change func(*fakePane)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.pane(id))
}

// dropPane removes a pane, as closing it by hand does, under the lock.
func (f *fakeHerdr) dropPane(id string) {
	f.panes = slices.DeleteFunc(f.panes, func(p *fakePane) bool { return p.id == id })
}

func (f *fakeHerdr) pane(id string) *fakePane {
	for _, p := range f.panes {
		if p.id == id {
			return p
		}
	}
	return nil
}

// tabByLabel is the tab labelled label, or nil.
func (f *fakeHerdr) tabByLabel(label string) *fakeTab {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tabs {
		if t.label == label {
			return t
		}
	}
	return nil
}

// methods is every method called, in order.
func (f *fakeHerdr) methods() []string {
	var out []string
	for _, r := range f.srv.Requests() {
		out = append(out, r.Method)
	}
	return out
}

// params is the params of every call to method, decoded.
func (f *fakeHerdr) params(method string) []map[string]any {
	var out []map[string]any
	for _, r := range f.srv.Requests() {
		if r.Method != method {
			continue
		}
		var m map[string]any
		require.NoError(f.t, json.Unmarshal(r.Params, &m))
		out = append(out, m)
	}
	return out
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

func (f *fakeHerdr) agentInfo(p *fakePane) map[string]any {
	a := f.paneInfo(p)
	a["name"] = p.name
	a["launch_pending"] = p.pending
	a["interactive_ready"] = p.ready
	return a
}

// waitTimeout is how long the fake holds a pane.wait_for_output that matched
// nothing before it answers timeout. herdr holds it for the whole timeout_ms;
// the fake holds it a little, so a test stays fast and a loop of waits
// cannot spin.
const waitTimeout = 20 * time.Millisecond

func (f *fakeHerdr) handle(r herdrtest.Request) herdrtest.Reply {
	reply := f.answer(r)
	if r.Method == "pane.wait_for_output" && reply.Error != nil && reply.Error.Code == "timeout" {
		time.Sleep(waitTimeout)
	}
	return reply
}

func (f *fakeHerdr) answer(r herdrtest.Request) herdrtest.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(r.Method)
	}
	f.calls[r.Method]++
	if code, ok := f.fail[r.Method]; ok && f.calls[r.Method] > f.failAfter[r.Method] {
		return errReply(code)
	}
	var p map[string]any
	_ = json.Unmarshal(r.Params, &p)
	str := func(k string) string { s, _ := p[k].(string); return s }
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
				out = append(out, f.agentInfo(p))
			}
		}
		return herdrtest.Reply{Result: map[string]any{"type": "agent_list", "agents": out}}
	case "workspace.create":
		id := fmt.Sprintf("w%d", len(f.workspaces)+10)
		f.workspaces = append(f.workspaces, &fakeWS{id: id, label: str("label")})
		tab := f.newTab(id, "")
		pane := f.newPane(id, tab.id)
		return herdrtest.Reply{Result: map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": id}, "tab": map[string]any{"tab_id": tab.id, "workspace_id": id}, "root_pane": f.paneInfo(pane)}}
	case "tab.create":
		ws := str("workspace_id")
		if !slices.ContainsFunc(f.workspaces, func(w *fakeWS) bool { return w.id == ws }) {
			return errReply("workspace_not_found")
		}
		tab := f.newTab(ws, str("label"))
		pane := f.newPane(ws, tab.id)
		return herdrtest.Reply{Result: map[string]any{"type": "tab_created", "tab": map[string]any{"tab_id": tab.id, "workspace_id": ws}, "root_pane": f.paneInfo(pane)}}
	case "tab.rename":
		for _, t := range f.tabs {
			if t.id == str("tab_id") {
				t.label = str("label")
				return herdrtest.Reply{Result: map[string]any{"type": "tab_info", "tab": map[string]any{"tab_id": t.id, "workspace_id": t.ws}}}
			}
		}
		return errReply("tab_not_found")
	case "agent.start":
		pane := f.pane(str("pane_id"))
		if pane == nil {
			return errReply("agent_pane_not_found")
		}
		for _, other := range f.panes {
			if other.name == str("name") {
				return errReply("agent_name_taken")
			}
		}
		if pane.busy > 0 || pane.fg {
			pane.busy--
			return errReply("agent_pane_busy")
		}
		pane.name, pane.kind, pane.pending, pane.polls = str("name"), str("kind"), true, 0
		agent := f.agentInfo(pane)
		if f.startTerminal != "" {
			agent["terminal_id"] = f.startTerminal
		}
		return herdrtest.Reply{Result: map[string]any{"type": "agent_started", "agent": agent, "argv": []string{}}}
	case "agent.get":
		if f.badGet {
			return herdrtest.Reply{Result: map[string]any{"type": "pane_info"}}
		}
		for _, pane := range f.panes {
			if pane.name != str("target") && pane.id != str("target") {
				continue
			}
			pane.polls++
			if pane.pending && pane.polls > f.readyAfter {
				f.settle(pane)
			}
			agent := f.agentInfo(pane)
			if f.getTerminal != "" && !pane.pending {
				agent["terminal_id"] = f.getTerminal
			}
			return herdrtest.Reply{Result: map[string]any{"type": "agent_info", "agent": agent}}
		}
		return errReply("agent_not_found")
	case "pane.close":
		i := slices.IndexFunc(f.panes, func(x *fakePane) bool { return x.id == str("pane_id") })
		if i < 0 {
			return errReply("pane_not_found")
		}
		closed := f.panes[i]
		f.panes = slices.Delete(f.panes, i, i+1)
		if !slices.ContainsFunc(f.panes, func(x *fakePane) bool { return x.tab == closed.tab }) {
			f.tabs = slices.DeleteFunc(f.tabs, func(t *fakeTab) bool { return t.id == closed.tab })
		}
		if !slices.ContainsFunc(f.panes, func(x *fakePane) bool { return x.ws == closed.ws }) {
			f.workspaces = slices.DeleteFunc(f.workspaces, func(w *fakeWS) bool { return w.id == closed.ws })
		}
		return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
	case "pane.get":
		pane := f.pane(str("pane_id"))
		if pane == nil {
			return errReply("pane_not_found")
		}
		return herdrtest.Reply{Result: map[string]any{"type": "pane_info", "pane": f.paneInfo(pane)}}
	case "pane.send_keys":
		var keys []string
		for _, k := range p["keys"].([]any) {
			keys = append(keys, k.(string))
		}
		f.keys[str("pane_id")] = append(f.keys[str("pane_id")], keys)
		if pane := f.pane(str("pane_id")); pane != nil && !f.stuckFg {
			pane.fg = false
		}
		return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
	case "pane.process_info":
		pane := f.pane(str("pane_id"))
		if real, ok := f.alias[str("pane_id")]; ok {
			pane = f.pane(real)
		}
		if pane == nil {
			return errReply("pane_not_found")
		}
		fg := 100
		if pane.fg {
			fg = 200
		}
		return herdrtest.Reply{Result: map[string]any{"type": "pane_process_info", "process_info": map[string]any{"pane_id": pane.id, "shell_pid": 100, "foreground_process_group_id": fg}}}
	case "pane.wait_for_output":
		id := str("pane_id")
		f.waits[id]++
		match, _ := p["match"].(map[string]any)
		pattern, _ := match["value"].(string)
		screen := "[oh-my-zsh] Would you like to update? [Y/n]"
		if f.waits[id] > f.promptAfter {
			screen = "~/repo ❯ "
		}
		if f.screen != "" {
			screen = f.screen
		}
		if regexp.MustCompile(pattern).MatchString(screen) {
			return herdrtest.Reply{Result: map[string]any{"type": "output_matched", "pane_id": id, "revision": 1, "matched_line": screen, "read": map[string]any{}}}
		}
		return errReply("timeout")
	}
	return errReply("unknown_method")
}

// settle ends an agent's startup as startStatus says.
func (f *fakeHerdr) settle(p *fakePane) {
	p.pending = false
	switch {
	case f.startStatus == "blocked":
		p.agent, p.status = p.kind, "blocked"
	case f.startStatus == "exited":
		p.status = "idle"
	case f.startStatus == "unknown":
		p.agent, p.status, p.ready = p.kind, "unknown", true
	case strings.HasPrefix(f.startStatus, "kind:"):
		p.agent, p.status = strings.TrimPrefix(f.startStatus, "kind:"), "idle"
	case p.kind == "codex":
		p.agent, p.status, p.ready = p.kind, "unknown", true
	default:
		p.agent, p.status, p.ready = p.kind, "idle", true
	}
}

// fixture is a workforce service over a fake herdr, a fresh state.db and a
// configuration with one tabs team, herdr-agentisan (short ha), whose
// workspace holds the boss's tab.
type fixture struct {
	h      *fakeHerdr
	db     string
	st     *store.Store
	svc    *workforce.Service
	repo   string
	ws     string
	boss   string
	config string
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
	f := &fixture{h: h, db: db, st: st, repo: repo, config: dir}
	f.ws = h.addWorkspace("herdr-agentisan")
	f.boss = h.addTab(f.ws, "ha-boss", "claude", "working")
	f.svc = &workforce.Service{
		Herdr: h.client(), Store: st, ConfigDir: dir,
		LookupEnv: func(string) (string, bool) { return "", false },
		Idle:      &team.IdleClock{},
		Preflight: func(context.Context, string) error { return nil },
		ReadyPoll: time.Millisecond, BusyWindow: 2 * time.Second, HandoffPoll: time.Millisecond,
	}
	return f
}

// markLost marks logicalID's row lost, as the team poll does (A35).
func (f *fixture) markLost(t *testing.T, logicalID string) {
	t.Helper()
	f.exec(t, "UPDATE workers SET status = 'lost' WHERE logical_id = ?", logicalID)
}

// moveProject gives logicalID's row to project.
func (f *fixture) moveProject(t *testing.T, logicalID, project string) {
	t.Helper()
	f.exec(t, "UPDATE workers SET project = ? WHERE logical_id = ?", project, logicalID)
}

func (f *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", f.db)
	require.NoError(t, err)
	defer raw.Close() //nolint:errcheck // test cleanup
	_, err = raw.ExecContext(t.Context(), query, args...)
	require.NoError(t, err)
}

// repoFile writes the project repo's committed .herdr-agentisan.toml, body
// after its schema_version.
func (f *fixture) repoFile(t *testing.T, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.repo, settings.RepoFileName), []byte("schema_version = 1\n"+body), 0o600))
}

// bindRow writes a live workers row binding logicalID, of group, to pane and
// its terminal, as a spawn records its worker.
func (f *fixture) bindRow(t *testing.T, logicalID, group, pane string) {
	t.Helper()
	f.h.mu.Lock()
	terminal := ""
	if p := f.h.pane(pane); p != nil {
		terminal = p.terminal
	}
	f.h.mu.Unlock()
	raw, err := sql.Open("sqlite", f.db)
	require.NoError(t, err)
	defer raw.Close() //nolint:errcheck // test cleanup
	_, err = raw.ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, terminal_id) VALUES (?, 'herdr-agentisan', ?, ?, 0, 'active', 0, ?)",
		logicalID, group, pane, terminal)
	require.NoError(t, err)
}

// worker is one worker as a spawn leaves it: its tab in the team's
// workspace, an idle agent and an active row.
type worker struct{ ID, PaneID string }

func (f *fixture) worker(t *testing.T, id, group string) worker {
	t.Helper()
	pane := f.h.addTab(f.ws, id, "claude", "idle")
	f.bindRow(t, id, group, pane)
	return worker{ID: id, PaneID: pane}
}

// row reads one worker's row.
func (f *fixture) row(t *testing.T, id string) store.Row {
	t.Helper()
	r, err := f.st.WorkerRow(t.Context(), id)
	require.NoError(t, err)
	return r
}

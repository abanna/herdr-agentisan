package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// teamHerdr is a fakeHerdr that also answers what the team poll asks: a
// layout of workspaces and panes, and workspace.report_metadata, which it
// refuses for the workspaces in refuse.
type teamHerdr struct {
	*fakeHerdr
	mu         sync.Mutex
	workspaces []herdr.WorkspaceInfo
	panes      []map[string]any
	refuse     map[string]bool
	// onCall, when set, runs before each request is answered.
	onCall func(method string)
}

// newTeamHerdr serves liveSpaces.
func newTeamHerdr(t *testing.T) *teamHerdr {
	t.Helper()
	workspaces, panes := liveSpaces()
	th := &teamHerdr{
		fakeHerdr:  &fakeHerdr{t: t, path: filepath.Join(shortDir(t), "h.sock"), streams: make(chan *stream, 64)},
		workspaces: workspaces, panes: panes, refuse: map[string]bool{},
	}
	th.h = th.serveTeam(th.serve(22))
	th.srv = herdrtest.StartAt(t, th.path, th.h)
	return th
}

func (th *teamHerdr) serveTeam(normal herdrtest.Handler) herdrtest.Handler {
	return func(r herdrtest.Request) herdrtest.Reply {
		th.mu.Lock()
		hook := th.onCall
		th.mu.Unlock()
		if hook != nil {
			hook(r.Method)
		}
		switch r.Method {
		case "pane.list":
			th.mu.Lock()
			defer th.mu.Unlock()
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": th.panes}}
		case "workspace.list":
			th.mu.Lock()
			defer th.mu.Unlock()
			// A copy: the server encodes the reply after the lock is released.
			return herdrtest.Reply{Result: map[string]any{"type": "workspace_list", "workspaces": slices.Clone(th.workspaces)}}
		case "workspace.report_metadata":
			var params struct {
				WorkspaceID string `json:"workspace_id"`
			}
			_ = json.Unmarshal(r.Params, &params)
			th.mu.Lock()
			defer th.mu.Unlock()
			if th.refuse[params.WorkspaceID] {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "workspace_not_found", Message: "workspace " + params.WorkspaceID + " not found"}}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
		}
		return normal(r)
	}
}

func (th *teamHerdr) relabel(id, label string) {
	th.mu.Lock()
	defer th.mu.Unlock()
	for i := range th.workspaces {
		if th.workspaces[i].WorkspaceID == id {
			th.workspaces[i].Label = label
		}
	}
}

// pushes returns the params of every workspace.report_metadata srv got, by
// workspace id, in order.
func pushes(t *testing.T, srv *herdrtest.Server) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, r := range srv.Requests() {
		if r.Method != "workspace.report_metadata" {
			continue
		}
		var params struct {
			WorkspaceID string `json:"workspace_id"`
		}
		require.NoError(t, json.Unmarshal(r.Params, &params))
		out[params.WorkspaceID] = append(out[params.WorkspaceID], string(r.Params))
	}
	return out
}

// lists counts the workspace.list requests srv got: one per poll.
func lists(srv *herdrtest.Server) int {
	n := 0
	for _, r := range srv.Requests() {
		if r.Method == "workspace.list" {
			n++
		}
	}
	return n
}

// polled waits until srv has answered n more polls than it had, so every
// push of the polls before has landed.
func polled(t *testing.T, srv *herdrtest.Server, n int) {
	t.Helper()
	from := lists(srv)
	require.Eventually(t, func() bool { return lists(srv) >= from+n+1 }, ready, 5*time.Millisecond,
		"the daemon did not poll %d more times", n)
}

// teamOpts are opts with the team poll on, reading configDir.
func teamOpts(t *testing.T, stateDir string, th *teamHerdr, configDir string) daemon.Options {
	t.Helper()
	o := opts(t, stateDir, th.fakeHerdr)
	o.Team = &daemon.TeamOptions{ConfigDir: configDir}
	return o
}

// liveSpaces is the live layout of 2026-10-10, trimmed: the boss space, three
// group spaces (one empty), the ignored edit shell, and a plain workspace.
func liveSpaces() ([]herdr.WorkspaceInfo, []map[string]any) {
	ws := []herdr.WorkspaceInfo{
		{WorkspaceID: "wJ", Label: "◆ boss"},
		{WorkspaceID: "wN", Label: "◆ coders"},
		{WorkspaceID: "wY", Label: "◆ qa"},
		{WorkspaceID: "w0", Label: "◆ research"},
		{WorkspaceID: "w12", Label: "◆ herdr"},
		{WorkspaceID: "w2", Label: "notes"},
	}
	pane := func(id, ws, agent, status string, tokens map[string]string) map[string]any {
		p := map[string]any{"pane_id": id, "workspace_id": ws, "agent": agent, "agent_status": status, "focused": false}
		if agent == "" {
			p["agent"] = nil
		}
		if tokens != nil {
			p["tokens"] = tokens
		}
		return p
	}
	panes := []map[string]any{
		pane("wJ:p1", "wJ", "claude", "working", nil),
		pane("wN:p1", "wN", "claude", "working", map[string]string{"ctx": "40"}),
		pane("wN:p2", "wN", "claude", "done", map[string]string{"handoff": "⟳ READY TO RESTART"}),
		pane("wN:p3", "wN", "claude", "blocked", nil),
		pane("wN:p4", "wN", "", "unknown", nil),
		pane("wY:p1", "wY", "codex", "idle", nil),
		pane("w12:p1", "w12", "", "unknown", nil),
		pane("w2:p1", "w2", "claude", "working", nil),
	}
	return ws, panes
}

// wantPushes are the exact params each group space of liveSpaces receives,
// every poll: its own $team, source agentisan, TTL 9000 ms, and no other
// token. Written out by hand, escapes and all, rather than from the
// package's constants.
var wantPushes = map[string]string{
	"wN": `{"workspace_id":"wN","source":"agentisan","tokens":{"team":"4 · ◐1 ●1 ⚠1 ⟳1"},"ttl_ms":9000}`,
	"wY": `{"workspace_id":"wY","source":"agentisan","tokens":{"team":"1 · ◐0 ●1"},"ttl_ms":9000}`,
	"w0": `{"workspace_id":"w0","source":"agentisan","tokens":{"team":"0 · ◐0 ●0"},"ttl_ms":9000}`,
}

// TestRunPushesTeamToEachGroupSpace is D4's $team on the spaces layout: on
// every poll, each ◆ group space receives its exact string as a workspace
// token, source agentisan, TTL 9000 ms. ◆ boss, ◆ herdr and workspaces
// without the prefix receive nothing.
func TestRunPushesTeamToEachGroupSpace(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	dir := shortDir(t)
	run(t, teamOpts(t, dir, th, ""))

	polled(t, th.srv, 2)
	got := pushes(t, th.srv)
	assert.ElementsMatch(t, []string{"wN", "wY", "w0"}, slices.Collect(maps.Keys(got)), "only the group spaces receive $team")
	for ws, params := range got {
		assert.GreaterOrEqual(t, len(params), 2, "%s is pushed on every poll, renewing its TTL", ws)
		for _, p := range params {
			assert.JSONEq(t, wantPushes[ws], p)
		}
	}
}

// TestTeamPushStopsWithTheDaemon: once the daemon has stopped, cancelled or
// because its herdr went away, nothing more is pushed, and nothing was
// cleared on the way out: every push carried a value and the 9 s TTL, so the
// tokens left behind are gone within about 9 s, after a crash just the same.
func TestTeamPushStopsWithTheDaemon(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stop func(th *teamHerdr, r *running)
		want error
	}{
		"the daemon is stopped":         {stop: func(_ *teamHerdr, r *running) { r.cancel() }},
		"herdr removes its socket":      {stop: func(th *teamHerdr, _ *running) { th.stopKeepingRequests() }, want: daemon.ErrHerdrGone},
		"herdr is replaced at its path": {stop: func(th *teamHerdr, _ *running) { th.replace() }, want: daemon.ErrHerdrGone},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			original := th.srv
			r := run(t, teamOpts(t, shortDir(t), th, ""))
			polled(t, original, 1)

			tc.stop(th, r)
			err := r.wait(t, ready)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
			after := pushes(t, original)
			time.Sleep(10 * 20 * time.Millisecond) // ten polls' worth
			assert.Equal(t, after, pushes(t, original), "no push after the daemon stopped")
			for ws, params := range after {
				for _, p := range params {
					assert.JSONEq(t, wantPushes[ws], p, "never an empty or cleared token, always the 9 s TTL")
				}
			}
			if th.srv != original {
				assert.Empty(t, th.srv.Requests(), "the replacement, which the next daemon belongs to, gets nothing")
			}
		})
	}
}

// stopKeepingRequests closes the server, removing its socket, as a herdr that
// exits does.
func (th *teamHerdr) stopKeepingRequests() { th.srv.Close() }

// replace puts a new server, answering the same layout, at the same path.
func (th *teamHerdr) replace() {
	th.srv.Close()
	th.srv = herdrtest.StartAt(th.t, th.path, th.serveTeam(th.serve(22)))
}

// TestTeamPushIgnoresAServerReplacedMidPoll is the check-to-use race on the
// poll, landed deterministically: herdr's socket is replaced while a list is
// being answered, after the poll's identity check. Every call after that
// dials the replacement, which the next daemon belongs to, and finds so once
// connected, before it sends anything: nothing more is asked of the original,
// nothing is pushed to either server, and the replacement never sees a
// request.
func TestTeamPushIgnoresAServerReplacedMidPoll(t *testing.T) {
	t.Parallel()

	for name, during := range map[string]string{
		"while the panes are listed":      "pane.list",
		"while the workspaces are listed": "workspace.list",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := newTeamHerdr(t)
			original := th.srv
			swap, swapped := make(chan struct{}), make(chan struct{})
			var first sync.Once
			th.onCall = func(method string) {
				if method == during {
					first.Do(func() { close(swap) })
					<-swapped // every such call waits, focus's pane.list included
				}
			}
			o := teamOpts(t, shortDir(t), th, "")
			o.Poll = time.Hour // only the poll at start runs
			r := run(t, o)

			select {
			case <-swap:
			case <-time.After(ready):
				t.Fatalf("the daemon did not call %s", during)
			}
			require.NoError(t, os.Remove(th.path))
			replacement := herdrtest.StartAt(t, th.path, th.serveTeam(th.serve(22)))
			close(swapped)

			time.Sleep(time.Second)
			assert.Empty(t, pushes(t, original), "nothing is pushed across the swap")
			if during == "pane.list" {
				assert.Zero(t, lists(original), "workspace.list, dialled after the swap, never reaches the original")
			}
			assert.Empty(t, replacement.Requests(), "the replacement is never asked anything")
			assert.True(t, r.alive(), "the poll stops; Run's own check ends the daemon")
		})
	}
}

// TestTeamPushSurvivesAFailingPush: herdr refusing one push (a workspace
// closed between the list and the push) is logged with the workspace, and
// does not stop the others, in that poll or the next.
func TestTeamPushSurvivesAFailingPush(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	th.refuse["wN"] = true
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, "")
	o.Logger = zerolog.New(&logs)
	r := run(t, o)

	polled(t, th.srv, 2)
	got := pushes(t, th.srv)
	assert.GreaterOrEqual(t, len(got["wN"]), 2, "the refused push is tried every poll")
	assert.GreaterOrEqual(t, len(got["wY"]), 2)
	assert.GreaterOrEqual(t, len(got["w0"]), 2)
	assert.Contains(t, logs.String(), `"workspace":"wN"`)
	assert.Contains(t, logs.String(), "workspace_not_found")
	assert.True(t, r.alive())
}

// TestTeamPushFollowsARelabel: the model is rebuilt every poll, so a space
// renamed away from ◆ stops receiving $team (the TTL clears what it has),
// and a workspace renamed to a group space starts.
func TestTeamPushFollowsARelabel(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	run(t, teamOpts(t, shortDir(t), th, ""))
	polled(t, th.srv, 1)

	th.relabel("wN", "coders")
	th.relabel("w2", "◆ docs")
	polled(t, th.srv, 1) // a poll in flight may have listed the old labels
	before := pushes(t, th.srv)
	polled(t, th.srv, 2)
	after := pushes(t, th.srv)

	assert.Equal(t, before["wN"], after["wN"], "a space renamed away from ◆ is no longer pushed")
	assert.Greater(t, len(after["w2"]), len(before["w2"]), "a workspace renamed into a group space is pushed")
	for _, p := range after["w2"] {
		assert.JSONEq(t, `{"workspace_id":"w2","source":"agentisan","tokens":{"team":"1 · ◐1 ●0"},"ttl_ms":9000}`, p)
	}
}

// TestTeamPushReadsTheConfiguredProject: the daemon reads [spaces] project
// once when it starts. No configuration at all, which is the user's case
// today, is the unnamed project; a file that does not resolve is logged and
// the spaces fall back to the unnamed project, since $team does not depend
// on it and focus and Back must keep working. Pushes go out either way.
func TestTeamPushReadsTheConfiguredProject(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		file     string // the config file; "" for none, "nodir" for no config dir
		want     string
		wantLogs []string
	}{
		"no config dir":       {file: "nodir", want: ""},
		"no config file":      {want: ""},
		"the key":             {file: "[spaces]\nproject = \"b\"\n[projects.a]\nrepo = \"/x\"\n[projects.b]\nrepo = \"/y\"\n", want: "b"},
		"the sole project":    {file: "[projects.a]\nrepo = \"/x\"\n", want: "a"},
		"several, no key":     {file: "[projects.a]\nrepo = \"/x\"\n[projects.b]\nrepo = \"/y\"\n", want: ""},
		"an unknown project":  {file: "[spaces]\nproject = \"c\"\n[projects.a]\nrepo = \"/x\"\n", want: "", wantLogs: []string{`"level":"error"`, "no such project"}},
		"an unsupported file": {file: "-", want: "", wantLogs: []string{`"level":"error"`, "schema_version"}},
		"a non-ASCII project": {file: "[spaces]\nproject = \"ä\"\n[projects.a]\nrepo = \"/x\"\n", want: "", wantLogs: []string{`"level":"error"`}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			switch tc.file {
			case "nodir":
				dir = ""
			case "":
			case "-":
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 2\n"), 0o600))
			default:
				require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+tc.file), 0o600))
			}
			th := newTeamHerdr(t)
			var logs lockedLog
			o := teamOpts(t, shortDir(t), th, dir)
			o.Logger = zerolog.New(&logs)
			run(t, o)

			polled(t, th.srv, 1)
			assert.Len(t, pushes(t, th.srv), 3, "the pushes do not depend on the project")
			assert.Contains(t, logs.String(), fmt.Sprintf(`"project":%q`, tc.want))
			for _, s := range tc.wantLogs {
				assert.Contains(t, logs.String(), s)
			}
		})
	}
}

// lockedLog is a log sink safe to read while the daemon writes it.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p) //nolint:wrapcheck // test sink
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

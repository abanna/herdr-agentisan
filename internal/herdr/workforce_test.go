package herdr_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// rootPaneOf is a root_pane as tab.create and workspace.create answer it.
func rootPaneOf(paneID, ws string) map[string]any {
	return map[string]any{
		"pane_id": paneID, "terminal_id": "term-" + paneID, "workspace_id": ws, "tab_id": ws + ":t2",
		"focused": false, "agent_status": "unknown", "revision": 0,
	}
}

// agentOf is an agent as agent.start and agent.get answer it.
func agentOf(paneID, name, status string, ready bool) map[string]any {
	a := map[string]any{
		"terminal_id": "term-" + paneID, "workspace_id": "w14", "tab_id": "w14:t2", "pane_id": paneID,
		"agent_status": status, "focused": false, "revision": 1,
	}
	if name != "" {
		a["name"] = name
	}
	if ready {
		a["interactive_ready"] = true
	}
	return a
}

// TestWorkforceCallsSendTheSchemaShape: every workforce call sends herdr
// 0.9.3's params exactly, never focusing what it opens, and reads its result.
func TestWorkforceCallsSendTheSchemaShape(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		result     map[string]any
		call       func(herdr.Client) (any, error)
		method     string
		params     string
		want       any
		wantClient time.Duration
	}{
		"tab.create": {
			result: map[string]any{"type": "tab_created", "tab": map[string]any{"tab_id": "w14:t2", "workspace_id": "w14"}, "root_pane": rootPaneOf("w14:p7", "w14")},
			call: func(c herdr.Client) (any, error) {
				return c.CreateTab(t.Context(), herdr.TabCreate{WorkspaceID: "w14", Cwd: "/repo", Label: "ha-coder-2", Env: map[string]string{"HERDR_AGENTISAN_WORKER": "ha-coder-2"}})
			},
			method: "tab.create",
			params: `{"workspace_id":"w14","cwd":"/repo","label":"ha-coder-2","focus":false,"env":{"HERDR_AGENTISAN_WORKER":"ha-coder-2"}}`,
			want:   herdr.Created{WorkspaceID: "w14", TabID: "w14:t2", RootPane: herdr.PaneInfo{PaneID: "w14:p7", TerminalID: "term-w14:p7", WorkspaceID: "w14", TabID: "w14:t2", AgentStatus: "unknown"}},
		},
		"workspace.create": {
			result: map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": "w20"}, "tab": map[string]any{"tab_id": "w20:t1", "workspace_id": "w20"}, "root_pane": rootPaneOf("w20:p1", "w20")},
			call: func(c herdr.Client) (any, error) {
				return c.CreateWorkspace(t.Context(), herdr.WorkspaceCreate{Cwd: "/repo", Label: "assay"})
			},
			method: "workspace.create",
			params: `{"cwd":"/repo","label":"assay","focus":false}`,
			want:   herdr.Created{WorkspaceID: "w20", TabID: "w20:t1", RootPane: herdr.PaneInfo{PaneID: "w20:p1", TerminalID: "term-w20:p1", WorkspaceID: "w20", TabID: "w20:t2", AgentStatus: "unknown"}},
		},
		"tab.rename": {
			result: map[string]any{"type": "tab_info", "tab": map[string]any{"tab_id": "w20:t1", "workspace_id": "w20"}},
			call:   func(c herdr.Client) (any, error) { return nil, c.RenameTab(t.Context(), "w20:t1", "as-coder-1") },
			method: "tab.rename",
			params: `{"tab_id":"w20:t1","label":"as-coder-1"}`,
		},
		"agent.start": {
			result: map[string]any{"type": "agent_started", "agent": agentOf("w14:p7", "ha-coder-2", "unknown", false), "argv": []string{"claude", "--name", "ha-coder-2"}},
			call: func(c herdr.Client) (any, error) {
				return c.StartAgent(t.Context(), herdr.AgentStart{Name: "ha-coder-2", Kind: "claude", PaneID: "w14:p7", Args: []string{"--name", "ha-coder-2"}, Timeout: 90 * time.Second})
			},
			method: "agent.start",
			params: `{"name":"ha-coder-2","kind":"claude","pane_id":"w14:p7","args":["--name","ha-coder-2"],"timeout_ms":90000}`,
			want:   herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-2", AgentStatus: "unknown"},
		},
		"agent.start with no args": {
			result: map[string]any{"type": "agent_started", "agent": agentOf("w14:p7", "ha-review-1", "unknown", false), "argv": []string{"codex"}},
			call: func(c herdr.Client) (any, error) {
				return c.StartAgent(t.Context(), herdr.AgentStart{Name: "ha-review-1", Kind: "codex", PaneID: "w14:p7"})
			},
			method: "agent.start",
			params: `{"name":"ha-review-1","kind":"codex","pane_id":"w14:p7"}`,
			want:   herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-review-1", AgentStatus: "unknown"},
		},
		"agent.get": {
			result: map[string]any{"type": "agent_info", "agent": agentOf("w14:p7", "ha-coder-2", "idle", true)},
			call:   func(c herdr.Client) (any, error) { return c.GetAgent(t.Context(), "ha-coder-2") },
			method: "agent.get",
			params: `{"target":"ha-coder-2"}`,
			want:   herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-2", AgentStatus: "idle", InteractiveReady: true},
		},
		"agent.get, launch pending": {
			result: map[string]any{"type": "agent_info", "agent": func() map[string]any {
				a := agentOf("w14:p7", "ha-coder-2", "idle", false)
				a["launch_pending"], a["agent"] = true, "claude"
				return a
			}()},
			call:   func(c herdr.Client) (any, error) { return c.GetAgent(t.Context(), "w14:p7") },
			method: "agent.get",
			params: `{"target":"w14:p7"}`,
			want:   herdr.Agent{PaneID: "w14:p7", TerminalID: "term-w14:p7", Name: "ha-coder-2", Agent: "claude", AgentStatus: "idle", LaunchPending: true},
		},
		"pane.get": {
			result: map[string]any{"type": "pane_info", "pane": rootPaneOf("w14:p7", "w14")},
			call:   func(c herdr.Client) (any, error) { return c.GetPane(t.Context(), "w14:p7") },
			method: "pane.get",
			params: `{"pane_id":"w14:p7"}`,
			want:   herdr.PaneInfo{PaneID: "w14:p7", TerminalID: "term-w14:p7", WorkspaceID: "w14", TabID: "w14:t2", AgentStatus: "unknown"},
		},
		"pane.close": {
			result: map[string]any{"type": "ok"},
			call:   func(c herdr.Client) (any, error) { return nil, c.ClosePane(t.Context(), "w14:p7") },
			method: "pane.close",
			params: `{"pane_id":"w14:p7"}`,
		},
		"pane.send_keys": {
			result: map[string]any{"type": "ok"},
			call:   func(c herdr.Client) (any, error) { return nil, c.SendKeys(t.Context(), "w14:p7", []string{"enter"}) },
			method: "pane.send_keys",
			params: `{"pane_id":"w14:p7","keys":["enter"]}`,
		},
		"pane.wait_for_output": {
			result: map[string]any{"type": "output_matched", "pane_id": "w14:p7", "revision": 4, "matched_line": "~/repo ❯ ", "read": map[string]any{}},
			call: func(c herdr.Client) (any, error) {
				return c.WaitForOutput(t.Context(), "w14:p7", `❯\s*$`, 2*time.Second)
			},
			method: "pane.wait_for_output",
			params: `{"pane_id":"w14:p7","source":"visible","match":{"type":"regex","value":"❯\\s*$"},"timeout_ms":2000}`,
			want:   "~/repo ❯ ",
		},
		"pane.wait_for_output, no time at all": {
			result: map[string]any{"type": "output_matched", "pane_id": "w14:p7", "revision": 4, "matched_line": "$ ", "read": map[string]any{}},
			call:   func(c herdr.Client) (any, error) { return c.WaitForOutput(t.Context(), "w14:p7", `\$ $`, 0) },
			method: "pane.wait_for_output",
			params: `{"pane_id":"w14:p7","source":"visible","match":{"type":"regex","value":"\\$ $"},"timeout_ms":1}`,
			want:   "$ ",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: tc.result} })

			got, err := tc.call(herdr.Client{SocketPath: srv.Path})
			require.NoError(t, err)
			if tc.want != nil {
				assert.Equal(t, tc.want, got)
			}
			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, tc.method, reqs[0].Method)
			assert.JSONEq(t, tc.params, string(reqs[0].Params))
		})
	}
}

// TestWaitForOutputOutlivesTheCallTimeout: herdr holds pane.wait_for_output
// open until a match or its own timeout, so the client waits that long and a
// little more rather than its usual CallTimeout.
func TestWaitForOutputOutlivesTheCallTimeout(t *testing.T) {
	t.Parallel()
	srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
		time.Sleep(300 * time.Millisecond)
		return herdrtest.Reply{Result: map[string]any{"type": "output_matched", "matched_line": "$ ", "pane_id": "w1:p1", "revision": 1, "read": map[string]any{}}}
	})

	c := herdr.Client{SocketPath: srv.Path, Timeout: 100 * time.Millisecond}
	got, err := c.WaitForOutput(t.Context(), "w1:p1", `\$ $`, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "$ ", got)

	_, err = c.ListPanes(t.Context())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, herdr.ErrUnavailable),
		"the lifted timeout is that call's alone: %v", err)
}

// TestWorkforceCallFailureClasses: herdr's codes the workforce branches on
// match their sentinels and ErrAPI; any other code matches ErrAPI alone; a
// result the schema does not allow is a protocol violation; the transport
// fails as every call does.
func TestWorkforceCallFailureClasses(t *testing.T) {
	t.Parallel()

	reply := func(result map[string]any) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: result} }
	}
	apiErr := func(code string) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: code}}
		}
	}
	silent := func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} }
	ok := map[string]any{"type": "ok"}
	tab := map[string]any{"tab_id": "w14:t2", "workspace_id": "w14"}
	createTab := func(c herdr.Client) error {
		_, err := c.CreateTab(t.Context(), herdr.TabCreate{WorkspaceID: "w14", Label: "ha-coder-1"})
		return err
	}
	createWS := func(c herdr.Client) error {
		_, err := c.CreateWorkspace(t.Context(), herdr.WorkspaceCreate{Label: "assay"})
		return err
	}
	start := func(c herdr.Client) error {
		_, err := c.StartAgent(t.Context(), herdr.AgentStart{Name: "ha-coder-1", Kind: "claude", PaneID: "w14:p7"})
		return err
	}
	get := func(c herdr.Client) error { _, err := c.GetAgent(t.Context(), "ha-coder-1"); return err }
	wait := func(c herdr.Client) error {
		_, err := c.WaitForOutput(t.Context(), "w14:p7", "x", 50*time.Millisecond)
		return err
	}
	closePane := func(c herdr.Client) error { return c.ClosePane(t.Context(), "w14:p7") }
	getPane := func(c herdr.Client) error { _, err := c.GetPane(t.Context(), "w14:p7"); return err }
	keys := func(c herdr.Client) error { return c.SendKeys(t.Context(), "w14:p7", []string{"enter"}) }
	renameTab := func(c herdr.Client) error { return c.RenameTab(t.Context(), "w14:t2", "x") }

	tests := map[string]struct {
		handler herdrtest.Handler
		call    func(herdr.Client) error
		want    []error
		not     []error
	}{
		"tab.create: wrong type":           {handler: reply(map[string]any{"type": "tab_info", "tab": tab}), call: createTab, want: []error{herdr.ErrProtocol}},
		"tab.create: no tab":               {handler: reply(map[string]any{"type": "tab_created", "root_pane": rootPaneOf("w14:p7", "w14")}), call: createTab, want: []error{herdr.ErrProtocol}},
		"tab.create: tab without a ws":     {handler: reply(map[string]any{"type": "tab_created", "tab": map[string]any{"tab_id": "w14:t2"}, "root_pane": rootPaneOf("w14:p7", "w14")}), call: createTab, want: []error{herdr.ErrProtocol}},
		"tab.create: no root pane":         {handler: reply(map[string]any{"type": "tab_created", "tab": tab}), call: createTab, want: []error{herdr.ErrProtocol}},
		"tab.create: root without a term":  {handler: reply(map[string]any{"type": "tab_created", "tab": tab, "root_pane": map[string]any{"pane_id": "w14:p7"}}), call: createTab, want: []error{herdr.ErrProtocol}},
		"tab.create: workspace not found":  {handler: apiErr("workspace_not_found"), call: createTab, want: []error{herdr.ErrAPI}, not: []error{herdr.ErrPaneBusy, herdr.ErrPaneNotFound}},
		"workspace.create: wrong type":     {handler: reply(map[string]any{"type": "tab_created", "tab": tab, "root_pane": rootPaneOf("w14:p7", "w14")}), call: createWS, want: []error{herdr.ErrProtocol}},
		"workspace.create: no root pane":   {handler: reply(map[string]any{"type": "workspace_created", "tab": tab}), call: createWS, want: []error{herdr.ErrProtocol}},
		"tab.rename: wrong type":           {handler: reply(ok), call: renameTab, want: []error{herdr.ErrProtocol}},
		"agent.start: busy":                {handler: apiErr("agent_pane_busy"), call: start, want: []error{herdr.ErrPaneBusy, herdr.ErrAPI}, not: []error{herdr.ErrNameTaken}},
		"agent.start: name taken":          {handler: apiErr("agent_name_taken"), call: start, want: []error{herdr.ErrNameTaken, herdr.ErrAPI}, not: []error{herdr.ErrPaneBusy}},
		"agent.start: invalid name":        {handler: apiErr("invalid_agent_name"), call: start, want: []error{herdr.ErrAPI}, not: []error{herdr.ErrPaneBusy, herdr.ErrNameTaken}},
		"agent.start: wrong type":          {handler: reply(map[string]any{"type": "agent_info", "agent": agentOf("w14:p7", "x", "idle", true)}), call: start, want: []error{herdr.ErrProtocol}},
		"agent.start: no terminal":         {handler: reply(map[string]any{"type": "agent_started", "agent": map[string]any{"pane_id": "w14:p7"}}), call: start, want: []error{herdr.ErrProtocol}},
		"agent.get: not found":             {handler: apiErr("agent_not_found"), call: get, want: []error{herdr.ErrAgentNotFound, herdr.ErrAPI}},
		"agent.get: no agent":              {handler: reply(map[string]any{"type": "agent_info"}), call: get, want: []error{herdr.ErrProtocol}},
		"agent.get: status not a string":   {handler: reply(map[string]any{"type": "agent_info", "agent": map[string]any{"pane_id": "w14:p7", "terminal_id": "t", "agent_status": 3}}), call: get, want: []error{herdr.ErrProtocol}},
		"pane.wait_for_output: timeout":    {handler: apiErr("timeout"), call: wait, want: []error{herdr.ErrWaitTimeout, herdr.ErrAPI}},
		"pane.wait_for_output: bad regex":  {handler: apiErr("invalid_regex"), call: wait, want: []error{herdr.ErrAPI}, not: []error{herdr.ErrWaitTimeout}},
		"pane.wait_for_output: no line":    {handler: reply(map[string]any{"type": "output_matched"}), call: wait, want: []error{herdr.ErrProtocol}},
		"pane.wait_for_output: wrong type": {handler: reply(ok), call: wait, want: []error{herdr.ErrProtocol}},
		"pane.get: pane not found":         {handler: apiErr("pane_not_found"), call: getPane, want: []error{herdr.ErrPaneNotFound, herdr.ErrAPI}},
		"pane.get: wrong type":             {handler: reply(map[string]any{"type": "pane_list", "pane": rootPaneOf("w14:p7", "w14")}), call: getPane, want: []error{herdr.ErrProtocol}},
		"pane.close: pane not found":       {handler: apiErr("pane_not_found"), call: closePane, want: []error{herdr.ErrPaneNotFound, herdr.ErrAPI}},
		"pane.close: wrong type":           {handler: reply(map[string]any{"type": "pane_info"}), call: closePane, want: []error{herdr.ErrProtocol}},
		"pane.send_keys: invalid key":      {handler: apiErr("invalid_key"), call: keys, want: []error{herdr.ErrAPI}},
		"closed without a reply":           {handler: silent, call: closePane, want: []error{herdr.ErrUnavailable}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)

			err := tc.call(herdr.Client{SocketPath: srv.Path})
			for _, want := range tc.want {
				require.ErrorIs(t, err, want)
			}
			for _, not := range tc.not {
				assert.False(t, errors.Is(err, not), "%v must not match %v", err, not)
			}
		})
	}
	_, err := herdr.Client{}.CreateTab(t.Context(), herdr.TabCreate{})
	require.ErrorIs(t, err, herdr.ErrNoSocket)
}

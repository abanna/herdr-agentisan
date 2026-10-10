package herdr_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// TestListWorkspacesSendsTheSchemaShape: workspace.list takes no params and
// answers workspace_list (herdr 0.9.3 src/app/api/workspaces.rs:14-21), each
// workspace a WorkspaceInfo (src/api/schema/workspaces.rs:61-76). Labels come
// back exactly as herdr has them: the resolver, not the client, reads them.
func TestListWorkspacesSendsTheSchemaShape(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		workspaces []map[string]any
		want       []herdr.WorkspaceInfo
	}{
		"several workspaces with the fields herdr sends": {
			workspaces: []map[string]any{
				{
					"workspace_id": "wN", "number": 2, "label": "◆ coders", "focused": true, "pane_count": 9,
					"tab_count": 1, "active_tab_id": "wN:t1", "agent_status": "working", "tokens": map[string]any{"team": "9 · ◐3 ●2"},
				},
				{"workspace_id": "w12", "number": 7, "label": "◆ herdr", "focused": false, "pane_count": 1, "tab_count": 1, "active_tab_id": "w12:t1", "agent_status": "unknown"},
			},
			want: []herdr.WorkspaceInfo{{WorkspaceID: "wN", Label: "◆ coders"}, {WorkspaceID: "w12", Label: "◆ herdr"}},
		},
		"no workspaces": {workspaces: []map[string]any{}, want: []herdr.WorkspaceInfo{}},
		"labels are kept byte for byte": {
			workspaces: []map[string]any{
				{"workspace_id": "w1", "label": "◆  coders "},
				{"workspace_id": "w2", "label": ""},
				{"workspace_id": "w3", "label": "agentisan · qa"},
			},
			want: []herdr.WorkspaceInfo{{WorkspaceID: "w1", Label: "◆  coders "}, {WorkspaceID: "w2"}, {WorkspaceID: "w3", Label: "agentisan · qa"}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "workspace_list", "workspaces": tc.workspaces}}
			})

			got, err := herdr.Client{SocketPath: srv.Path}.ListWorkspaces(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "workspace.list", reqs[0].Method)
			assert.JSONEq(t, `{}`, string(reqs[0].Params))
		})
	}
}

// TestReportWorkspaceMetadataSendsTheSchemaShape: workspace.report_metadata
// takes WorkspaceReportMetadataParams (src/api/schema/workspaces.rs:48-59) and
// answers ok (src/app/api/workspaces.rs:241-309).
func TestReportWorkspaceMetadataSendsTheSchemaShape(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		meta herdr.WorkspaceMetadata
		want string
	}{
		"a token with a TTL": {
			meta: herdr.WorkspaceMetadata{WorkspaceID: "wN", Source: "agentisan", Tokens: map[string]string{"team": "5 · ◐3 ●2"}, TTLMillis: 9000},
			want: `{"workspace_id":"wN","source":"agentisan","tokens":{"team":"5 · ◐3 ●2"},"ttl_ms":9000}`,
		},
		// herdr reads an absent ttl_ms as "never expires" but rejects 0.
		"no TTL omits ttl_ms": {
			meta: herdr.WorkspaceMetadata{WorkspaceID: "wN", Source: "agentisan", Tokens: map[string]string{"team": "0 · ◐0 ●0"}},
			want: `{"workspace_id":"wN","source":"agentisan","tokens":{"team":"0 · ◐0 ●0"}}`,
		},
		"workspace id with invalid UTF-8 is sent as valid JSON": {
			meta: herdr.WorkspaceMetadata{WorkspaceID: "w\xff", Source: "agentisan", Tokens: map[string]string{"team": "1"}},
			want: `{"workspace_id":"w�","source":"agentisan","tokens":{"team":"1"}}`,
		},
		"workspace id with a newline stays one request line": {
			meta: herdr.WorkspaceMetadata{WorkspaceID: "w1\nw2", Source: "agentisan", Tokens: map[string]string{"team": "1"}},
			want: `{"workspace_id":"w1\nw2","source":"agentisan","tokens":{"team":"1"}}`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, okReply)

			require.NoError(t, herdr.Client{SocketPath: srv.Path}.ReportWorkspaceMetadata(t.Context(), tc.meta))

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "workspace.report_metadata", reqs[0].Method)
			assert.JSONEq(t, tc.want, string(reqs[0].Params))
		})
	}
}

// TestWorkspaceCallFailureClasses: both calls fail the way every call does,
// and an answer missing what the schema requires is a protocol violation
// rather than a silently empty one.
func TestWorkspaceCallFailureClasses(t *testing.T) {
	t.Parallel()

	reply := func(result map[string]any) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: result} }
	}
	silent := func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} }
	list := func(c herdr.Client) error { _, err := c.ListWorkspaces(t.Context()); return err }
	panes := func(c herdr.Client) error { _, err := c.ListPanes(t.Context()); return err }
	pane := func(fields map[string]any) herdrtest.Handler {
		fields["pane_id"] = "w1:p1"
		return reply(map[string]any{"type": "pane_list", "panes": []map[string]any{fields}})
	}
	report := func(c herdr.Client) error {
		return c.ReportWorkspaceMetadata(t.Context(), herdr.WorkspaceMetadata{WorkspaceID: "w9", Source: "agentisan", Tokens: map[string]string{"team": "1"}})
	}

	tests := map[string]struct {
		handler herdrtest.Handler
		call    func(herdr.Client) error
		want    error
	}{
		"list: wrong result type":             {handler: reply(map[string]any{"type": "pane_list", "panes": []any{}}), call: list, want: herdr.ErrProtocol},
		"list: workspaces missing":            {handler: reply(map[string]any{"type": "workspace_list"}), call: list, want: herdr.ErrProtocol},
		"list: workspaces is not an array":    {handler: reply(map[string]any{"type": "workspace_list", "workspaces": "w1"}), call: list, want: herdr.ErrProtocol},
		"list: a workspace without an id":     {handler: reply(map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{"label": "◆ qa"}}}), call: list, want: herdr.ErrProtocol},
		"list: a label that is not a string":  {handler: reply(map[string]any{"type": "workspace_list", "workspaces": []map[string]any{{"workspace_id": "w1", "label": 7}}}), call: list, want: herdr.ErrProtocol},
		"list: closed without a reply":        {handler: silent, call: list, want: herdr.ErrUnavailable},
		"panes: agent_status is not a string": {handler: pane(map[string]any{"agent_status": 3}), call: panes, want: herdr.ErrProtocol},
		"panes: a token that is not a string": {handler: pane(map[string]any{"tokens": map[string]any{"handoff": true}}), call: panes, want: herdr.ErrProtocol},
		"panes: tokens is not a map":          {handler: pane(map[string]any{"tokens": []any{"handoff"}}), call: panes, want: herdr.ErrProtocol},
		"report: workspace not found": {handler: func(herdrtest.Request) herdrtest.Reply {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "workspace_not_found", Message: "workspace w9 not found"}}
		}, call: report, want: herdr.ErrAPI},
		"report: wrong result type":      {handler: reply(map[string]any{"type": "pong"}), call: report, want: herdr.ErrProtocol},
		"report: closed without a reply": {handler: silent, call: report, want: herdr.ErrUnavailable},
		"report: no socket":              {call: report, want: herdr.ErrNoSocket},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := herdr.Client{}
			if tc.handler != nil {
				client.SocketPath = herdrtest.Start(t, tc.handler).Path
			}
			require.ErrorIs(t, tc.call(client), tc.want)
		})
	}
}

// TestListPanesReadsAgentStatusAndTokens: pane.list carries what the team
// model counts (src/api/schema/panes.rs:449-484): the agent, its status and
// the pane's tokens. A plain shell has no agent and status unknown; a pane
// with no tokens leaves the key out, which decodes to nil.
func TestListPanesReadsAgentStatusAndTokens(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pane map[string]any
		want herdr.PaneInfo
	}{
		"an agent with tokens": {
			pane: map[string]any{
				"pane_id": "wN:p2", "workspace_id": "wN", "agent": "claude", "agent_status": "done", "revision": 4,
				"tokens": map[string]any{"ctx": "16", "item": "GH-892", "handoff": "⟳ READY TO RESTART"},
			},
			want: herdr.PaneInfo{
				PaneID: "wN:p2", WorkspaceID: "wN", Agent: "claude", AgentStatus: "done",
				Tokens: map[string]string{"ctx": "16", "item": "GH-892", "handoff": "⟳ READY TO RESTART"},
			},
		},
		"a plain shell": {
			pane: map[string]any{"pane_id": "wN:p3", "workspace_id": "wN", "agent": nil, "agent_status": "unknown"},
			want: herdr.PaneInfo{PaneID: "wN:p3", WorkspaceID: "wN", AgentStatus: "unknown"},
		},
		// A status this build does not know is kept as herdr sent it; the
		// model decides what it counts as.
		"a status from a newer herdr": {
			pane: map[string]any{"pane_id": "w1:p1", "workspace_id": "w1", "agent_status": "napping"},
			want: herdr.PaneInfo{PaneID: "w1:p1", WorkspaceID: "w1", AgentStatus: "napping"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []map[string]any{tc.pane}}}
			})

			got, err := herdr.Client{SocketPath: srv.Path}.ListPanes(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []herdr.PaneInfo{tc.want}, got)
		})
	}
}

// errReplaced stands in for a Dialed hook's verdict that the socket now
// names another server.
var errReplaced = errors.New("the socket names another server")

// TestDialedRunsBeforeAnythingIsSent: the Dialed hook runs once per
// connection, after the dial and before the request is written, so a call it
// refuses sends nothing: herdr never sees a request line.
func TestDialedRunsBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()

	report := func(c herdr.Client) error {
		return c.ReportWorkspaceMetadata(t.Context(), herdr.WorkspaceMetadata{WorkspaceID: "w1", Source: "agentisan", Tokens: map[string]string{"team": "1"}})
	}
	subscribe := func(c herdr.Client) error {
		s, err := c.Subscribe(t.Context(), herdr.SubscribePaneFocused)
		if err == nil {
			_ = s.Close()
		}
		return err
	}
	tests := map[string]struct {
		call    func(herdr.Client) error
		verdict error
		sent    bool
	}{
		"a call the hook allows":         {call: report, sent: true},
		"a call the hook refuses":        {call: report, verdict: errReplaced},
		"a subscription the hook allows": {call: subscribe, sent: true},
		"a subscription it refuses":      {call: subscribe, verdict: errReplaced},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
				if r.Method == "events.subscribe" {
					return herdrtest.Reply{Result: herdrtest.SubscriptionStarted()}
				}
				return okReply(r)
			})
			var calls atomic.Int32
			c := herdr.Client{SocketPath: srv.Path, Dialed: func() error {
				calls.Add(1)
				return tc.verdict
			}}

			err := tc.call(c)
			assert.EqualValues(t, 1, calls.Load(), "the hook runs once per connection")
			if tc.verdict != nil {
				require.ErrorIs(t, err, tc.verdict)
				assert.Empty(t, srv.Requests(), "nothing reaches herdr")
				return
			}
			require.NoError(t, err)
			assert.Len(t, srv.Requests(), 1)
		})
	}
}

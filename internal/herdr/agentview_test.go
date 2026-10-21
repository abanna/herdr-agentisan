// safety-scan: allow (test fixture: herdr's agent view sorts by a field named token, so `"token":"order_ha"` is the wire shape; the values are token names, not secrets)
package herdr_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// viewReply answers as herdr does once it has taken or cleared a view
// (src/app/api/agent_view.rs:33-40, :64-71).
func viewReply(active bool) herdrtest.Handler {
	return func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Result: map[string]any{"type": "agent_view", "active": active, "source": "plugin:nerdsrun.agentisan"}}
	}
}

// TestSetAgentViewSendsTheSchemaShape: agent.view.set takes
// AgentViewSetParams (herdr 0.9.3 src/api/schema/agents.rs:52-61): the
// filter a tree tagged by op (:69-91), a builtin field as its name (:93-103),
// and each sort a field and an order (:129-160). What a view leaves unset is
// left out, as herdr's own serde does.
func TestSetAgentViewSendsTheSchemaShape(t *testing.T) {
	t.Parallel()

	team := herdr.AnyOf(herdr.In(herdr.ViewWorkspaceID, "wJ", "wN"), herdr.In(herdr.ViewStatus, "blocked"))
	tests := map[string]struct {
		view herdr.AgentView
		want string
	}{
		"a team's view": {
			view: herdr.AgentView{Source: "plugin:nerdsrun.agentisan", Label: "agentisan", Filter: &team, Sort: []herdr.ViewSort{herdr.ByToken("order_ha")}},
			want: `{"source":"plugin:nerdsrun.agentisan","label":"agentisan","filter":{"op":"any","filters":[` +
				`{"op":"in","field":"workspace_id","values":["wJ","wN"]},{"op":"in","field":"status","values":["blocked"]}]},` +
				`"sort":[{"field":{"token":"order_ha"},"order":"asc"}]}`,
		},
		"only a source": {
			view: herdr.AgentView{Source: "plugin:nerdsrun.agentisan"},
			want: `{"source":"plugin:nerdsrun.agentisan"}`,
		},
		"a label herdr counts as one character a glyph": {
			view: herdr.AgentView{Source: "s", Label: "◆ spaces"},
			want: `{"source":"s","label":"◆ spaces"}`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, viewReply(true))

			require.NoError(t, herdr.Client{SocketPath: srv.Path}.SetAgentView(t.Context(), tc.view))

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "agent.view.set", reqs[0].Method)
			assert.JSONEq(t, tc.want, string(reqs[0].Params))
		})
	}
}

// TestClearAgentViewSendsTheSource: agent.view.clear takes
// AgentViewClearParams (agents.rs:63-67), and herdr clears the view only
// when that source set it (agent_view.rs:55-62). Either way it answers with
// the view it now has: active when another source's view stays, which is
// not an error.
func TestClearAgentViewSendsTheSource(t *testing.T) {
	t.Parallel()

	for name, active := range map[string]bool{"the view is cleared": false, "another source's view stays": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, viewReply(active))

			require.NoError(t, herdr.Client{SocketPath: srv.Path}.ClearAgentView(t.Context(), "plugin:nerdsrun.agentisan"))

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "agent.view.clear", reqs[0].Method)
			assert.JSONEq(t, `{"source":"plugin:nerdsrun.agentisan"}`, string(reqs[0].Params))
		})
	}
}

// TestAgentViewFailureClasses: herdr refuses a view it cannot take with an
// error body that keeps the view it had (agent_view.rs:12-29), which the
// caller tells apart by its code; an answer that is not the view herdr has
// is a protocol violation, and the transport fails as every call does.
func TestAgentViewFailureClasses(t *testing.T) {
	t.Parallel()

	refuse := func(code string) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply {
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: code, Message: code}}
		}
	}
	reply := func(result map[string]any) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: result} }
	}
	silent := func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} }
	set := func(c herdr.Client) error {
		return c.SetAgentView(t.Context(), herdr.AgentView{Source: "plugin:nerdsrun.agentisan", Label: "agentisan"})
	}
	clear := func(c herdr.Client) error { return c.ClearAgentView(t.Context(), "plugin:nerdsrun.agentisan") }

	tests := map[string]struct {
		handler herdrtest.Handler
		call    func(herdr.Client) error
		want    error
		code    string
	}{
		"set: the plugin is not linked":   {handler: refuse("plugin_not_found"), call: set, want: herdr.ErrAPI, code: "plugin_not_found"},
		"set: the plugin is disabled":     {handler: refuse("plugin_disabled"), call: set, want: herdr.ErrAPI, code: "plugin_disabled"},
		"set: an invalid view":            {handler: refuse("invalid_agent_view"), call: set, want: herdr.ErrAPI, code: "invalid_agent_view"},
		"set: wrong result type":          {handler: reply(map[string]any{"type": "ok"}), call: set, want: herdr.ErrProtocol},
		"set: answered with no view":      {handler: reply(map[string]any{"type": "agent_view", "active": false}), call: set, want: herdr.ErrProtocol},
		"set: active is not a bool":       {handler: reply(map[string]any{"type": "agent_view", "active": "yes"}), call: set, want: herdr.ErrProtocol},
		"set: closed without a reply":     {handler: silent, call: set, want: herdr.ErrUnavailable},
		"set: no socket":                  {call: set, want: herdr.ErrNoSocket},
		"clear: an invalid source":        {handler: refuse("invalid_agent_view"), call: clear, want: herdr.ErrAPI, code: "invalid_agent_view"},
		"clear: wrong result type":        {handler: reply(map[string]any{"type": "ok"}), call: clear, want: herdr.ErrProtocol},
		"clear: closed without a reply":   {handler: silent, call: clear, want: herdr.ErrUnavailable},
		"clear: no socket":                {call: clear, want: herdr.ErrNoSocket},
		"clear: a reply without its type": {handler: reply(map[string]any{"active": false}), call: clear, want: herdr.ErrProtocol},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := herdr.Client{}
			if tc.handler != nil {
				client.SocketPath = herdrtest.Start(t, tc.handler).Path
			}
			err := tc.call(client)
			require.ErrorIs(t, err, tc.want)
			if tc.code != "" {
				var apiErr *herdr.APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, tc.code, apiErr.Code)
			}
			assert.False(t, errors.Is(err, herdr.ErrPaneNotFound), "no view error is a missing pane")
		})
	}
}

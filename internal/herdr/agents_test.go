package herdr_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// agentEntry is one agent in an agent_list result, with the fields herdr
// 0.9.3's AgentInfo requires (src/api/schema/agents.rs:186-229). A nil name
// is left out, as herdr leaves out an unnamed agent's.
func agentEntry(paneID string, name any) map[string]any {
	a := map[string]any{
		"terminal_id": "t-" + paneID, "agent_status": "working", "workspace_id": "w2", "tab_id": "w2:t1",
		"pane_id": paneID, "focused": false, "revision": 3,
	}
	if name != nil {
		a["name"] = name
	}
	return a
}

// withSeq is a with completion_seq set to seq.
func withSeq(a map[string]any, seq any) map[string]any {
	a["completion_seq"] = seq
	return a
}

// TestListAgentsSendsTheSchemaShape: agent.list takes no parameters and
// answers every agent terminal with its pane and, when it has one, its name.
// A shell is never in it (src/app/agents.rs:366-376).
func TestListAgentsSendsTheSchemaShape(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		agents []map[string]any
		want   []herdr.AgentInfo
	}{
		"named and unnamed agents": {
			agents: []map[string]any{agentEntry("w2:p1", "pee01"), agentEntry("w2:p2", nil)},
			want:   []herdr.AgentInfo{{PaneID: "w2:p1", Name: "pee01", TerminalID: "t-w2:p1"}, {PaneID: "w2:p2", TerminalID: "t-w2:p2"}},
		},
		"a null name is no name":  {agents: []map[string]any{{"pane_id": "w2:p1", "name": nil}}, want: []herdr.AgentInfo{{PaneID: "w2:p1"}}},
		"a non-ASCII name":        {agents: []map[string]any{agentEntry("w2:p1", "エージェント")}, want: []herdr.AgentInfo{{PaneID: "w2:p1", Name: "エージェント", TerminalID: "t-w2:p1"}}},
		"no agents at all":        {agents: []map[string]any{}, want: []herdr.AgentInfo{}},
		"fields it does not read": {agents: []map[string]any{agentEntry("w2:p1", "qa")}, want: []herdr.AgentInfo{{PaneID: "w2:p1", Name: "qa", TerminalID: "t-w2:p1"}}},
		// completion_seq is left out until a turn ends, and again once the
		// agent changes state (src/api/schema/agents.rs:221-223).
		"a turn's end":           {agents: []map[string]any{withSeq(agentEntry("w2:p1", "qa"), 11358)}, want: []herdr.AgentInfo{{PaneID: "w2:p1", Name: "qa", TerminalID: "t-w2:p1", CompletionSeq: new(uint64(11358))}}},
		"a turn's end of zero":   {agents: []map[string]any{withSeq(agentEntry("w2:p1", "qa"), 0)}, want: []herdr.AgentInfo{{PaneID: "w2:p1", Name: "qa", TerminalID: "t-w2:p1", CompletionSeq: new(uint64(0))}}},
		"the largest turn's end": {agents: []map[string]any{withSeq(agentEntry("w2:p1", "qa"), uint64(18446744073709551615))}, want: []herdr.AgentInfo{{PaneID: "w2:p1", Name: "qa", TerminalID: "t-w2:p1", CompletionSeq: new(uint64(18446744073709551615))}}},
		"a null turn's end":      {agents: []map[string]any{withSeq(agentEntry("w2:p1", "qa"), nil)}, want: []herdr.AgentInfo{{PaneID: "w2:p1", Name: "qa", TerminalID: "t-w2:p1"}}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply {
				return herdrtest.Reply{Result: map[string]any{"type": "agent_list", "agents": tc.agents}}
			})

			got, err := herdr.Client{SocketPath: srv.Path}.ListAgents(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			reqs := srv.Requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, "agent.list", reqs[0].Method)
			assert.JSONEq(t, `{}`, string(reqs[0].Params), "the schema requires an object, even empty")
		})
	}
}

// TestListAgentsFailureClasses: agent.list fails the way every call does, and a result missing what the schema requires is a protocol
// violation rather than a silently empty answer.
func TestListAgentsFailureClasses(t *testing.T) {
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
	list := func(c herdr.Client) error { _, err := c.ListAgents(t.Context()); return err }

	tests := map[string]struct {
		handler herdrtest.Handler
		call    func(herdr.Client) error
		want    error
	}{
		"list: wrong result type":             {handler: reply(map[string]any{"type": "pane_list", "panes": []any{}}), call: list, want: herdr.ErrProtocol},
		"list: agents missing":                {handler: reply(map[string]any{"type": "agent_list"}), call: list, want: herdr.ErrProtocol},
		"list: agents is not an array":        {handler: reply(map[string]any{"type": "agent_list", "agents": "pee01"}), call: list, want: herdr.ErrProtocol},
		"list: an agent without a pane":       {handler: reply(map[string]any{"type": "agent_list", "agents": []map[string]any{agentEntry("", "pee01")}}), call: list, want: herdr.ErrProtocol},
		"list: a name that is no string":      {handler: reply(map[string]any{"type": "agent_list", "agents": []map[string]any{agentEntry("w2:p1", 7)}}), call: list, want: herdr.ErrProtocol},
		"list: a terminal that is no string":  {handler: reply(map[string]any{"type": "agent_list", "agents": []map[string]any{{"pane_id": "w2:p1", "terminal_id": 7}}}), call: list, want: herdr.ErrProtocol},
		"list: a negative turn's end":         {handler: reply(map[string]any{"type": "agent_list", "agents": []map[string]any{withSeq(agentEntry("w2:p1", "qa"), -1)}}), call: list, want: herdr.ErrProtocol},
		"list: a turn's end past uint64":      {handler: reply(map[string]any{"type": "agent_list", "agents": []map[string]any{withSeq(agentEntry("w2:p1", "qa"), 18446744073709551616.0)}}), call: list, want: herdr.ErrProtocol},
		"list: a fractional turn's end":       {handler: reply(map[string]any{"type": "agent_list", "agents": []map[string]any{withSeq(agentEntry("w2:p1", "qa"), 1.5)}}), call: list, want: herdr.ErrProtocol},
		"list: a turn's end that is a string": {handler: reply(map[string]any{"type": "agent_list", "agents": []map[string]any{withSeq(agentEntry("w2:p1", "qa"), "7")}}), call: list, want: herdr.ErrProtocol},
		"list: an error body":                 {handler: apiErr("server_unavailable"), call: list, want: herdr.ErrAPI},
		"list: closed without a reply":        {handler: silent, call: list, want: herdr.ErrUnavailable},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)

			err := tc.call(herdr.Client{SocketPath: srv.Path})
			require.ErrorIs(t, err, tc.want)
		})
	}
}

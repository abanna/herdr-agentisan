package dashboard_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// TestLocatorWhenHerdrRefusesThePaneList: a herdr that answers the pane
// list with an error, or with something the schema does not allow, was
// asked: HERDR_WORKSPACE_ID is not the place then, and the footer says
// herdr's answer, not that it is not answering.
func TestLocatorWhenHerdrRefusesThePaneList(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reply herdrtest.Reply
		is    error
	}{
		"an API error":        {reply: herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "internal", Message: "pane list failed"}}, is: herdr.ErrAPI},
		"a wrong result type": {reply: herdrtest.Reply{Result: map[string]any{"type": "pong"}}, is: herdr.ErrProtocol},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(r herdrtest.Request) herdrtest.Reply {
				if r.Method == "pane.list" {
					return tc.reply
				}
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "invalid_request", Message: "unexpected " + r.Method}}
			})
			l := &dashboard.Locator{Herdr: herdr.Client{SocketPath: srv.Path}, PID: pid, PaneID: "w2:p1", WorkspaceID: "w2"}
			p, err := l.Locate(t.Context())
			require.ErrorIs(t, err, dashboard.ErrNoTeamHere)
			require.ErrorIs(t, err, tc.is)
			assert.Empty(t, p.WorkspaceID, "herdr answered: HERDR_WORKSPACE_ID is not the place")
			assert.Contains(t, err.Error(), "herdr cannot say where this dashboard runs")
			assert.Contains(t, err.Error(), "pass --project")
			assert.NotContains(t, err.Error(), "not answering")
		})
	}
}

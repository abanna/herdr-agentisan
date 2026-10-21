package herdr_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
)

// TestRetireCallsSendTheSchemaShape: pane.get and pane.close send herdr
// 0.9.3's params exactly and read their results.
func TestRetireCallsSendTheSchemaShape(t *testing.T) {
	t.Parallel()
	pane := map[string]any{
		"pane_id": "w14:p7", "terminal_id": "term-w14:p7", "workspace_id": "w14", "tab_id": "w14:t2",
		"focused": false, "agent_status": "unknown", "revision": 0,
	}
	tests := map[string]struct {
		result map[string]any
		call   func(herdr.Client) (any, error)
		method string
		want   any
	}{
		"pane.get": {
			result: map[string]any{"type": "pane_info", "pane": pane},
			call:   func(c herdr.Client) (any, error) { return c.GetPane(t.Context(), "w14:p7") },
			method: "pane.get",
			want:   herdr.PaneInfo{PaneID: "w14:p7", TerminalID: "term-w14:p7", WorkspaceID: "w14", TabID: "w14:t2", AgentStatus: "unknown"},
		},
		"pane.close": {
			result: map[string]any{"type": "ok"},
			call:   func(c herdr.Client) (any, error) { return nil, c.ClosePane(t.Context(), "w14:p7") },
			method: "pane.close",
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
			assert.JSONEq(t, `{"pane_id":"w14:p7"}`, string(reqs[0].Params))
		})
	}
}

// TestRetireCallFailureClasses: a pane herdr does not have is
// ErrPaneNotFound and ErrAPI; a result the schema does not allow is a
// protocol violation; the transport fails as every call does.
func TestRetireCallFailureClasses(t *testing.T) {
	t.Parallel()
	reply := func(result map[string]any) herdrtest.Handler {
		return func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Result: result} }
	}
	notFound := func(herdrtest.Request) herdrtest.Reply {
		return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "pane_not_found", Message: "pane_not_found"}}
	}
	silent := func(herdrtest.Request) herdrtest.Reply { return herdrtest.Reply{Silent: true} }
	getPane := func(c herdr.Client) error { _, err := c.GetPane(t.Context(), "w14:p7"); return err }
	closePane := func(c herdr.Client) error { return c.ClosePane(t.Context(), "w14:p7") }

	tests := map[string]struct {
		handler herdrtest.Handler
		call    func(herdr.Client) error
		want    []error
	}{
		"pane.get: pane not found":   {handler: notFound, call: getPane, want: []error{herdr.ErrPaneNotFound, herdr.ErrAPI}},
		"pane.get: wrong type":       {handler: reply(map[string]any{"type": "pane_list", "pane": map[string]any{"pane_id": "w14:p7"}}), call: getPane, want: []error{herdr.ErrProtocol}},
		"pane.close: pane not found": {handler: notFound, call: closePane, want: []error{herdr.ErrPaneNotFound, herdr.ErrAPI}},
		"pane.close: wrong type":     {handler: reply(map[string]any{"type": "pane_info"}), call: closePane, want: []error{herdr.ErrProtocol}},
		"closed without a reply":     {handler: silent, call: closePane, want: []error{herdr.ErrUnavailable}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, tc.handler)

			err := tc.call(herdr.Client{SocketPath: srv.Path})
			for _, want := range tc.want {
				require.ErrorIs(t, err, want)
			}
		})
	}
}

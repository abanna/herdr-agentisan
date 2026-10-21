package herdr

import (
	"context"
	"fmt"
)

// The builtin fields a view's filter tests, of those this plugin uses
// (src/api/schema/agents.rs:93-103).
const (
	// ViewWorkspaceID is the id of the pane's workspace, exactly as
	// workspace.list gives it: an alias or a stale id matches nothing.
	ViewWorkspaceID = "workspace_id"
	// ViewStatus is the agent's status: idle, working, blocked, done or
	// unknown, done being idle and not yet seen.
	ViewStatus = "status"
)

// AgentView is the payload of agent.view.set (src/api/schema/agents.rs:52-61),
// limited to what this plugin sends: the agents panel lists the panes Filter
// matches, in Sort's order, under Label. herdr holds one view, whichever
// source set it, and checks a view whole before it takes it
// (src/app/agent_view.rs:19-40).
type AgentView struct {
	// Source names the view's writer. One of the form plugin:<id> must name
	// a plugin installed and enabled, and herdr clears it when that plugin
	// is unlinked or disabled.
	Source string `json:"source"`
	// Label heads the panel: 1 to 32 characters once herdr has trimmed it
	// and dropped control characters (agent_view.rs:193-205). "" is left
	// out, and herdr shows "filtered".
	Label string `json:"label,omitempty"`
	// Filter selects the panes; nil lists every one.
	Filter *ViewFilter `json:"filter,omitempty"`
	// Sort orders them, the first field first. With none, the panel keeps
	// its own order and mode.
	Sort []ViewSort `json:"sort,omitempty"`
}

// ViewFilter is one node of a view's filter (agents.rs:69-91), of the two
// kinds this plugin builds: an any node, matching when one of Filters does,
// and an in node, matching when Field is exactly one of Values. herdr
// allows a tree of 64 nodes at most, 8 deep, an in node 1 to 32 values and
// an any node at least one filter (agent_view.rs:12-15, :207-260).
type ViewFilter struct {
	Op      string       `json:"op"`
	Filters []ViewFilter `json:"filters,omitempty"`
	Field   string       `json:"field,omitempty"`
	Values  []string     `json:"values,omitempty"`
}

// AnyOf matches a pane that one of filters matches.
func AnyOf(filters ...ViewFilter) ViewFilter {
	return ViewFilter{Op: "any", Filters: filters}
}

// In matches a pane whose builtin field is one of values.
func In(field string, values ...string) ViewFilter {
	return ViewFilter{Op: "in", Field: field, Values: values}
}

// ViewSort is one sort field, a token's value (agents.rs:129-160).
type ViewSort struct {
	Field struct {
		Token string `json:"token"`
	} `json:"field"`
	Order string `json:"order"`
}

// ByToken sorts by the token key, ascending: by string, a pane without the
// token after every pane with it, and stably, so panes that tie keep the
// panel's order (src/agent_view_eval.rs).
func ByToken(key string) ViewSort {
	s := ViewSort{Order: "asc"}
	s.Field.Token = key
	return s
}

// viewResult is herdr's answer to both view calls: the view it now has
// (src/api/schema/response.rs:110-116).
type viewResult struct {
	Type   string `json:"type"`
	Active bool   `json:"active"`
}

// SetAgentView replaces herdr's agent view with v
// (src/app/api/agent_view.rs:7-41). A view herdr cannot take is an ErrAPI
// whose code says why, and the view it had stays: invalid_agent_view, or,
// for a plugin source, plugin_not_found or plugin_disabled.
func (c Client) SetAgentView(ctx context.Context, v AgentView) error {
	var out viewResult
	if err := c.Call(ctx, "agent.view.set", v, &out); err != nil {
		return err
	}
	if out.Type != "agent_view" || !out.Active {
		return fmt.Errorf("%w: agent.view.set: result type %q, active %t, want an active \"agent_view\"", ErrProtocol, out.Type, out.Active)
	}
	return nil
}

// ClearAgentView clears herdr's agent view if source set it
// (agent_view.rs:43-72); a view another source set stays, which is not an
// error.
func (c Client) ClearAgentView(ctx context.Context, source string) error {
	params := struct {
		Source string `json:"source"`
	}{source}
	var out viewResult
	if err := c.Call(ctx, "agent.view.clear", params, &out); err != nil {
		return err
	}
	if out.Type != "agent_view" {
		return fmt.Errorf("%w: agent.view.clear: result type %q, want \"agent_view\"", ErrProtocol, out.Type)
	}
	return nil
}

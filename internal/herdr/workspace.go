package herdr

import (
	"context"
	"fmt"
)

// WorkspaceInfo is one workspace in workspace.list, limited to the fields
// this plugin reads (herdr 0.9.3 src/api/schema/workspaces.rs:61-76).
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	// Label is the name the sidebar shows: the one the user gave the
	// workspace, or one herdr derived from its directory.
	Label string `json:"label"`
}

// ListWorkspaces returns every workspace, in sidebar order.
func (c Client) ListWorkspaces(ctx context.Context) ([]WorkspaceInfo, error) {
	var out struct {
		Type       string          `json:"type"`
		Workspaces []WorkspaceInfo `json:"workspaces"`
	}
	if err := c.Call(ctx, "workspace.list", struct{}{}, &out); err != nil {
		return nil, err
	}
	if out.Type != "workspace_list" {
		return nil, fmt.Errorf("%w: workspace.list: result type %q, want \"workspace_list\"", ErrProtocol, out.Type)
	}
	if out.Workspaces == nil {
		return nil, fmt.Errorf("%w: workspace.list: result has no workspaces", ErrProtocol)
	}
	for _, w := range out.Workspaces {
		if w.WorkspaceID == "" {
			return nil, fmt.Errorf("%w: workspace.list: a workspace has no workspace_id", ErrProtocol)
		}
	}
	return out.Workspaces, nil
}

// WorkspaceMetadata is the payload of workspace.report_metadata, limited to
// the token fields this plugin sends (src/api/schema/workspaces.rs:48-59).
// Its rules are pane.report_metadata's: see PaneMetadata.
type WorkspaceMetadata struct {
	WorkspaceID string `json:"workspace_id"`
	Source      string `json:"source"`
	// Tokens maps key to value. An empty value clears the key.
	Tokens map[string]string `json:"tokens,omitempty"`
	// TTLMillis expires the tokens. Zero is omitted, which herdr reads as
	// "never expires"; herdr rejects an explicit 0 and anything over 24 h.
	TTLMillis uint64 `json:"ttl_ms,omitempty"`
}

// ReportWorkspaceMetadata sets tokens on a workspace. As with a pane, herdr
// answers ok without applying a report it considers stale, so success means
// accepted (src/app/api/workspaces.rs:241-309). A workspace herdr does not
// know is an ErrAPI with code workspace_not_found.
func (c Client) ReportWorkspaceMetadata(ctx context.Context, m WorkspaceMetadata) error {
	var out struct {
		Type string `json:"type"`
	}
	if err := c.Call(ctx, "workspace.report_metadata", m, &out); err != nil {
		return err
	}
	if out.Type != "ok" {
		return fmt.Errorf("%w: workspace.report_metadata: result type %q, want \"ok\"", ErrProtocol, out.Type)
	}
	return nil
}

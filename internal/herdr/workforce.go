package herdr

import (
	"context"
	"fmt"
)

// The calls worker retire makes (ADR-001 D11, A23, A35): re-read a worker's
// pane and close it. Every shape is herdr 0.9.3's (src/api/schema/*.rs,
// src/app/api/*.rs).

// GetPane reads one pane: pane.get, answered with pane_info. An id herdr
// does not have is ErrPaneNotFound.
func (c Client) GetPane(ctx context.Context, paneID string) (PaneInfo, error) {
	return c.paneCall(ctx, "pane.get", paneID)
}

// ClosePane closes one pane, and its tab or workspace with it when it was
// the last there (A15): pane.close, answered with ok.
func (c Client) ClosePane(ctx context.Context, paneID string) error {
	return c.callOK(ctx, "pane.close", struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID})
}

func (c Client) callOK(ctx context.Context, method string, params any) error {
	var out struct {
		Type string `json:"type"`
	}
	if err := c.Call(ctx, method, params, &out); err != nil {
		return err
	}
	if out.Type != "ok" {
		return fmt.Errorf("%w: %s: result type %q, want \"ok\"", ErrProtocol, method, out.Type)
	}
	return nil
}

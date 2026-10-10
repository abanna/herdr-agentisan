package dashboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// ErrFocus means an agent could not be focused. The dashboard shows it in the
// footer; it is never fatal.
var ErrFocus = errors.New("focus failed")

// Focuser brings an agent's pane to the front. Enter and a left click call it
// with the selected agent.
type Focuser interface {
	Focus(ctx context.Context, a snapshot.Agent) error
}

// HerdrFocuser focuses through herdr's socket (ADR-001 D5 "Jump"): agent.focus
// by the agent's name, then pane.zoom on its pane. A failed focus skips the
// zoom.
type HerdrFocuser struct {
	Client herdr.Client
}

// Focus focuses a, then zooms the pane herdr just focused. The snapshot's
// pane_id is never the zoom target: pane.zoom also focuses its target, and a
// snapshot id goes stale when a pane moves or herdr reissues the id, so
// zooming it would move the user off the agent. FocusAgent refuses an answer
// without a pane id, so the zoom never names an empty one.
func (f HerdrFocuser) Focus(ctx context.Context, a snapshot.Agent) error {
	info, err := f.Client.FocusAgent(ctx, a.Name)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFocus, err)
	}
	if _, err := f.Client.ZoomPane(ctx, info.PaneID); err != nil {
		return fmt.Errorf("%w: %w", ErrFocus, err)
	}
	return nil
}

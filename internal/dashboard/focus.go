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

// errNoPaneID is an unnamed row with no pane id: nothing herdr can focus.
var errNoPaneID = errors.New("a row with no agent name has no pane id to focus")

// Focuser brings an agent's pane to the front. Enter and a left click call it
// with the selected agent.
type Focuser interface {
	Focus(ctx context.Context, a snapshot.Agent) error
}

// HerdrFocuser focuses through herdr's socket (ADR-001 D5 "Jump"): agent.focus
// by the agent's name, or pane.focus by the pane id for an unnamed row, then
// pane.zoom on the pane herdr focused. A failed focus skips the zoom.
type HerdrFocuser struct {
	Client herdr.Client
}

// Focus focuses a, then zooms the pane herdr just focused.
//
// A named row goes by its name, which survives herdr reissuing pane ids. An
// unnamed row (a shell, or an agent nobody named) is named by a label or a
// pane id, which agent.focus does not resolve: herdr 0.9.3 takes an agent's
// name or an agent terminal's pane id, never a label, and no shell at all
// (src/app/terminal_targets.rs:74-104). So it goes by its pane id through
// pane.focus, which takes any pane (src/app/api/panes.rs:484-500), and a
// label that is also another agent's name can never reach that agent. Its
// id is as old as the snapshot, a few seconds at most (the daemon's 3 s
// poll, then the dashboard's 1 s one); ADR-001 A18 accepts that.
//
// The snapshot's pane_id is never the zoom target: pane.zoom also focuses
// its target, and a snapshot id goes stale when a pane moves or herdr
// reissues the id, so zooming it would move the user off the agent. Both
// focus calls refuse an answer without a pane id, so the zoom never names an
// empty one.
func (f HerdrFocuser) Focus(ctx context.Context, a snapshot.Agent) error {
	paneID, err := f.focus(ctx, a)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFocus, err)
	}
	if _, err := f.Client.ZoomPane(ctx, paneID); err != nil {
		return fmt.Errorf("%w: %w", ErrFocus, err)
	}
	return nil
}

// focus focuses a and returns the id herdr answered with.
func (f HerdrFocuser) focus(ctx context.Context, a snapshot.Agent) (string, error) {
	if !a.Unnamed {
		info, err := f.Client.FocusAgent(ctx, a.Name)
		return info.PaneID, err //nolint:wrapcheck // Focus wraps it with ErrFocus
	}
	if a.PaneID == "" {
		return "", fmt.Errorf("%w: %q", errNoPaneID, a.Name)
	}
	info, err := f.Client.FocusPane(ctx, a.PaneID)
	return info.PaneID, err //nolint:wrapcheck // Focus wraps it with ErrFocus
}

package plugin

import (
	"context"
	"errors"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// BackAction is Back's [[actions]] id; a key binding names ID + "." + BackAction.
const BackAction = "back"

// BackTitle titles the toast a Back that went nowhere shows.
const BackTitle = "Agentisan: back"

// GoBack asks the daemon to go back: daemon.Back on its socket.
type GoBack func(ctx context.Context) (daemon.BackResult, error)

// Back runs the back action (ADR-001 D5, A16) through the daemon. A key press
// that went nowhere must say so, and herdr only logs a failed plugin command
// (src/app/api.rs:136-163 at v0.9.3): run as a herdr action (env.ActionID
// set), Back shows a toast saying why; by hand, the caller prints the error,
// which is returned either way.
func Back(ctx context.Context, env Env, goBack GoBack, n Notifier) (daemon.BackResult, error) {
	res, err := goBack(ctx)
	if err == nil {
		return res, nil
	}
	err = fmt.Errorf("back: %w", err)
	if env.ActionID == "" {
		return daemon.BackResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, herdr.CallTimeout)
	defer cancel()
	if _, toastErr := n.ShowNotification(ctx, herdr.Notification{Title: BackTitle, Body: backFailure(err)}); toastErr != nil {
		return daemon.BackResult{}, errors.Join(err, fmt.Errorf("show why in a toast: %w", toastErr))
	}
	return daemon.BackResult{}, err
}

// backFailure is what the toast tells the user.
func backFailure(err error) string {
	var refused *daemon.RequestError
	switch {
	case errors.Is(err, daemon.ErrUnavailable):
		return "The agentisan daemon is not running. Run the action \"Agentisan: restart daemon\"."
	case errors.Is(err, daemon.ErrNoHistory):
		return "No earlier pane to go back to."
	case errors.As(err, &refused):
		return "Back failed: " + refused.Message
	default:
		return "Back failed: " + err.Error()
	}
}

package plugin

import (
	"context"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// PingTitle is the toast title the ping action shows.
const PingTitle = "agentisan plugin alive"

// Notifier is the slice of the herdr client Ping needs.
type Notifier interface {
	ShowNotification(ctx context.Context, n herdr.Notification) (herdr.NotificationResult, error)
}

// PingResult reports what herdr did with the toast.
type PingResult struct {
	Shown  bool   `json:"shown"`
	Reason string `json:"reason"`
}

// Ping proves the plugin is wired end to end: herdr launched it, and it can
// reach herdr's socket. herdr declining to display the toast (disabled,
// rate-limited, no foreground client, busy) still proves both, so it is a
// successful ping that reports the reason, not an error.
func Ping(ctx context.Context, n Notifier, version string) (PingResult, error) {
	res, err := n.ShowNotification(ctx, herdr.Notification{
		Title: PingTitle,
		Body:  "herdr-agentisan " + version,
	})
	if err != nil {
		return PingResult{}, fmt.Errorf("ping: %w", err)
	}
	return PingResult{Shown: res.Shown, Reason: res.Reason}, nil
}

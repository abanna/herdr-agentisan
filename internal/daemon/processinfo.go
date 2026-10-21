package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/herdr"
)

// paneProcesses are one poll's pane.process_info replies, by pane, so the
// reconcile's shell reads (A35) and the launch-model asks (A39) share one
// ask per pane per poll. A failed ask is held too, so the pane is asked again
// only at the next poll; ErrHerdrGone is not, as it ends the poll.
type paneProcesses map[string]paneReply

// paneReply is one pane's process_info reply.
type paneReply struct {
	info herdr.ProcessInfo
	err  error
}

// ask is pane's reply this poll, asking herdr only the first time.
func (pp paneProcesses) ask(ctx context.Context, c herdr.Client, pane string) (herdr.ProcessInfo, error) {
	if r, ok := pp[pane]; ok {
		return r.info, r.err
	}
	info, err := c.PaneProcessInfo(ctx, pane)
	if err != nil {
		err = fmt.Errorf("read pane %s's processes: %w", pane, err)
	}
	if !errors.Is(err, ErrHerdrGone) {
		pp[pane] = paneReply{info: info, err: err}
	}
	return info, err
}

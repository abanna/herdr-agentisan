package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// The bounds of the wait before the daemon resubscribes to focus events.
const (
	resubscribeFirst = 100 * time.Millisecond
	resubscribeCap   = 30 * time.Second
)

// backoff is the wait before resubscribe attempt n, counted from 0: doubling
// from resubscribeFirst, never more than resubscribeCap.
func backoff(n int) time.Duration {
	d := resubscribeFirst
	for i := 0; i < n && d < resubscribeCap; i++ {
		d *= 2
	}
	return min(d, resubscribeCap)
}

// followFocus records focus in st until ctx ends (D3, A11): it subscribes to
// pane.focused, records the pane focused now, then every focus event.
//
// Whenever the subscription ends, because herdr dropped events the daemon
// had not read (ErrEventsLost) or the connection dropped, it resubscribes
// after a bounded backoff and resyncs the same way: focus may have moved
// while it was not listening (A3). It only ever follows the server ident
// names: once the socket is another server's, or gone, it stops, and Run's
// poll ends the daemon.
func (o Options) followFocus(ctx context.Context, st *store.Store, ident Identity) {
	failures := 0
	for {
		err := o.streamFocus(ctx, st, ident, &failures)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrHerdrGone) {
			o.Logger.Info().Err(err).Msg("focus collection stops: the daemon's herdr server was replaced")
			return
		}
		wait := backoff(failures)
		failures++
		if errors.Is(err, herdr.ErrEventsLost) {
			o.Logger.Info().Err(err).Dur("retry_in", wait).Msg("herdr dropped focus events; resubscribing")
		} else {
			o.Logger.Warn().Err(err).Dur("retry_in", wait).Msg("focus subscription ended; resubscribing")
		}
		if !o.sameServer(ident) || sleep(ctx, wait) != nil || !o.sameServer(ident) {
			o.Logger.Info().Msg("focus collection stops: the daemon's herdr server is gone or stopping")
			return
		}
	}
}

// streamFocus subscribes to focus events, records the pane focused now, then
// records every focus event until the subscription ends, and returns why it
// ended. An event resets failures: the subscription is working.
//
// The identity is checked again once the subscription is acknowledged. A
// socket replaced at any moment from the last check up to then is seen: a
// socket identity never recurs, so a connection that may have reached the
// replacement is closed unread (ErrHerdrGone), and nothing it, or the
// replacement's pane.list, says is recorded.
func (o Options) streamFocus(ctx context.Context, st *store.Store, ident Identity, failures *int) error {
	sub, err := o.Herdr.Subscribe(ctx, herdr.SubscribePaneFocused)
	if err != nil {
		return fmt.Errorf("subscribe to focus events: %w", err)
	}
	defer sub.Close() //nolint:errcheck // the subscription is over either way
	if !o.sameServer(ident) {
		return fmt.Errorf("%w: the socket was replaced while subscribing to focus events", ErrHerdrGone)
	}
	o.snapshotFocus(ctx, st)
	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			return fmt.Errorf("focus events: %w", err)
		}
		*failures = 0
		if ev.Kind == herdr.EventPaneFocused {
			o.recordFocus(ctx, st, ev.PaneID)
		}
	}
}

// snapshotFocus records the pane herdr reports focused now. herdr reports at
// most one: the focused pane of the active tab of the active workspace.
func (o Options) snapshotFocus(ctx context.Context, st *store.Store) {
	panes, err := o.Herdr.ListPanes(ctx)
	if err != nil {
		o.Logger.Warn().Err(err).Msg("read the focused pane")
		return
	}
	for _, p := range panes {
		if p.Focused {
			o.recordFocus(ctx, st, p.PaneID)
			return
		}
	}
}

func (o Options) recordFocus(ctx context.Context, st *store.Store, pane string) {
	if err := st.RecordFocus(ctx, pane, o.Now()); err != nil && ctx.Err() == nil {
		o.Logger.Warn().Err(err).Str("pane", pane).Msg("record focus")
	}
}

// sameServer reports whether the herdr socket is still the one ident names.
func (o Options) sameServer(ident Identity) bool {
	now, err := HerdrIdentity(o.HerdrSocket)
	return err == nil && now == ident
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("daemon: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

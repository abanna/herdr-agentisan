package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// viewSource is the agent view's source: "plugin:" and this plugin's id,
// plugin.ID, which internal/plugin cannot give here, since it imports this
// package. herdr takes the view only from a plugin installed and enabled,
// and clears it when the plugin is unlinked or disabled (A28 addendum).
const viewSource = "plugin:nerdsrun.agentisan"

// viewClearTimeout bounds the clear on a clean stop, which runs on a context
// of its own: the daemon's has ended by then.
const viewClearTimeout = 2 * time.Second

// viewSync is the agent view's state between the goroutines that feed it
// and the one that sends it (A28 addendum): the focus collector gives the
// focused workspace, the team poll each model it builds and the workspace
// its pane list had focused (A49), and syncView sets the view of the team
// that holds the workspace, or clears it outside every team. It sends
// nothing when that is what it last sent, and forgets what it sent after a
// call that fails and whenever focus is read afresh, so the next decision
// sends again: herdr has no call to read the view, and another source, or an
// unlinked plugin, may have replaced it. Every poll is a decision, so a
// failed call is tried again within one.
type viewSync struct {
	wake chan struct{}

	mu sync.Mutex
	// workspace is the focused workspace; "" until focus is known.
	workspace string
	// model is the last model a poll built; nil until one is.
	model *team.Model
	// sent is the view the last call that succeeded left; nil when unknown.
	sent *viewState
	// forgets counts the times sent was forgotten, so a send that raced a
	// forget does not record what it sent.
	forgets uint64
	// seen counts the focused workspaces the collector recorded, so a
	// poll's focus that one may have overtaken is dropped.
	seen uint64
}

// viewState is a view to set, or a clear.
type viewState struct {
	clear bool
	view  herdr.AgentView
}

func newViewSync() *viewSync { return &viewSync{wake: make(chan struct{}, 1)} }

// focus records the focused workspace. One that is "", from an event that
// carries none, changes nothing.
func (v *viewSync) focus(workspace string) {
	if workspace == "" {
		return
	}
	v.mu.Lock()
	v.workspace = workspace
	v.seen++
	v.mu.Unlock()
	v.poke()
}

// resynced records focus read afresh after a time the daemon was not
// listening: the focused workspace, or "" when none was read, which keeps
// the one known, and it forgets the view last sent.
func (v *viewSync) resynced(workspace string) {
	v.mu.Lock()
	if workspace != "" {
		v.workspace = workspace
		v.seen++
	}
	v.sent = nil
	v.forgets++
	v.mu.Unlock()
	v.poke()
}

// focusSeen is the count of focused workspaces the collector has recorded,
// which a poll reads before it lists the panes and gives back to polled.
func (v *viewSync) focusSeen() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seen
}

// polled records a model the poll built and the workspace of the pane its
// list had focused, "" for none, which changes nothing (A49). herdr can move
// focus without a pane.focused event, so the poll's list is a source of
// focus too, but an older one than any event the collector recorded after
// the poll read seen: the focus is taken only when the collector recorded
// none since. Unlike a resync it forgets nothing, so a poll that finds focus
// where it was sends nothing.
func (v *viewSync) polled(m team.Model, workspace string, seen uint64) {
	v.mu.Lock()
	v.model = &m
	if workspace != "" && v.seen == seen {
		v.workspace = workspace
	}
	v.mu.Unlock()
	v.poke()
}

func (v *viewSync) forget() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sent = nil
	v.forgets++
}

// poke wakes syncView; a wake already pending stands for this one.
func (v *viewSync) poke() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

// next is the view to send now, with the forget count it was decided at; ok
// is false while focus or the model is unknown, or when the view is the one
// last sent.
func (v *viewSync) next() (want viewState, forgets uint64, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.model == nil || v.workspace == "" {
		return viewState{}, 0, false
	}
	view, inTeam := team.View(*v.model, v.workspace)
	if inTeam {
		view.Source = viewSource
		want.view = view
	}
	want.clear = !inTeam
	if v.sent != nil && reflect.DeepEqual(*v.sent, want) {
		return viewState{}, 0, false
	}
	return want, v.forgets, true
}

// sentAt records s as sent, unless what was sent was forgotten since the
// decision at forgets.
func (v *viewSync) sentAt(s viewState, forgets uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.forgets == forgets {
		v.sent = &s
	}
}

// syncView sends v's view on every change until ctx ends or the daemon's
// herdr server goes, then, on a clean stop, clears it. Its client checks on
// every dial that the socket still names the daemon's server (A3), so no
// call reaches a replacement; one that finds another stops it, with no
// clear. A failure is logged when it differs from the last one logged, not
// every poll: plugin_not_found, while the plugin is not linked, would
// otherwise repeat every 3 s.
func (o Options) syncView(ctx context.Context, v *viewSync, ident Identity) {
	onServer := o.onServer(ident)
	c := herdr.Client{SocketPath: o.HerdrSocket, Dialed: onServer}
	logged := ""
	for {
		select {
		case <-ctx.Done():
			o.clearView(ctx, c, onServer)
			return
		case <-v.wake:
		}
		want, forgets, ok := v.next()
		if !ok {
			continue
		}
		err := sendView(ctx, c, want)
		if errors.Is(err, ErrHerdrGone) {
			o.Logger.Info().Err(err).Msg("agent view stops: the daemon's herdr server is gone or was replaced")
			return
		}
		if err != nil {
			v.forget()
			if why := failure(err); ctx.Err() == nil && why != logged {
				o.Logger.Warn().Err(err).Bool("clear", want.clear).Msg("agent view: the call failed; the next poll tries again")
				logged = why
			}
			continue
		}
		v.sentAt(want, forgets)
		logged = ""
	}
}

func sendView(ctx context.Context, c herdr.Client, s viewState) error {
	var err error
	if s.clear {
		err = c.ClearAgentView(ctx, viewSource)
	} else {
		err = c.SetAgentView(ctx, s.view)
	}
	if err != nil {
		return fmt.Errorf("send the agent view (clear %t): %w", s.clear, err)
	}
	return nil
}

// failure is what tells one failure from another for the log: herdr's code
// when it answered with one.
func failure(err error) string {
	if apiErr, ok := errors.AsType[*herdr.APIError](err); ok {
		return apiErr.Code
	}
	return err.Error()
}

// clearView clears the daemon's view on a clean stop. The socket is checked
// first, as the poll checks it: a socket that is gone fails the dial before
// the client's Dialed hook runs, and a server that is gone or replaced took
// the view with it, so there is nothing to clear and nothing to log.
func (o Options) clearView(ctx context.Context, c herdr.Client, onServer func() error) {
	if onServer() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), viewClearTimeout)
	defer cancel()
	if err := c.ClearAgentView(ctx, viewSource); err != nil && !errors.Is(err, ErrHerdrGone) {
		o.Logger.Warn().Err(err).Msg("clear the agent view on stop")
	}
}

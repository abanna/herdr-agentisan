package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/store"
)

// DefaultBackBudget bounds one back op (ADR-001 D5, A16): the history read,
// the wait for an earlier Back and every herdr call. Inside ioTimeout, a
// client hears why Back failed rather than timing out on the daemon.
const DefaultBackBudget = ioTimeout - time.Second

// maxPending bounds the moves herdr has yet to announce that Back remembers. A
// walk makes fewer than FocusCap, so dropping the oldest drops only stale ones.
const maxPending = store.FocusCap

// The back op's refusal codes.
const (
	codeNoHistory   = "no_history"
	codeHerdrFailed = "herdr_failed"
	codeStateFailed = "state_failed"
)

var (
	// ErrNoHistory means Back has no older pane to go to, and moved nothing.
	// A refusal with code no_history matches it.
	ErrNoHistory = errors.New("no earlier pane to go back to")
	// ErrHerdrCall means a herdr call Back made failed or timed out, and
	// nothing after it was sent. A refusal with code herdr_failed matches it.
	ErrHerdrCall = errors.New("herdr did not complete the move")
)

// BackResult is the back op's answer.
type BackResult struct {
	// To is the pane Back focused.
	To string `json:"to"`
	// From is the pane it left and un-zoomed; empty when herdr reported no
	// focused pane.
	From string `json:"from,omitempty"`
}

// walker is Back's place in the focus history, in memory: a restarted daemon
// starts a fresh walk.
type walker struct {
	one chan struct{} // held by the Back in progress

	mu sync.Mutex
	// While active, Back continues down rows, the history as it stood when
	// the walk began (newest first), below row at, where it took the user
	// to target. Rows recorded since, Back's own included, are off the walk.
	active bool
	rows   []store.Focus
	at     int
	target string
	// pending are Back's moves herdr has not announced yet, oldest first.
	pending []string
}

func newWalker() *walker { return &walker{one: make(chan struct{}, 1)} }

// observe is the collector reporting, before it records it, that pane took
// focus. A move of Back's keeps the walk going, however late herdr
// announces it, even after the next Back began; so does a resync finding the
// pane the walk left the user on. Any other focus ends the walk.
func (w *walker) observe(pane string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i := slices.Index(w.pending, pane); i >= 0 {
		w.pending = w.pending[i+1:]
	} else if pane != w.target {
		w.active = false
	}
}

// resynced forgets pending moves once the collector has read focus afresh:
// their announcements are lost, and a stale one could pass a move of the
// user's off as Back's.
func (w *walker) resynced() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = nil
}

// plan is one Back's view of the history: it may go to rows[from:]. here is
// where the walk left the user.
type plan struct {
	rows []store.Focus
	from int
	here string // "" for a fresh walk
}

func (w *walker) plan(fresh []store.Focus) plan {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active {
		return plan{rows: w.rows, from: w.at + 1, here: w.target}
	}
	return plan{rows: fresh}
}

// older reports whether a pane other than the user's lies below where the
// walk stands, or below the newest row for a fresh walk. Without one, Back
// refuses before asking herdr anything.
func (p plan) older() bool {
	here, from := p.here, p.from
	if here == "" {
		if len(p.rows) == 0 {
			return false
		}
		here, from = p.rows[0].PaneID, 1
	}
	return slices.ContainsFunc(p.rows[from:], func(f store.Focus) bool { return f.PaneID != here })
}

// pick returns the first row Back may go to: not the user's pane, which also
// passes over rows repeating it, and not one herdr no longer lists.
func (p plan) pick(current string, live map[string]bool) (int, bool) {
	for i := p.from; i < len(p.rows); i++ {
		if pane := p.rows[i].PaneID; pane != current && live[pane] {
			return i, true
		}
	}
	return 0, false
}

func (w *walker) expect(pane string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, pane)
	if n := len(w.pending); n > maxPending {
		w.pending = w.pending[n-maxPending:]
	}
}

func (w *walker) failed(pane string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if i := slices.Index(w.pending, pane); i >= 0 {
		w.pending = slices.Delete(w.pending, i, i+1)
	}
}

func (w *walker) went(p plan, i int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active, w.rows, w.at, w.target = true, p.rows, i, p.rows[i].PaneID
}

// back is the back op: from the focus history and herdr's panes, it goes to
// the newest older pane the walk has not passed, un-zooming the user's pane
// and then focusing the target. With nowhere to go it is ErrNoHistory; a
// herdr call that fails stops it there with ErrHerdrCall.
func (o Options) back(ctx context.Context, st *store.Store, w *walker) (BackResult, error) {
	ctx, cancel := context.WithTimeout(ctx, o.BackBudget)
	defer cancel()
	select {
	case w.one <- struct{}{}:
		defer func() { <-w.one }()
	case <-ctx.Done():
		return BackResult{}, fmt.Errorf("%w: an earlier Back is still waiting on herdr: %w", ErrHerdrCall, ctx.Err())
	}

	rows, err := st.RecentFocus(ctx, store.FocusCap)
	if err != nil {
		return BackResult{}, fmt.Errorf("back: %w", err)
	}
	p := w.plan(rows)
	if !p.older() {
		return BackResult{}, ErrNoHistory
	}
	panes, err := o.Herdr.ListPanes(ctx)
	if err != nil {
		return BackResult{}, fmt.Errorf("%w: %w", ErrHerdrCall, err)
	}
	current, live := "", make(map[string]bool, len(panes))
	for _, pane := range panes {
		live[pane.PaneID] = true
		if pane.Focused {
			current = pane.PaneID
		}
	}
	if p.here != "" && current != p.here {
		p = plan{rows: rows} // the user moved; herdr has not announced it yet
	}
	i, ok := p.pick(current, live)
	if !ok {
		return BackResult{}, ErrNoHistory
	}
	to := p.rows[i].PaneID
	if p.here == "" && current != "" && current != p.rows[0].PaneID {
		w.expect(current) // the user's move, still unannounced: no end to this walk
	}
	w.expect(to) // before the move: its announcement may beat the answer
	if current != "" {
		// Un-zoom first: pane.zoom focuses its target whatever the mode
		// (apply_pane_zoom, src/app/actions.rs:822 at v0.9.3), so after the
		// focus it would pull the user back. The pane is focused already, so
		// this moves and announces nothing (actions.rs:238-240).
		if _, err := o.Herdr.UnzoomPane(ctx, current); err != nil {
			w.failed(to)
			return BackResult{}, fmt.Errorf("%w: un-zoom %s: %w", ErrHerdrCall, current, err)
		}
	}
	if _, err := o.Herdr.FocusPane(ctx, to); err != nil {
		w.failed(to)
		return BackResult{}, fmt.Errorf("%w: focus %s: %w", ErrHerdrCall, to, err)
	}
	w.went(p, i)
	return BackResult{To: to, From: current}, nil
}

// backCode is the refusal code the back op answers err with.
func backCode(err error) string {
	switch {
	case errors.Is(err, ErrNoHistory):
		return codeNoHistory
	case errors.Is(err, ErrHerdrCall):
		return codeHerdrFailed
	default:
		return codeStateFailed
	}
}

// Back asks the daemon at socket to go back. A refusal is a *RequestError
// that errors.Is matches to ErrNoHistory or ErrHerdrCall by its code; a
// daemon that does not answer is ErrUnavailable.
func Back(ctx context.Context, socket string) (BackResult, error) {
	data, err := call(ctx, socket, "back")
	if err != nil {
		return BackResult{}, err
	}
	var res BackResult
	if err := json.Unmarshal(data, &res); err != nil {
		return BackResult{}, fmt.Errorf("%w: back: %w", ErrProtocol, err)
	}
	if res.To == "" {
		return BackResult{}, fmt.Errorf("%w: back: the answer names no pane", ErrProtocol)
	}
	return res, nil
}

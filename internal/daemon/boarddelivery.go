package daemon

import (
	"context"
	"fmt"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/store"
)

// A board-mode boss (ADR-001 A74): a tabs team whose notices.delivery is
// board has its boss's queue posted on its board while the agent last seen
// on its boss's pane is Claude, and held, never typed, until one has been
// seen since the daemon started.

// heldReason is why a board-mode boss's queue waits before its agent is
// known.
const heldReason = "messages wait: the boss's agent has not been seen since the daemon started (A74)"

// untyped serves to's queue off the typed path when to is the boss of a
// board-mode team (A74): posted on its board, or held while its agent is
// unknown. It reports whether it did; a typed boss, a name no team or two
// teams have, and every other recipient go on to targetOf.
//
// The delivery is read, and the queue posted, under in.route, the lock a
// poll's remember takes, as claimTyped claims (NERD-5533 7c30e8aa, C2): a
// pass that read board posts only while no poll has remembered another
// agent since, so a boss that turned Codex is never posted to. route is
// released before what the post logs, emits and wakes (posted), and before
// a held queue's warning. A recipient that is not a board-key boss takes
// no route: its delivery is typed.
func (d *dispatcher) untyped(ctx context.Context, bosses map[string]string, to store.Address) bool {
	project, board := d.boardBoss(bosses, to)
	if !board {
		return false
	}
	var (
		out store.PostedQueue
		err error
	)
	d.in.route.Lock()
	delivery := d.in.delivery(project)
	if delivery == notice.DeliverBoard {
		out, err = d.postQueued(ctx, to, project)
	}
	d.in.route.Unlock()
	switch delivery {
	case notice.DeliverBoard:
		d.posted(ctx, to, project, out, err)
		return true
	case notice.DeliverHeld:
		d.note(to, "held", heldReason, nil)
		return true
	case notice.DeliverTyped:
	}
	return false
}

// claimTyped claims to's next message to type, if it has one, as claim
// does. It claims nothing once to is the boss of a board-mode team that a
// poll has turned from typed since the pass decided to type it: the rest
// wait for the next pass's board path (A74; #183's review and PR 2's
// pre-check, NERD-5533 cbf65a5e).
//
// For the boss of one tabs team whose key is board, the delivery is read
// and the row claimed under in.route, the lock remember takes, so a poll's
// switch falls wholly before the claim, and nothing is claimed, or wholly
// after it, and the row is already dispatching, which A44 never appends to:
// a notice the poll flushes after it remembers Claude is never typed. route
// is released before the prompt. Lock order: route, then in.mu, then the
// store; route is taken nowhere else but remember, and neither holds
// another lock when it takes it.
//
// Every other recipient, and every boss with the key unset, is claimed as
// on the typed path alone, with no route taken.
func (d *dispatcher) claimTyped(ctx context.Context, bosses map[string]string, to store.Address) (store.Message, bool, error) {
	project, board := d.boardBoss(bosses, to)
	if !board {
		return d.claim(ctx, to)
	}
	d.in.route.Lock()
	defer d.in.route.Unlock()
	if d.in.delivery(project) != notice.DeliverTyped {
		return store.Message{}, false, nil
	}
	return d.claim(ctx, to)
}

// boardBoss is the project to is the boss of, and whether its key is board:
// false for any other recipient, a name no team or two teams have, and
// every boss with the key unset.
func (d *dispatcher) boardBoss(bosses map[string]string, to store.Address) (string, bool) {
	if to.Kind != store.KindBoss {
		return "", false
	}
	project, ok := bossProject(bosses, to.ID)
	return project, ok && d.in.boardKey(project)
}

// bossProject is the one configured project whose boss is named boss. It
// reports false for none, and for two: a name two teams share stays typed
// (A74).
func bossProject(bosses map[string]string, boss string) (string, bool) {
	found, n := "", 0
	for project, b := range bosses {
		if b == boss {
			found, n = project, n+1
		}
	}
	return found, n == 1
}

// postQueued posts the oldest MaxQueuePost rows of to's queue on project's
// board instead of typing them (A74), in one transaction that marks each
// row delivered, or failed where the board refuses it. Like claim, a stop
// does not cut it short, and it is bounded on its own: the store stops at
// half that budget, commits the rows it has posted and says more remain,
// so a pass that gets the connection always posts. untyped runs it under
// route.
func (d *dispatcher) postQueued(ctx context.Context, to store.Address, project string) (store.PostedQueue, error) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	out, err := d.st.PostQueued(sctx, to, project, d.now())
	if err != nil {
		return store.PostedQueue{}, fmt.Errorf("post the queue: %w", err)
	}
	return out, nil
}

// posted records how postQueued ended: it wakes the dispatcher again while
// more remain, so a backlog drains in passes back to back, and logs and
// emits each row posted. A post that failed leaves every row queued for
// the next pass, never typed, and is warned of once while it lasts.
func (d *dispatcher) posted(ctx context.Context, to store.Address, project string, out store.PostedQueue, err error) {
	ctx = context.WithoutCancel(ctx)
	if err != nil {
		d.note(to, "board", "messages wait: posting them to the board failed", err)
		return
	}
	delete(d.warned, to)
	if out.More {
		d.in.Wake()
	}
	if len(out.Posts) == 0 {
		return
	}
	ids := make([]string, 0, len(out.Delivered)+len(out.Failed))
	for _, m := range out.Delivered {
		ids = append(ids, m.ID)
		d.events.MessageDelivered(ctx, messageEvent(m, ""))
	}
	for _, m := range out.Failed {
		ids = append(ids, m.ID)
		d.events.MessageFailed(ctx, messageEvent(m, m.Reason))
	}
	seqs := make([]int64, 0, len(out.Posts))
	for _, p := range out.Posts {
		seqs = append(seqs, p.Seq)
	}
	d.log.Info().Str("recipient", to.ID).Str("project", project).Strs("messages", ids).Ints64("posts", seqs).
		Bool("more", out.More).Msg("messages: posted to the board")
}

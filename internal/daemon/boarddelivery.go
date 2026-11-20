package daemon

import (
	"context"

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
func (d *dispatcher) untyped(ctx context.Context, bosses map[string]string, to store.Address) bool {
	if to.Kind != store.KindBoss {
		return false
	}
	project, ok := bossProject(bosses, to.ID)
	if !ok {
		return false
	}
	switch d.in.delivery(project) {
	case notice.DeliverBoard:
		d.post(ctx, to, project)
		return true
	case notice.DeliverHeld:
		d.note(to, "held", heldReason, nil)
		return true
	case notice.DeliverTyped:
	}
	return false
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

// post posts the oldest MaxQueuePost rows of to's queue on project's board
// instead of typing them (A74), in one transaction that marks each row
// delivered, or failed where the board refuses it, and wakes the dispatcher
// again while more remain, so a backlog drains in passes back to back. Like
// claim, a stop does not cut it short, and it is bounded on its own. A post
// that fails leaves every row queued for the next pass, never typed, and is
// warned of once while it lasts.
func (d *dispatcher) post(ctx context.Context, to store.Address, project string) {
	ctx = context.WithoutCancel(ctx)
	sctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	out, err := d.st.PostQueued(sctx, to, project, d.now())
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

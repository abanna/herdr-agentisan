package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

const (
	opBossSnapshot  = "boss.snapshot"
	bossBoardBudget = time.Second
	bossPollMaxAge  = 3
	// bossClockSlack covers the wall clock's slew over bossBoardBudget (500 ppm).
	bossClockSlack = time.Millisecond
)

// bossPoll is the one state every poll publishes, one that built no model
// too: placement and rows come from it, never from the inbox's later view.
type bossPoll struct {
	at       time.Time
	seen     time.Duration // o.Mono when the poll began: ages never read the wall clock
	teams    map[string]snapshot.BossSnapshot
	model    *team.Model // the newest poll's; nil when it built none
	degraded bool        // teams and at are the last complete poll's, carried over
}

func publishBossPoll(cache *snapshotCache, m *team.Model, rows []store.Worker, at, observed time.Time, seen time.Duration, idle *team.IdleClock, started map[string]time.Time) {
	bindings := make(map[string]store.Worker, len(rows))
	for _, r := range rows {
		bindings[r.LogicalID] = r
	}
	next := &bossPoll{at: observed.UTC(), seen: seen, teams: make(map[string]snapshot.BossSnapshot, len(m.Projects)), model: m}
	for _, p := range m.Projects {
		next.teams[p.Name] = team.BossSnapshot(p, at, idle, started, bindings)
	}
	cache.boss.Store(next)
}

// carryBossPoll publishes m with the last complete poll's rows.
func carryBossPoll(cache *snapshotCache, m *team.Model) {
	prev := cache.boss.Load()
	cache.boss.Store(&bossPoll{at: prev.at, teams: prev.teams, model: m, degraded: true})
}

func bossPlaced(m *team.Model, project, pane, terminal string) bool {
	return m != nil && slices.ContainsFunc(m.Projects, func(p team.Project) bool {
		return p.Name == project && slices.ContainsFunc(p.Boss, func(b team.Worker) bool { return b.Pane.ID == pane && b.Pane.TerminalID == terminal })
	})
}

// bossSnapshotOp reuses board placement and restricts the read to the boss
// role. It has no board wait registry and reads only metadata (NERD-5748).
func (o Options) bossSnapshotOp(st *store.Store, cache *snapshotCache) op {
	return op{budget: opBudgets[opBossSnapshot], run: func(ctx context.Context, args json.RawMessage) (any, error) {
		if len(args) > 0 {
			var obj map[string]json.RawMessage
			if json.Unmarshal(args, &obj) != nil || obj == nil || len(obj) != 0 {
				return nil, bossRefusal(codeBadRequest)
			}
		}
		first := cache.boss.Load()
		who, err := o.boardPlacer(func() (*team.Model, map[string]string) { return first.model, nil })(ctx, peerFrom(ctx))
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, bossRefusal(codeTimeout)
			}
			return nil, bossRefusal(boardCode(err))
		}
		if who.Reader != store.ReaderBoss {
			return nil, bossRefusal(codeNotAllowed)
		}
		read, cancel := context.WithTimeout(ctx, bossBoardBudget)
		defer cancel()
		asOf, since := o.Now().UTC().Add(-bossClockSlack), o.Mono() // before the read, so no later post is older
		board, err := st.BoardStatus(read, who.Project, store.ReaderBoss)
		poll := cache.boss.Load() // after the read: a state published since placement is the one that counts
		out, ok := poll.teams[who.Project]
		if !ok || !bossPlaced(poll.model, who.Project, who.Pane, who.Term) || !o.paneShows(ctx, who.Pane, who.Term, who.Tab) {
			return nil, bossRefusal(codeNotReady) // demoted, no model or a reissued pane id: the client asks again
		}
		out.At = o.Now().UTC()
		out.Board.State = snapshot.Unavailable
		if err == nil && !out.At.Before(asOf.Add(o.Mono()-since)) { // else the wall clock stepped back during the read
			out.Board = snapshot.BossBoard{
				BossSource: snapshot.BossSource{AsOf: &asOf, State: snapshot.Fresh},
				Mark:       &board.Mark, Newest: &board.Newest, Unread: &board.Unread,
			}
		}
		out.TeamPoll = snapshot.BossSource{AsOf: &poll.at, State: snapshot.Fresh}
		if poll.degraded || o.Mono()-poll.seen > bossPollMaxAge*o.Poll {
			out.TeamPoll.State = snapshot.Stale
		}
		if err := out.Validate(); err != nil {
			return nil, bossRefusal(codeFailed)
		}
		raw, err := json.Marshal(out)
		if err != nil || len(raw) > snapshot.MaxBossBytes {
			return nil, bossRefusal(codeFailed)
		}
		return json.RawMessage(raw), nil
	}, code: boardCode}
}

// paneShows reports whether herdr lists pane now, showing terminal in tab: a pane id can be
// reissued, or the pane moved, before the poll sees it; the lineage that placed the caller is the pane's.
func (o Options) paneShows(ctx context.Context, pane, terminal, tab string) bool {
	c, _ := o.Herdr.(herdr.Client)
	panes, err := c.ListPanes(ctx)
	return err == nil && slices.ContainsFunc(panes, func(p herdr.PaneInfo) bool { return p.PaneID == pane && p.TerminalID == terminal && p.TabID == tab })
}

func bossRefusal(code string) *workforce.RefusalError {
	var message string
	switch code {
	case codeBadRequest:
		message = "boss snapshot takes empty args"
	case codeNotReady:
		message = "the team poll has no snapshot yet"
	case codeNotAllowed:
		message = "boss snapshot requires a team's boss place"
	case codeHerdrFailed:
		message = "could not place the caller"
	case codeTimeout:
		message = "caller placement timed out"
	default:
		code, message = codeFailed, "boss snapshot failed"
	}
	return &workforce.RefusalError{Code: code, Message: message}
}

// BossSnapshotOf requests the caller's boss snapshot and decodes v1.
func BossSnapshotOf(ctx context.Context, socket string) (snapshot.BossSnapshot, error) {
	raw, err := Workforce(ctx, socket, opBossSnapshot, struct{}{})
	switch {
	case errors.Is(err, ErrUnavailable):
		return snapshot.BossSnapshot{}, ErrUnavailable
	case errors.Is(err, ErrProtocol):
		return snapshot.BossSnapshot{}, ErrProtocol
	case err != nil:
		if refused, ok := errors.AsType[*RequestError](err); ok {
			safe := bossRefusal(refused.Code)
			return snapshot.BossSnapshot{}, &RequestError{Code: safe.Code, Message: safe.Message}
		}
		return snapshot.BossSnapshot{}, ErrRequest
	}
	s, err := snapshot.DecodeBoss(bytes.NewReader(raw))
	if err != nil {
		return snapshot.BossSnapshot{}, fmt.Errorf("%w: invalid boss snapshot reply", ErrProtocol)
	}
	return s, nil
}

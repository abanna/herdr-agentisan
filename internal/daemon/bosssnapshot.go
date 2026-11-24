package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

const (
	opBossSnapshot  = "boss.snapshot"
	bossBoardBudget = time.Second
	bossPollMaxAge  = 3
)

// bossPoll is published as a whole after a named, reconciled poll. Readers
// copy a single team's snapshot and attach its current board metadata.
type bossPoll struct {
	at     time.Time
	teams  map[string]snapshot.BossSnapshot
	bosses map[string][]string // each team's boss places in this projection (A60)
}

func publishBossPoll(cache *snapshotCache, m team.Model, rows []store.Worker, at, observed time.Time, idle *team.IdleClock, started map[string]time.Time) {
	bindings := make(map[string]store.Worker, len(rows))
	for _, r := range rows {
		bindings[r.LogicalID] = r
	}
	next := &bossPoll{at: observed.UTC(), teams: make(map[string]snapshot.BossSnapshot, len(m.Projects)), bosses: map[string][]string{}}
	for _, p := range m.Projects {
		next.teams[p.Name] = team.BossSnapshot(p, at, idle, started, bindings)
		for _, b := range p.Boss {
			next.bosses[p.Name] = append(next.bosses[p.Name], b.Pane.ID)
		}
	}
	cache.boss.Store(next)
}

// bossSnapshotOp reuses board placement and restricts the read to the boss
// role. It has no board wait registry and reads only metadata (NERD-5748).
func (o Options) bossSnapshotOp(st *store.Store, cache *snapshotCache, place placer) op {
	return op{budget: opBudgets[opBossSnapshot], run: func(ctx context.Context, args json.RawMessage) (any, error) {
		if len(args) > 0 {
			var obj map[string]json.RawMessage
			if json.Unmarshal(args, &obj) != nil || obj == nil || len(obj) != 0 {
				return nil, bossRefusal(codeBadRequest)
			}
		}
		who, err := place(ctx, peerFrom(ctx))
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, bossRefusal(codeTimeout)
			}
			return nil, bossRefusal(boardCode(err))
		}
		if who.Reader != store.ReaderBoss {
			return nil, bossRefusal(codeNotAllowed)
		}
		poll := cache.boss.Load()
		if poll == nil {
			return nil, bossRefusal(codeNotReady)
		}
		out, ok := poll.teams[who.Project]
		if !ok {
			return nil, bossRefusal(codeNotReady)
		}
		if !slices.Contains(poll.bosses[who.Project], who.Pane) {
			return nil, bossRefusal(codeNotAllowed)
		}

		read, cancel := context.WithTimeout(ctx, bossBoardBudget)
		defer cancel()
		asOf := o.Now().UTC() // before the read, so no later post is older
		board, err := st.BoardStatus(read, who.Project, store.ReaderBoss)
		out.At = o.Now().UTC()
		out.Board.State = snapshot.Unavailable
		if err == nil {
			out.Board = snapshot.BossBoard{
				BossSource: snapshot.BossSource{AsOf: &asOf, State: snapshot.Fresh},
				Mark:       &board.Mark, Newest: &board.Newest, Unread: &board.Unread,
			}
		}
		out.TeamPoll = snapshot.BossSource{AsOf: &poll.at, State: snapshot.Fresh}
		if age := out.At.Sub(poll.at); age < 0 || age > bossPollMaxAge*o.Poll {
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

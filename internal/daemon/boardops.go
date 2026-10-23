package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/report"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/team"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// The board ops (ADR-001 A60). The boss reads its team's board and waits
// for a post on it; the user's dashboard reads it.
// A worker never reads it: each op places its caller by the kernel's peer
// credentials (Peer), never by what its request says, and refuses any
// caller that is not a team's boss or the dashboard in its boss's tab. That
// is policy, not a boundary: workers run as the same user (A60).
const (
	OpBoardRead = "board.read"
	OpBoardWait = "board.wait"
)

const (
	// BoardBudget bounds a read: the caller's placement, at most
	// a pane.list and a pane.process_info a pane in a boss's tab, and one
	// transaction.
	BoardBudget = 10 * time.Second
	// DefaultBoardWait is how long a board wait waits when it is given no
	// timeout.
	DefaultBoardWait = 30 * time.Minute
	// MaxBoardWait is the longest timeout a board wait takes.
	MaxBoardWait = 6 * time.Hour
	// DefaultBoardLimit is how many posts a read returns when it is given
	// no limit.
	DefaultBoardLimit = 50
)

var (
	// ErrNotAllowed means a board op's caller is neither a team's boss nor
	// the user's dashboard in its boss's tab: workers may only post.
	ErrNotAllowed = errors.New("only a team's boss and the user's dashboard read the board; workers may only post")
	// ErrSuperseded means a later board wait from the same pane took this
	// one's place, so restarted waits never stack.
	ErrSuperseded = errors.New("a later board wait from the same pane took this one's place")
	// ErrStopping means the daemon stopped while a board wait waited.
	ErrStopping = errors.New("the daemon is stopping")
	// ErrCannotPlace means herdr could not be asked where a caller runs, so
	// it is refused as herdr_failed, not as not_allowed.
	ErrCannotPlace = errors.New("cannot read where the caller runs")
)

// The board ops' refusal codes.
const (
	codeNotAllowed = "not_allowed"
	codeSuperseded = "superseded"
	codeDaemonGone = "daemon_gone"
)

// BoardReadRequest is board.read's args: at most Limit posts,
// DefaultBoardLimit when 0. All reads the newest posts, newest first, and
// moves no mark, as the dashboard's panel does; otherwise the read is the
// caller's unread posts, oldest first, and moves its mark past them.
type BoardReadRequest struct {
	Limit int  `json:"limit,omitempty"`
	All   bool `json:"all,omitempty"`
}

// BoardPostView is a post as the board ops answer it.
type BoardPostView struct {
	Seq        int64     `json:"seq"`
	ID         string    `json:"id"`
	Channel    string    `json:"channel"`
	AuthorKind string    `json:"author_kind"`
	Author     string    `json:"author"`
	Kind       string    `json:"kind"`
	Subject    string    `json:"subject,omitempty"`
	Worker     string    `json:"worker,omitempty"`
	Body       string    `json:"body"`
	At         time.Time `json:"at"`
}

// BoardReadResult is board.read's answer: whose board it read, as which
// reader, the posts, how many unread are left after them (0 for All), and
// the reader's mark and the newest post's seq after the read.
type BoardReadResult struct {
	Project string          `json:"project"`
	Reader  string          `json:"reader"`
	Posts   []BoardPostView `json:"posts"`
	More    int             `json:"more"`
	Mark    int64           `json:"mark"`
	Newest  int64           `json:"newest"`
}

// BoardStateResult is board.wait's answer: where the caller stands on its
// team's board.
type BoardStateResult struct {
	Project string `json:"project"`
	Reader  string `json:"reader"`
	Mark    int64  `json:"mark"`
	Newest  int64  `json:"newest"`
	Unread  int    `json:"unread"`
	// TimedOut says no post came within the wait's timeout.
	TimedOut bool `json:"timed_out,omitempty"`
}

// BoardWaitRequest is board.wait's args: it answers once the newest post
// is past After, the caller's mark when nil, so a post made between a read
// and the next wait is never missed; or once Timeout passes,
// DefaultBoardWait when 0, at most MaxBoardWait.
type BoardWaitRequest struct {
	After   *int64        `json:"after,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

// boardCaller is where a board op's caller runs: the team whose board it
// reads, as which reader, and from which pane.
type boardCaller struct {
	Project string
	Reader  string
	Pane    string
}

// placer places a board op's caller.
type placer func(ctx context.Context, p Peer) (boardCaller, error)

// board serves the board ops on st.
type board struct {
	st    *store.Store
	place placer
	mu    sync.Mutex
	// waits holds each pane's board wait: a later one closes the earlier's.
	waits map[string]chan struct{}
}

// newBoard serves the board ops on st, each caller placed with place.
func newBoard(st *store.Store, place placer) *board {
	return &board{st: st, place: place, waits: map[string]chan struct{}{}}
}

// boardOps are the board ops on st, each caller placed with place.
func boardOps(st *store.Store, place placer) map[string]op { return newBoard(st, place).ops() }

func (b *board) ops() map[string]op {
	ops := map[string]op{
		OpBoardRead: {run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req BoardReadRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return b.read(ctx, req)
		}},
		OpBoardWait: {run: func(ctx context.Context, args json.RawMessage) (any, error) {
			var req BoardWaitRequest
			if err := decodeArgs(args, &req); err != nil {
				return nil, err
			}
			return b.wait(ctx, req)
		}},
	}
	for name, o := range ops {
		o.budget = opBudgets[name]
		o.code = boardCode
		ops[name] = o
	}
	return ops
}

// boardWaitBudget is a board wait's: its timeout, and an exchange more to
// tell it.
func boardWaitBudget(args json.RawMessage) time.Duration {
	var req BoardWaitRequest
	if json.Unmarshal(args, &req) != nil || req.Timeout <= 0 || req.Timeout > MaxBoardWait {
		return DefaultBoardWait + ioTimeout
	}
	return req.Timeout + ioTimeout
}

// boardCode is the refusal code for a board op's error.
func boardCode(err error) string {
	switch {
	case errors.Is(err, ErrNotAllowed):
		return codeNotAllowed
	case errors.Is(err, ErrSuperseded):
		return codeSuperseded
	case errors.Is(err, ErrStopping):
		return codeDaemonGone
	case errors.Is(err, ErrCannotPlace):
		return codeHerdrFailed
	}
	return workforceCode(err)
}

func (b *board) read(ctx context.Context, req BoardReadRequest) (BoardReadResult, error) {
	who, err := b.place(ctx, peerFrom(ctx))
	if err != nil {
		return BoardReadResult{}, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = DefaultBoardLimit
	}
	res := BoardReadResult{Project: who.Project, Reader: who.Reader}
	var posts []store.BoardPost
	if req.All {
		posts, err = b.st.LatestPosts(ctx, who.Project, limit)
	} else {
		var r store.BoardRead
		r, err = b.st.ReadBoard(ctx, who.Project, who.Reader, limit)
		posts, res.More = r.Posts, r.More
	}
	if err != nil {
		return BoardReadResult{}, fmt.Errorf("read %q's board: %w", who.Project, err)
	}
	st, err := b.st.BoardStatus(ctx, who.Project, who.Reader)
	if err != nil {
		return BoardReadResult{}, fmt.Errorf("read %q's board: %w", who.Project, err)
	}
	res.Mark, res.Newest = st.Mark, st.Newest
	res.Posts = make([]BoardPostView, 0, len(posts))
	for _, p := range posts {
		res.Posts = append(res.Posts, BoardPostView{
			Seq: p.Seq, ID: p.ID, Channel: p.Channel, AuthorKind: p.Author.Kind, Author: p.Author.ID,
			Kind: p.Kind, Subject: p.Subject, Worker: p.Worker, Body: p.Body, At: p.At,
		})
	}
	return res, nil
}

func (b *board) state(ctx context.Context, who boardCaller) (BoardStateResult, error) {
	st, err := b.st.BoardStatus(ctx, who.Project, who.Reader)
	if err != nil {
		return BoardStateResult{}, fmt.Errorf("read %q's board: %w", who.Project, err)
	}
	return BoardStateResult{Project: who.Project, Reader: who.Reader, Mark: st.Mark, Newest: st.Newest, Unread: st.Unread}, nil
}

// wait answers once a post past req.After is on the caller's board, at
// once if one is there already, or once req's timeout passes, counted from
// the op's start, so a slow placement still ends inside the op's budget. A
// later wait from the same pane ends it (ErrSuperseded), and so does the
// daemon stopping (ErrStopping): the server closes only once every op
// answered.
func (b *board) wait(ctx context.Context, req BoardWaitRequest) (BoardStateResult, error) {
	switch {
	case req.Timeout < 0 || req.Timeout > MaxBoardWait:
		return BoardStateResult{}, fmt.Errorf("%w: a timeout of %s: want up to %s", workforce.ErrBadRequest, req.Timeout, MaxBoardWait)
	case req.After != nil && *req.After < 0:
		return BoardStateResult{}, fmt.Errorf("%w: after %d", workforce.ErrBadRequest, *req.After)
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = DefaultBoardWait
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	who, err := b.place(ctx, peerFrom(ctx))
	if err != nil {
		return BoardStateResult{}, err
	}
	gone := b.join(who.Pane)
	defer b.leave(who.Pane, gone)
	for {
		changed := b.st.BoardChanged()
		st, err := b.state(ctx, who)
		if err != nil {
			return BoardStateResult{}, err
		}
		after := st.Mark
		if req.After != nil {
			after = *req.After
		}
		if st.Newest > after {
			return st, nil
		}
		select {
		case <-changed:
		case <-gone:
			return BoardStateResult{}, ErrSuperseded
		case <-timer.C:
			st.TimedOut = true
			return st, nil
		case <-ctx.Done():
			return BoardStateResult{}, fmt.Errorf("%w: %w", ErrStopping, ctx.Err())
		}
	}
}

// join makes pane's wait the one in force, ending the one before it.
func (b *board) join(pane string) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if old, ok := b.waits[pane]; ok {
		close(old)
	}
	ch := make(chan struct{})
	b.waits[pane] = ch
	return ch
}

// leave forgets pane's wait, if it is still ch.
func (b *board) leave(pane string, ch chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waits[pane] == ch {
		delete(b.waits, pane)
	}
}

// paneProber is what placing a caller asks herdr.
type paneProber interface {
	ListPanes(ctx context.Context) ([]herdr.PaneInfo, error)
	PaneProcessInfo(ctx context.Context, paneID string) (herdr.ProcessInfo, error)
}

// newBoardOps are the board ops on st, each caller placed in the team
// poll's last model (view) by its processes, as herdr's client and the
// OS's process table tell them (A60).
func (o Options) newBoardOps(st *store.Store, view func() (*team.Model, map[string]string)) map[string]op {
	c, _ := o.Herdr.(herdr.Client)
	return boardOps(st, placeBoardCaller(view, c, report.SystemStat(), os.Getuid()))
}

// placeBoardCaller places a caller (A60): in a team's boss place, it reads
// that team's board as the boss; in a pane of that place or of the boss's
// tab whose foreground runs the plugin's dashboard, as the user, since a ◆
// boss space holds its dashboard among the boss's panes. Anything else is
// ErrNotAllowed, a caller the kernel told nothing of or that runs as
// another user included: the check fails closed. Its process is matched to
// a pane by its lineage, read through stat with no anchors, since an
// anchor would come from the caller's own environment, which it writes.
func placeBoardCaller(view func() (*team.Model, map[string]string), c paneProber, stat report.StatFunc, uid int) placer {
	return func(ctx context.Context, p Peer) (boardCaller, error) {
		switch {
		case !p.Known || stat == nil:
			return boardCaller{}, fmt.Errorf("%w: nothing tells which process is asking", ErrNotAllowed)
		case int64(p.UID) != int64(uid):
			return boardCaller{}, fmt.Errorf("%w: the caller runs as another user", ErrNotAllowed)
		}
		m, _ := view()
		if m == nil {
			return boardCaller{}, fmt.Errorf("%w: the team poll has no model to place the caller in", ErrNotReady)
		}
		lineage := report.ReadLineage(p.PID, stat)
		var failed error
		type probed struct{ in, dashboard bool }
		probes := map[string]probed{}
		probe := func(pane string) probed {
			if got, ok := probes[pane]; ok {
				return got
			}
			var got probed
			if info, err := c.PaneProcessInfo(ctx, pane); err != nil {
				failed = fmt.Errorf("%w: pane %s's processes: %w", ErrCannotPlace, pane, err)
			} else {
				got = probed{in: lineage.RunsIn(info), dashboard: runsDashboard(info)}
			}
			probes[pane] = got
			return got
		}
		for _, pr := range m.Projects {
			for _, b := range pr.Boss {
				switch got := probe(b.Pane.ID); {
				case got.in && got.dashboard:
					return boardCaller{Project: pr.Name, Reader: store.ReaderUser, Pane: b.Pane.ID}, nil
				case got.in:
					return boardCaller{Project: pr.Name, Reader: store.ReaderBoss, Pane: b.Pane.ID}, nil
				}
			}
		}
		panes, err := c.ListPanes(ctx)
		if err != nil {
			return boardCaller{}, fmt.Errorf("%w: the panes: %w", ErrCannotPlace, err)
		}
		for _, pr := range m.Projects {
			for _, b := range pr.Boss {
				for _, pane := range panes {
					if b.Pane.TabID == "" || pane.TabID != b.Pane.TabID {
						continue
					}
					if got := probe(pane.PaneID); got.in && got.dashboard {
						return boardCaller{Project: pr.Name, Reader: store.ReaderUser, Pane: pane.PaneID}, nil
					}
				}
			}
		}
		if failed != nil {
			return boardCaller{}, failed
		}
		return boardCaller{}, fmt.Errorf("%w: the caller runs in no team's boss place, nor in the dashboard in a boss's tab", ErrNotAllowed)
	}
}

// runsDashboard reports whether a pane's foreground runs the plugin's
// dashboard, as its manifest's pane and team up start it.
func runsDashboard(info herdr.ProcessInfo) bool {
	return slices.ContainsFunc(info.ForegroundProcesses, func(p herdr.Process) bool {
		return len(p.Argv) >= 2 && filepath.Base(p.Argv[0]) == "herdr-agentisan" && p.Argv[1] == "dashboard"
	})
}

package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// BoardSource reads the latest posts without moving a mark, then marks what
// the panel showed. It must honor context cancellation.
type BoardSource interface {
	Board(context.Context) (daemon.BoardReadResult, error)
	Seen(ctx context.Context, upto int64) error
}

// DaemonBoard reads the board placed by the daemon from the caller's process.
// It sends no caller-selected project or reader.
type DaemonBoard struct{ Socket string }

// Board fetches the newest 200 posts without moving the user mark.
func (b DaemonBoard) Board(ctx context.Context) (daemon.BoardReadResult, error) {
	raw, err := daemon.Workforce(ctx, b.Socket, daemon.OpBoardRead, daemon.BoardReadRequest{All: true, Limit: 200})
	if err != nil {
		return daemon.BoardReadResult{}, fmt.Errorf("board: %w", err)
	}
	var r daemon.BoardReadResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("%w: %w", ErrBoard, err)
	}
	return r, nil
}

// Seen advances only the daemon-placed reader's mark.
func (b DaemonBoard) Seen(ctx context.Context, upto int64) error {
	_, err := daemon.Workforce(ctx, b.Socket, daemon.OpBoardSeen, daemon.BoardSeenRequest{Upto: upto})
	if err != nil {
		return fmt.Errorf("board seen: %w", err)
	}
	return nil
}

// ErrBoard means a board fixture or answer could not be read.
var ErrBoard = errors.New("cannot read board")

// FileBoard reads a bounded, strict board.read fixture. Its user mark is
// kept in memory; neither Seen nor a snapshot refresh writes the fixture.
type FileBoard struct {
	Path string
	mu   sync.Mutex
	mark int64
}

const maxBoardFixture = 2 << 20

// Board rereads the fixture and overlays the in-memory mark.
func (b *FileBoard) Board(ctx context.Context) (daemon.BoardReadResult, error) {
	if err := ctx.Err(); err != nil {
		return daemon.BoardReadResult{}, fmt.Errorf("%w: %w", ErrBoard, err)
	}
	f, err := os.OpenFile(b.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return daemon.BoardReadResult{}, fmt.Errorf("%w: %w", ErrBoard, err)
	}
	defer f.Close() //nolint:errcheck // read only
	st, err := f.Stat()
	if err != nil {
		return daemon.BoardReadResult{}, fmt.Errorf("%w: %w", ErrBoard, err)
	}
	if !st.Mode().IsRegular() || st.Size() > maxBoardFixture {
		return daemon.BoardReadResult{}, fmt.Errorf("%w: want a regular file up to %d bytes", ErrBoard, maxBoardFixture)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBoardFixture+1))
	if err != nil {
		return daemon.BoardReadResult{}, fmt.Errorf("%w: %w", ErrBoard, err)
	}
	var r daemon.BoardReadResult
	if len(raw) > maxBoardFixture {
		return r, fmt.Errorf("%w: fixture too large", ErrBoard)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("%w: %w", ErrBoard, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return r, fmt.Errorf("%w: trailing JSON", ErrBoard)
	}
	if r.Mark < 0 || r.Newest < r.Mark || len(r.Posts) > 200 {
		return r, fmt.Errorf("%w: invalid mark or post count", ErrBoard)
	}
	last := int64(0)
	for _, p := range r.Posts {
		if p.Seq <= 0 || (last > 0 && p.Seq >= last) || p.Seq > r.Newest || (p.Channel != "team" && p.Channel != "daemon") {
			return r, fmt.Errorf("%w: invalid post order or channel", ErrBoard)
		}
		last = p.Seq
	}
	b.mu.Lock()
	r.Mark = max(r.Mark, b.mark)
	b.mu.Unlock()
	return r, nil
}

// Seen ratchets the fixture's in-memory mark, bounded by its newest post.
func (b *FileBoard) Seen(ctx context.Context, upto int64) error {
	if upto < 0 {
		return fmt.Errorf("%w: negative mark", ErrBoard)
	}
	r, err := b.Board(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.mark = max(b.mark, min(upto, r.Newest))
	b.mu.Unlock()
	return nil
}

type fixtureBoardSource struct {
	snapshot.Source
	board *FileBoard
}

func (s fixtureBoardSource) Snapshot(ctx context.Context) (snapshot.Snapshot, error) {
	snap, err := s.Source.Snapshot(ctx)
	if err != nil {
		return snap, fmt.Errorf("snapshot fixture: %w", err)
	}
	r, err := s.board.Board(ctx)
	if err != nil {
		return snap, err
	}
	if snap.Project == nil {
		snap.Project = &snapshot.Project{}
	}
	snap.Project.BoardUnread = 0
	for _, p := range r.Posts {
		if p.Seq > r.Mark {
			snap.Project.BoardUnread++
		}
	}
	return snap, nil
}

// BoardFrame is the panel's visible state. OpenMark stays fixed until close,
// keeping dots on posts that were new when this opening first read them.
type BoardFrame struct {
	Open, Ready, Expanded, Available bool
	Posts                            []daemon.BoardPostView
	Mark, OpenMark                   int64
	Selected                         int
	Filter                           string
	Err                              error
}

func (b BoardFrame) posts() []daemon.BoardPostView {
	out := make([]daemon.BoardPostView, 0, len(b.Posts))
	for _, p := range b.Posts {
		if b.Filter == "" || p.Channel == b.Filter {
			out = append(out, p)
		}
	}
	return out
}

type (
	boardPanel struct {
		BoardFrame
		gen, serial uint64
		busy        bool
	}
	boardReply struct {
		gen, serial uint64
		read        *daemon.BoardReadResult
		upto        int64
		err         error
	}
	boardTick struct{ gen, serial uint64 }
)

const (
	boardInterval = 3 * time.Second
	boardTimeout  = 5 * time.Second
)

func (m *Model) openBoard() tea.Cmd {
	m.board.gen++
	busy, serial, gen := m.board.busy, m.board.serial, m.board.gen
	m.board = boardPanel{BoardFrame: BoardFrame{Open: true, Available: m.cfg.Board != nil}, busy: busy, serial: serial, gen: gen}
	m.help = false
	return m.boardRead()
}
func (m *Model) closeBoard() { m.board.Open = false; m.board.Err = nil; m.help = false; m.settle() }
func (m *Model) boardRead() tea.Cmd {
	if !m.board.Open || m.board.busy || m.cfg.Board == nil {
		return nil
	}
	return m.boardCall(0)
}

func (m *Model) boardCall(upto int64) tea.Cmd {
	m.board.serial++
	m.board.busy = true
	gen, serial, src, ctx := m.board.gen, m.board.serial, m.cfg.Board, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, boardTimeout)
		defer cancel()
		msg := boardReply{gen: gen, serial: serial, upto: upto}
		if upto > 0 {
			msg.err = src.Seen(ctx, upto)
		} else {
			r, err := src.Board(ctx)
			msg.read = &r
			msg.err = err
		}
		return msg
	}
}

func (m *Model) boardNext() tea.Cmd {
	if !m.board.Open || m.board.busy || m.cfg.Board == nil {
		return nil
	}
	if m.board.Ready && m.board.Err == nil {
		posts := m.board.posts()
		if len(posts) > 0 && posts[0].Seq > m.board.Mark {
			return m.boardCall(posts[0].Seq)
		}
	}
	gen, serial := m.board.gen, m.board.serial
	return tea.Tick(boardInterval, func(time.Time) tea.Msg { return boardTick{gen: gen, serial: serial} })
}

func (m *Model) boardAnswer(msg boardReply) tea.Cmd {
	if msg.serial != m.board.serial || !m.board.busy {
		return nil
	}
	m.board.busy = false
	if !m.board.Open {
		return nil
	}
	if msg.gen != m.board.gen {
		return m.boardRead()
	}
	m.board.Err = msg.err
	if msg.err != nil {
		return m.boardNext()
	}
	if msg.read == nil {
		m.board.Mark = max(m.board.Mark, msg.upto)
		return m.boardNext()
	}
	m.boardLoaded(*msg.read)
	return m.boardNext()
}

func (m *Model) boardLoaded(r daemon.BoardReadResult) {
	if !m.board.Ready {
		m.board.OpenMark = r.Mark
	}
	var seq int64
	old := m.board.posts()
	if m.board.Selected < len(old) {
		seq = old[m.board.Selected].Seq
	}
	m.board.Posts = r.Posts
	m.board.Mark = r.Mark
	m.board.Ready = true
	for i, p := range m.board.posts() {
		if p.Seq == seq {
			m.board.Selected = i
			break
		}
	}
	m.board.Selected = min(m.board.Selected, max(0, len(m.board.posts())-1))
}

func (m Model) boardKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k := msg.String(); k {
	case "q", keyQuit:
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "b":
		m.closeBoard()
	case keyEsc:
		switch {
		case m.help:
			m.help = false
		case m.board.Expanded:
			m.board.Expanded = false
		default:
			m.closeBoard()
		}
	case keyUp:
		m.board.Selected = max(0, m.board.Selected-1)
		m.board.Expanded = false
	case keyDown:
		m.board.Selected = min(max(0, len(m.board.posts())-1), m.board.Selected+1)
		m.board.Expanded = false
	case keyEnter:
		m.board.Expanded = !m.board.Expanded
	case "t", "d":
		filter := "team"
		if k == "d" {
			filter = "daemon"
		}
		if m.board.Filter == filter {
			filter = ""
		}
		m.board.Filter = filter
		m.board.Selected = 0
		m.board.Expanded = false
		return m, m.boardNext()
	}
	return m, nil
}

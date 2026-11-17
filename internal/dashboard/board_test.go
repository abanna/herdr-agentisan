package dashboard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	tea "charm.land/bubbletea/v2"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/stretchr/testify/require"
)

type boardSource struct {
	reads, marks int
	upto         int64
	result       daemon.BoardReadResult
	err          error
}

func (b *boardSource) Board(context.Context) (daemon.BoardReadResult, error) {
	b.reads++
	return b.result, b.err
}

func (b *boardSource) Seen(_ context.Context, upto int64) error {
	b.marks++
	b.upto = upto
	return b.err
}

func boardKey(t *testing.T, m dashboard.Model, key string) (dashboard.Model, tea.Cmd) {
	t.Helper()
	code := rune(key[0])
	if key == "enter" {
		code = tea.KeyEnter
	}
	if key == "esc" {
		code = tea.KeyEscape
	}
	next, cmd := m.Update(tea.KeyPressMsg{Code: code, Text: key})
	return next.(dashboard.Model), cmd
}

func TestBoardSingleFlightAndStaleAnswers(t *testing.T) {
	b := &boardSource{result: daemon.BoardReadResult{Mark: 1, Newest: 3, Posts: []daemon.BoardPostView{{Seq: 3, Channel: "daemon", Body: "notice"}, {Seq: 2, Channel: "team", Body: "post"}}}}
	m := dashboard.New(t.Context(), dashboard.Config{Board: b})
	m, read := boardKey(t, m, "b")
	require.NotNil(t, read)
	_, ignored := boardKey(t, m, "t")
	require.Nil(t, ignored)
	answer := read()
	require.Equal(t, 1, b.reads)
	next, seen := m.Update(answer)
	m = next.(dashboard.Model)
	require.NotNil(t, seen)
	require.True(t, m.Frame().Board.Open)
	require.EqualValues(t, 1, m.Frame().Board.OpenMark)
	reply := seen()
	require.EqualValues(t, 3, b.upto)
	next, tick := m.Update(reply)
	m = next.(dashboard.Model)
	require.NotNil(t, tick)
	m, _ = boardKey(t, m, "b")
	require.False(t, m.Frame().Board.Open)
	_, stale := m.Update(reply)
	require.Nil(t, stale)
	m, read = boardKey(t, m, "b")
	require.NotNil(t, read)
	_, stale = m.Update(answer)
	require.Nil(t, stale)
}

func TestBoardFiltersAndKeys(t *testing.T) {
	b := &boardSource{result: daemon.BoardReadResult{Mark: 1, Newest: 4, Posts: []daemon.BoardPostView{{Seq: 4, Channel: "daemon"}, {Seq: 3, Channel: "team"}, {Seq: 2, Channel: "daemon"}}}}
	m := dashboard.New(t.Context(), dashboard.Config{Board: b})
	m, read := boardKey(t, m, "b")
	m, ignored := boardKey(t, m, "t")
	require.Nil(t, ignored)
	next, seen := m.Update(read())
	m = next.(dashboard.Model)
	require.NotNil(t, seen)
	reply := seen()
	require.EqualValues(t, 3, b.upto)
	next, _ = m.Update(reply)
	m = next.(dashboard.Model)
	m, seen = boardKey(t, m, "d")
	require.NotNil(t, seen)
	reply = seen()
	require.EqualValues(t, 4, b.upto)
	next, _ = m.Update(reply)
	m = next.(dashboard.Model)
	m, _ = boardKey(t, m, "enter")
	require.True(t, m.Frame().Board.Expanded)
	m, _ = boardKey(t, m, "?")
	m, _ = boardKey(t, m, "esc")
	require.False(t, m.Frame().Help)
	require.True(t, m.Frame().Board.Expanded)
	m, _ = boardKey(t, m, "esc")
	require.True(t, m.Frame().Board.Open)
	require.False(t, m.Frame().Board.Expanded)
	m, _ = boardKey(t, m, "1")
	require.Empty(t, m.Frame().Group)
	m, _ = boardKey(t, m, "/")
	require.False(t, m.Frame().Search)
	m, _ = boardKey(t, m, "esc")
	require.False(t, m.Frame().Board.Open)
}

func TestBoardFixtureAndBadge(t *testing.T) {
	b := &dashboard.FileBoard{Path: "../../docs/dashboard/sample-board.json"}
	r, err := b.Board(t.Context())
	require.NoError(t, err)
	require.Len(t, r.Posts, 16)
	cfg, err := dashboard.FixtureConfig(t.Context(), "testdata/full.json", b.Path, "")
	require.NoError(t, err)
	s, err := cfg.Source.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, s.Project.BoardUnread)
	require.NoError(t, cfg.Board.Seen(t.Context(), r.Newest))
	s, err = cfg.Source.Snapshot(t.Context())
	require.NoError(t, err)
	require.Zero(t, s.Project.BoardUnread)
	require.NoError(t, b.Seen(t.Context(), 999))
	r, err = b.Board(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 42, r.Mark)
	require.NoError(t, b.Seen(t.Context(), 1))
	r, err = b.Board(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 42, r.Mark)
	require.ErrorIs(t, b.Seen(t.Context(), -1), dashboard.ErrBoard)
}

func TestBoardFixtureValidation(t *testing.T) {
	tests := map[string]string{"empty": "", "unknown": `{"other":1}`, "trailing": `{} {}`, "negative": `{"mark":-1}`, "order": `{"newest":2,"posts":[{"seq":1,"channel":"team"},{"seq":2,"channel":"team"}]}`, "channel": `{"newest":1,"posts":[{"seq":1,"channel":"other"}]}`, "deep": strings.Repeat("[", 11000), "too large": strings.Repeat(" ", 2<<20+1)}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "board.json")
			require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
			_, err := (&dashboard.FileBoard{Path: p}).Board(t.Context())
			require.ErrorIs(t, err, dashboard.ErrBoard)
		})
	}
	for _, p := range []string{"", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		_, err := (&dashboard.FileBoard{Path: p}).Board(t.Context())
		require.ErrorIs(t, err, dashboard.ErrBoard)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := (&dashboard.FileBoard{Path: "unopened"}).Board(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestBoardGolden(t *testing.T) {
	b := &dashboard.FileBoard{Path: "../../docs/dashboard/sample-board.json"}
	r, err := b.Board(t.Context())
	require.NoError(t, err)
	for _, tc := range []struct {
		name, filter string
		w, h         int
		expanded     bool
	}{{"expanded", "", 97, 40, true}, {"team", "team", 80, 24, false}, {"daemon", "daemon", 60, 20, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := dashboard.Frame{Snapshot: fixture(t, "full"), Now: time.Date(2026, 10, 23, 17, 19, 0, 0, time.UTC), Board: dashboard.BoardFrame{Open: true, Available: true, Ready: true, Posts: r.Posts, OpenMark: r.Mark, Selected: 1, Expanded: tc.expanded, Filter: tc.filter}}
			f.Snapshot.At = f.Now
			f.Snapshot.Project.BoardUnread = 3
			golden.RequireEqual(t, dashboard.Render(f, tc.w, tc.h, plain)+"\n")
		})
	}
}

// boardHeights and boardWidths are the sizes TestBoardStatesAndGeometry
// draws. Its checks are exactly h lines, none wider than w, and Render
// builds h lines and ends each with cut(0, w), so only an index or a
// strings.Repeat count gone out of range, or a broken last cut, can fail
// them. With no posts the board's rows are its title, its top border, one
// state message and its bottom border. The sizes are each side of the
// thresholds where an index or a count can go wrong:
//
//   - h 1: the project line alone, no footer, no room for the board.
//   - h 2: the footer too, still no room.
//   - h 3: one header line, room 1: the bottom border replaces the only row.
//   - h 4, 5: two bare header lines, room 1 and 2.
//   - h 8: bare, room 5, one more than the rows: the loop stops at their end.
//   - h 9, 10, 11: boxedHeight, a boxed header, room 3, 4 and 5: one row
//     short, the rows exactly, one row more.
//   - h 40: far taller than the rows.
//
// Heights 6 and 7 are room 3 and 4 under a bare header, the board's path at
// 9 and 10; 12 to 39 add only blank lines.
//
//   - w 1, 2: a border's w-2 dashes below zero and at zero (the board's and
//     the boxed header's).
//   - w 3, 4, 5: rowWidth (w-4) below zero, at zero and one cell, for the
//     state message; the boxed header's text (w-4) the same.
//   - w 9, 10 and 11, 12: the boss line's room (its text less the clock and
//     a gap) at 0 and 1, bare (text w-2) and boxed (text w-4).
//   - w 15, 16; 26, 27; 29, 30; 64, 65: each state message cut and whole in
//     its box: "no posts yet" 12 cells, "waiting for the daemon…" 23, "no
//     board with this fixture" 26, the refusal 61.
//   - w 47, 48: the board's title, 48 cells, cut by the last cut and whole.
//   - w 72, 73: the footer's 72 cells cut to w-1 and whole.
//   - w 43, 45, 59, 61, 68, 70, 79, 81 (and 73): for the full fixture, bare
//     and boxed, the project line's count (text 41) and compact PRs (71)
//     and each of the boss line's drop levels (text 42, 57, 66, 77).
//     Nothing there can go out of range, since each is cut to its text, but
//     each level is drawn once in each header.
//   - w 100: the widest the sweep drew before.
var (
	boardHeights = []int{1, 2, 3, 4, 5, 8, 9, 10, 11, 40}
	boardWidths  = []int{1, 2, 3, 4, 5, 9, 10, 11, 12, 15, 16, 26, 27, 29, 30, 43, 45, 47, 48, 59, 61, 64, 65, 66, 68, 70, 72, 73, 74, 79, 81, 100}
)

// boardPostHeights and boardPostWidths are the sizes of the states that draw
// posts. Their rows are the title, the top border, a row for each post
// (and a row for each wrapped line of the expanded one) and the bottom
// border, so the board overflows its room at most heights and the panel
// scrolls. Every height from 1 to 24 puts room on each side of the rows'
// end and of the scroll limit, and 40 holds them all. The widths are each
// side of boardPrefix's two thresholds, which it reads from rowWidth, the
// width less the box's 4 cells: kind and the wider name from a row width of
// 62, w 66, and channel from 70, w 74. The rest are the box's few cells,
// each prefix column dropping out, the wrapped body down to one cell, and
// 100.
var (
	boardPostHeights = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 40}
	boardPostWidths  = []int{1, 2, 3, 4, 5, 9, 13, 17, 21, 25, 31, 45, 65, 66, 73, 74, 100}
)

// boardSweepPosts is ten posts that between them reach every branch of the
// row's prefix: each of worker, subject and author as the name, a name
// longer than either column, a time that is today's, another day's and
// zero, both channels, a read and an unread mark, a kind longer than its
// column, and bodies of wide clusters and of several long lines.
func boardSweepPosts() []daemon.BoardPostView {
	long := "a body of several words that has to wrap at most of the widths this sweep draws it at, so that its continuation lines are drawn too"
	return []daemon.BoardPostView{
		{Seq: 10, At: at, Channel: "daemon", Author: "daemon", Kind: "notice", Body: "merged\n" + long},
		{Seq: 9, At: at.Add(-time.Hour), Channel: "team", Author: "boss", Worker: "coder-1", Kind: "post", Body: long},
		{Seq: 8, At: at.Add(-48 * time.Hour), Channel: "team", Author: "boss", Subject: "NERD-5639 and a subject longer than the name column", Kind: "a kind longer than its column", Body: "界界界界界界界界界界界界界界界界界界界界界👩\u200d💻👩\u200d💻👩\u200d💻👩\u200d💻👩\u200d💻界界界界界界界界"},
		{Seq: 7, Channel: "daemon", Author: "daemon", Kind: "notice", Body: "no time\n\n\nand blank lines"},
		{Seq: 6, At: at, Channel: "team", Author: "qa", Kind: "review", Body: "read: " + long + "\n" + long},
		{Seq: 5, At: at, Channel: "team", Author: "qa", Kind: "review", Body: "x"},
		{Seq: 4, At: at, Channel: "daemon", Author: "daemon", Kind: "notice", Body: ""},
		{Seq: 3, At: at, Channel: "team", Author: "coder-2", Kind: "post", Body: long},
		{Seq: 2, At: at, Channel: "daemon", Author: "daemon", Kind: "notice", Body: "second to last"},
		{Seq: 1, At: at, Channel: "team", Author: "coder-1", Kind: "post", Body: "last post"},
	}
}

func TestBoardStatesAndGeometry(t *testing.T) {
	t.Parallel()
	posts := boardSweepPosts()
	ready := func(b dashboard.BoardFrame) dashboard.BoardFrame {
		b.Open, b.Available, b.Ready, b.Posts, b.OpenMark = true, true, true, posts, 6
		return b
	}
	for _, tc := range []struct {
		name          string
		b             dashboard.BoardFrame
		want          string
		widths, highs []int
	}{
		{"empty", dashboard.BoardFrame{Open: true, Ready: true}, "no posts yet", boardWidths, boardHeights},
		{"missing", dashboard.BoardFrame{Open: true}, "no board with this fixture", boardWidths, boardHeights},
		{"waiting", dashboard.BoardFrame{Open: true, Available: true}, "waiting for the daemon", boardWidths, boardHeights},
		{"refused", dashboard.BoardFrame{Open: true, Err: daemon.ErrNotAllowed}, "the board reads only", boardWidths, boardHeights},
		{"restart", dashboard.BoardFrame{Open: true, Err: daemon.ErrNotReady}, "waiting for the daemon", boardWidths, boardHeights},
		// The first row selected, then the last: the offset's two limits.
		{"posts_first", ready(dashboard.BoardFrame{}), "last post", boardPostWidths, boardPostHeights},
		{"posts_last", ready(dashboard.BoardFrame{Selected: 9}), "last post", boardPostWidths, boardPostHeights},
		// An expansion scrolled inside its limit, past it and below zero.
		{"expanded_scrolled", ready(dashboard.BoardFrame{Selected: 4, Expanded: true, Scroll: 2}), "last post", boardPostWidths, boardPostHeights},
		{"expanded_past_the_end", ready(dashboard.BoardFrame{Selected: 9, Expanded: true, Scroll: 1000}), "last post", boardPostWidths, boardPostHeights},
		{"expanded_below_zero", ready(dashboard.BoardFrame{Selected: 1, Expanded: true, Scroll: -5}), "last post", boardPostWidths, boardPostHeights},
		// A filter that leaves fewer posts than Selected names.
		{"filtered_selection_gone", ready(dashboard.BoardFrame{Selected: 9, Filter: "daemon", Expanded: true, Scroll: 3}), "second to last", boardPostWidths, boardPostHeights},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Board: tc.b}
			require.Contains(t, dashboard.Render(f, 97, 40, plain), tc.want)
			for _, w := range tc.widths {
				for _, h := range tc.highs {
					out := dashboard.Render(f, w, h, plain)
					lines := strings.Split(out, "\n")
					require.Len(t, lines, h)
					for _, l := range lines {
						require.LessOrEqual(t, ansi.StringWidth(l), w)
					}
				}
			}
		})
	}
}

func TestBoardSafeTextAndTime(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 2, 0, 0, time.FixedZone("east", 2*3600))
	cases := map[string]struct {
		at   time.Time
		want string
	}{"zero": {time.Time{}, "--:--"}, "local today": {now.UTC(), "00:02"}, "midnight": {now.Add(-3 * time.Minute), "12-31"}, "future": {now.Add(24 * time.Hour), "01-02"}}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := daemon.BoardPostView{Seq: 1, At: tc.at, Author: "author", Kind: "long_kind_name", Body: "safe\xff\r\n\x1b[31m\x00\u0085\ufeff\u202e界👩\u200d💻\nsecond line"}
			f := dashboard.Frame{Now: now, Board: dashboard.BoardFrame{Open: true, Ready: true, Expanded: true, Posts: []daemon.BoardPostView{p}}}
			out := dashboard.Render(f, 97, 20, plain)
			require.Contains(t, out, tc.want)
			require.Contains(t, out, "界👩\u200d💻")
			require.Contains(t, out, "second line")
			require.NotContains(t, out, "\x1b")
			require.NotContains(t, out, "\u202e")
			require.NotContains(t, out, "\ufeff")
			require.NotContains(t, out, "\u0085")
		})
	}
}

func TestBoardClicksAndReturnToGroup(t *testing.T) {
	b := &boardSource{result: daemon.BoardReadResult{Posts: []daemon.BoardPostView{{Seq: 1, Channel: "team", Body: "post"}}}}
	m := dashboard.New(t.Context(), dashboard.Config{Board: b})
	// The normal snapshot path supplies the overview before opening a group.
	cfg, err := dashboard.FixtureConfig(t.Context(), "testdata/full.json", "", "")
	require.NoError(t, err)
	cfg.Board = b
	m = dashboard.New(t.Context(), cfg)
	next, _ := m.Update(m.Init()())
	m = next.(dashboard.Model)
	m, _ = boardKey(t, m, "1")
	group := m.Frame().Group
	require.NotEmpty(t, group)
	m, read := boardKey(t, m, "b")
	next, _ = m.Update(read())
	m = next.(dashboard.Model)
	next, cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 10, Y: 7})
	m = next.(dashboard.Model)
	require.Nil(t, cmd)
	require.True(t, m.Frame().Board.Open)
	next, cmd = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 5})
	m = next.(dashboard.Model)
	require.Nil(t, cmd)
	require.False(t, m.Frame().Board.Open)
	require.Equal(t, group, m.Frame().Group)
	m, _ = boardKey(t, m, "/")
	m, _ = boardKey(t, m, "b")
	require.True(t, m.Frame().Search)
	require.Equal(t, "b", m.Frame().Query)
}

func TestBoardReopenWaitsForInflight(t *testing.T) {
	t.Parallel()
	b := &boardSource{result: daemon.BoardReadResult{Newest: 2, Posts: []daemon.BoardPostView{{Seq: 2, Channel: "team"}}}}
	m := dashboard.New(t.Context(), dashboard.Config{Board: b})
	m, old := boardKey(t, m, "b")
	m, _ = boardKey(t, m, "b")
	m, cmd := boardKey(t, m, "b")
	require.Nil(t, cmd)
	next, read := m.Update(old())
	m = next.(dashboard.Model)
	require.NotNil(t, read)
	require.False(t, m.Frame().Board.Ready)
	next, seen := m.Update(read())
	m = next.(dashboard.Model)
	require.True(t, m.Frame().Board.Ready)
	reply := seen()
	next, tick := m.Update(reply)
	m = next.(dashboard.Model)
	b.err = errors.New("offline")
	next, read = m.Update(tick())
	m = next.(dashboard.Model)
	require.NotNil(t, read)
	next, _ = m.Update(read())
	m = next.(dashboard.Model)
	require.Len(t, m.Frame().Board.Posts, 1)
	require.Error(t, m.Frame().Board.Err)
	m, _ = boardKey(t, m, "b")
	require.NoError(t, m.Frame().Board.Err)
}

func TestDaemonBoard(t *testing.T) {
	for name, reply := range map[string]string{"ok": `{"ok":true,"data":{"mark":2,"newest":3,"posts":[]}}` + "\n", "refused": `{"ok":false,"error":{"code":"not_allowed","message":"refused"}}` + "\n", "bad result": `{"ok":true,"data":7}` + "\n"} {
		t.Run(name, func(t *testing.T) {
			sock := filepath.Join(shortDir(t), "board.sock")
			fakeDaemon(t, sock, reply)
			b := dashboard.DaemonBoard{Socket: sock}
			r, err := b.Board(t.Context())
			switch name {
			case "ok":
				require.NoError(t, err)
				require.EqualValues(t, 2, r.Mark)
				require.NoError(t, b.Seen(t.Context(), 3))
			case "refused":
				require.ErrorIs(t, err, daemon.ErrNotAllowed)
				require.ErrorIs(t, b.Seen(t.Context(), 3), daemon.ErrNotAllowed)
			default:
				require.ErrorIs(t, err, dashboard.ErrBoard)
			}
		})
	}
}

func TestBoardFixturePathsAndLimits(t *testing.T) {
	raw, err := os.ReadFile("../../docs/dashboard/sample-board.json")
	require.NoError(t, err)
	dir := t.TempDir()
	for _, name := range []string{"spaces and 界.json", "-leading.json", "embedded\nnewline.json", "bad-byte-\xff.json", "Café.json", "Cafe\u0301.json", "secret-note.json", "CON", "Case.json", "case.json"} {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, raw, 0o600))
		_, err := (&dashboard.FileBoard{Path: p}).Board(t.Context())
		require.NoError(t, err)
	}
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, raw, 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))
	_, err = (&dashboard.FileBoard{Path: link}).Board(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.Remove(target))
	_, err = (&dashboard.FileBoard{Path: link}).Board(t.Context())
	require.ErrorIs(t, err, dashboard.ErrBoard)
	p := filepath.Join(dir, "limit")
	require.NoError(t, os.WriteFile(p, append(raw, bytes.Repeat([]byte(" "), (2<<20)-len(raw))...), 0o600))
	_, err = (&dashboard.FileBoard{Path: p}).Board(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(p, 0))
	_, err = (&dashboard.FileBoard{Path: p}).Board(t.Context())
	require.ErrorIs(t, err, os.ErrPermission)
}

func TestBoardFixtureNodesAndConcurrentMarks(t *testing.T) {
	b := &dashboard.FileBoard{Path: "../../docs/dashboard/sample-board.json"}
	var wg sync.WaitGroup
	errs := make(chan error, 13)
	for i := int64(30); i <= 42; i++ {
		wg.Go(func() { errs <- b.Seen(t.Context(), i) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	r, err := b.Board(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 42, r.Mark)
	dir := t.TempDir()
	p := filepath.Join(dir, "fifo")
	require.NoError(t, syscall.Mkfifo(p, 0o600))
	_, err = (&dashboard.FileBoard{Path: p}).Board(t.Context())
	require.ErrorIs(t, err, dashboard.ErrBoard)
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(sub, 0o700))
	p = filepath.Join(sub, "board")
	require.NoError(t, os.WriteFile(p, []byte("{}"), 0o600))
	require.NoError(t, os.Chmod(sub, 0))
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	_, err = (&dashboard.FileBoard{Path: p}).Board(t.Context())
	require.ErrorIs(t, err, os.ErrPermission)
}

func TestBoardFixtureFDLimit(t *testing.T) {
	if os.Getenv("HA_BOARD_FD_HELPER") == "1" {
		var old syscall.Rlimit
		require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old))
		limited := old
		limited.Cur = 3
		require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limited))
		defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old)
		_, err := (&dashboard.FileBoard{Path: "../../docs/dashboard/sample-board.json"}).Board(t.Context())
		require.ErrorIs(t, err, syscall.EMFILE)
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBoardFixtureFDLimit$")
	cmd.Env = []string{"HA_BOARD_FD_HELPER=1"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestBoardFixturePostCount(t *testing.T) {
	p := filepath.Join(t.TempDir(), "count.json")
	for _, count := range []int{0, 200, 201} {
		r := daemon.BoardReadResult{Newest: int64(count)}
		for i := count; i > 0; i-- {
			r.Posts = append(r.Posts, daemon.BoardPostView{Seq: int64(i), Channel: "team"})
		}
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, raw, 0o600))
		_, err = (&dashboard.FileBoard{Path: p}).Board(t.Context())
		if count > 200 {
			require.ErrorIs(t, err, dashboard.ErrBoard)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestDaemonBoardLargeReply(t *testing.T) {
	r := daemon.BoardReadResult{Posts: make([]daemon.BoardPostView, 200)}
	for i := range r.Posts {
		r.Posts[i] = daemon.BoardPostView{Seq: int64(200 - i), Body: strings.Repeat("<", store.MaxBody)}
	}
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	require.Greater(t, len(raw), 1<<20)
	sock := filepath.Join(shortDir(t), "board.sock")
	fakeDaemon(t, sock, `{"ok":true,"data":`+string(raw)+"}\n")
	got, err := (dashboard.DaemonBoard{Socket: sock}).Board(t.Context())
	require.NoError(t, err)
	require.Equal(t, r.Posts, got.Posts)
	_, err = daemon.Health(t.Context(), sock)
	require.ErrorIs(t, err, daemon.ErrUnavailable)
}

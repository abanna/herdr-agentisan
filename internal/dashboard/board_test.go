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

func TestBoardStatesAndGeometry(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    dashboard.BoardFrame
		want string
	}{{"empty", dashboard.BoardFrame{Open: true, Ready: true}, "no posts yet"}, {"missing", dashboard.BoardFrame{Open: true}, "no board with this fixture"}, {"waiting", dashboard.BoardFrame{Open: true, Available: true}, "waiting for the daemon"}, {"refused", dashboard.BoardFrame{Open: true, Err: daemon.ErrNotAllowed}, "the board reads only"}, {"restart", dashboard.BoardFrame{Open: true, Err: daemon.ErrNotReady}, "waiting for the daemon"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Board: tc.b}
			require.Contains(t, dashboard.Render(f, 97, 40, plain), tc.want)
			for w := 1; w <= 100; w++ {
				for h := 1; h <= 40; h++ {
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

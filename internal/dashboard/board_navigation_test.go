package dashboard_test

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func boardNav(t *testing.T, m dashboard.Model, code rune) dashboard.Model {
	t.Helper()
	next, cmd := m.Update(tea.KeyPressMsg{Code: code})
	require.Nil(t, cmd, "navigation must not read or mark")
	return next.(dashboard.Model)
}

func TestBoardTallExpansionNavigation(t *testing.T) {
	var body strings.Builder
	for i := range 100 {
		fmt.Fprintf(&body, "LINE-%03d abcdefghijklmnopqrstuvwxyz\n", i)
	}
	body.WriteString("TAIL-MARKER")
	for _, size := range [][2]int{{97, 40}, {60, 20}, {40, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			b := &boardSource{result: daemon.BoardReadResult{Newest: 2, Posts: []daemon.BoardPostView{{Seq: 2, Channel: "team", Body: body.String()}, {Seq: 1, Channel: "daemon", Body: "other post"}}}}
			m := dashboard.New(t.Context(), dashboard.Config{Board: b})
			next, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m = next.(dashboard.Model)
			m, read := boardKey(t, m, "b")
			next, seen := m.Update(read())
			m = next.(dashboard.Model)
			next, _ = m.Update(seen())
			m = next.(dashboard.Model)
			m, _ = boardKey(t, m, "enter")
			frame := func() string {
				t.Helper()
				out := m.View().Content
				lines := strings.Split(out, "\n")
				require.Len(t, lines, size[1])
				for _, line := range lines {
					require.LessOrEqual(t, ansi.StringWidth(line), size[0])
				}
				return out
			}
			require.Contains(t, frame(), "LINE-000")
			require.NotContains(t, frame(), "TAIL-MARKER")
			var journey strings.Builder
			for range 250 {
				out := frame()
				journey.WriteString(out)
				if strings.Contains(out, "TAIL-MARKER") {
					break
				}
				m = boardNav(t, m, tea.KeyPgDown)
			}
			require.Contains(t, frame(), "TAIL-MARKER", "PageDown must reach the tail")
			for i := range 100 {
				require.Contains(t, journey.String(), fmt.Sprintf("LINE-%03d", i), "no body line may be skipped")
			}
			require.True(t, m.Frame().Board.Expanded)
			require.Zero(t, m.Frame().Board.Selected)
			m = boardNav(t, m, tea.KeyHome)
			require.Contains(t, frame(), "LINE-000")
			m = boardNav(t, m, tea.KeyDown)
			require.NotContains(t, frame(), "LINE-000")
			m = boardNav(t, m, tea.KeyUp)
			require.Contains(t, frame(), "LINE-000")
			m = boardNav(t, m, tea.KeyEnd)
			require.Contains(t, frame(), "TAIL-MARKER")
			m = boardNav(t, m, tea.KeyPgUp)
			require.NotContains(t, frame(), "TAIL-MARKER")
			require.Equal(t, 1, b.reads)
			require.Equal(t, 1, b.marks)
			require.EqualValues(t, 2, b.upto)
			m, _ = boardKey(t, m, "esc")
			m = boardNav(t, m, tea.KeyDown)
			require.Equal(t, 1, m.Frame().Board.Selected)
			m, _ = boardKey(t, m, "enter")
			require.Contains(t, frame(), "other post")
			m, _ = boardKey(t, m, "t")
			require.False(t, m.Frame().Board.Expanded)
			m, _ = boardKey(t, m, "enter")
			require.Contains(t, frame(), "LINE-000", "filter selection must reset body scroll")
			m, _ = boardKey(t, m, "esc")
			m, _ = boardKey(t, m, "esc")
			require.False(t, m.Frame().Board.Open)
		})
	}
}

func TestBoardRefusalAfterSuccessfulRead(t *testing.T) {
	for _, refusal := range []error{daemon.ErrNotAllowed, daemon.ErrNotReady} {
		t.Run(refusal.Error(), func(t *testing.T) {
			b := &boardSource{result: daemon.BoardReadResult{Mark: 0, Newest: 1, Posts: []daemon.BoardPostView{{Seq: 1, Channel: "team", Author: "coder", Kind: "post", Body: "PRIVATE-BOARD-MARKER"}}}}
			m := dashboard.New(t.Context(), dashboard.Config{Board: b})
			m, read := boardKey(t, m, "b")
			n, seen := m.Update(read())
			m = n.(dashboard.Model)
			n, tick := m.Update(seen())
			m = n.(dashboard.Model)
			require.Contains(t, m.View().Content, "PRIVATE-BOARD-MARKER")
			b.err = refusal
			n, read = m.Update(tick())
			m = n.(dashboard.Model)
			n, _ = m.Update(read())
			m = n.(dashboard.Model)
			require.ErrorIs(t, m.Frame().Board.Err, refusal)
			require.NotContains(t, m.View().Content, "PRIVATE-BOARD-MARKER", "a placement refusal must hide previously loaded posts")
		})
	}
}

func TestBoardExpansionRefreshAndResize(t *testing.T) {
	b := &boardSource{result: daemon.BoardReadResult{Mark: 1, Newest: 1, Posts: []daemon.BoardPostView{{Seq: 1, Channel: "team", Body: strings.Repeat("body line\n", 100) + "TAIL-MARKER"}}}}
	m := dashboard.New(t.Context(), dashboard.Config{Board: b})
	m, read := boardKey(t, m, "b")
	next, tick := m.Update(read())
	m = next.(dashboard.Model)
	m, _ = boardKey(t, m, "enter")
	m = boardNav(t, m, tea.KeyEnd)
	scroll := m.Frame().Board.Scroll
	require.Positive(t, scroll)
	b.result.Newest = 2
	b.result.Posts = append([]daemon.BoardPostView{{Seq: 2, Channel: "team", Body: "new post"}}, b.result.Posts...)
	next, read = m.Update(tick())
	m = next.(dashboard.Model)
	next, seen := m.Update(read())
	m = next.(dashboard.Model)
	require.NotNil(t, seen)
	require.Equal(t, 1, m.Frame().Board.Selected)
	require.True(t, m.Frame().Board.Expanded)
	require.Equal(t, scroll, m.Frame().Board.Scroll)
	require.Contains(t, m.View().Content, "TAIL-MARKER")
	for _, size := range [][2]int{{1, 1}, {20, 3}, {97, 7}, {97, 8}, {97, 200}, {40, 12}, {97, 40}} {
		next, _ = m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = next.(dashboard.Model)
		m = boardNav(t, m, tea.KeyEnd)
		out := m.View().Content
		require.Len(t, strings.Split(out, "\n"), size[1])
		for line := range strings.SplitSeq(out, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), size[0])
		}
	}
	// Once the selected post disappears, no new post inherits its expansion.
	next, tick = m.Update(seen())
	m = next.(dashboard.Model)
	b.result.Mark = 2
	b.result.Posts = b.result.Posts[:1]
	next, read = m.Update(tick())
	m = next.(dashboard.Model)
	next, _ = m.Update(read())
	m = next.(dashboard.Model)
	require.False(t, m.Frame().Board.Expanded)
	require.Zero(t, m.Frame().Board.Scroll)
	require.Contains(t, m.View().Content, "new post")
}

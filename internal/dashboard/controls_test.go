package dashboard_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

func TestWheelMatchesArrows(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"overview", "group", "board list", "expanded post", "search"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			cfg := config(src, rec)
			cfg.Board = &boardSource{result: daemon.BoardReadResult{Mark: 2, Newest: 2, Posts: []daemon.BoardPostView{
				{Seq: 2, Channel: "team", Body: strings.Repeat("long post\n", 30)},
				{Seq: 1, Channel: "team", Body: "second post"},
			}}}
			m := started(t, cfg, 80, 12)
			switch mode {
			case "group":
				m, _ = update(t, m, tea.KeyPressMsg{Code: '1'})
				require.NotEmpty(t, m.Frame().Group)
			case "board list", "expanded post":
				var read tea.Cmd
				m, read = boardKey(t, m, "b")
				m = run(t, m, read)
				require.True(t, m.Frame().Board.Ready)
				if mode == "expanded post" {
					m, _ = boardKey(t, m, "enter")
					require.True(t, m.Frame().Board.Expanded)
				}
			case "search":
				m, _ = update(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
				require.True(t, m.Frame().Search)
			}
			for _, step := range []struct {
				wheel tea.MouseButton
				arrow tea.KeyPressMsg
			}{{tea.MouseWheelDown, keyDown}, {tea.MouseWheelUp, keyUp}} {
				want, _ := update(t, m, step.arrow)
				require.NotEqual(t, m.Frame(), want.Frame(), "this arrow must exercise movement")
				next, cmd := update(t, m, tea.MouseWheelMsg{Button: step.wheel})
				require.Nil(t, cmd)
				require.Equal(t, want.Frame(), next.Frame())
				m = next
			}
			for _, button := range []tea.MouseButton{tea.MouseWheelLeft, tea.MouseWheelRight} {
				next, cmd := update(t, m, tea.MouseWheelMsg{Button: button})
				require.Nil(t, cmd)
				require.Equal(t, m.Frame(), next.Frame())
			}
			require.Empty(t, rec.names())
		})
	}
}

func TestBtopShortcut(t *testing.T) {
	t.Parallel()
	key := tea.KeyPressMsg{Code: 'm', Text: "m"}
	rec, op := &recorder{}, &opener{}
	cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, rec)
	cfg.Btop = op
	m := started(t, cfg, 80, 24)

	for _, mode := range []string{"overview", "group", "board"} {
		t.Run(mode, func(t *testing.T) {
			start := m
			switch mode {
			case "group":
				start, _ = update(t, m, tea.KeyPressMsg{Code: '1'})
			case "board":
				start, _ = boardKey(t, m, "b")
			}
			prev := op.count()
			next, cmd := update(t, start, key)
			require.Equal(t, prev, op.count(), "the opener runs in the command")
			require.Equal(t, start.Frame(), next.Frame())
			run(t, next, cmd)
			require.Equal(t, prev+1, op.count())
			require.Empty(t, rec.names())
		})
	}

	search, _ := update(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	search, cmd := update(t, search, key)
	require.Nil(t, cmd)
	require.Equal(t, "m", search.Frame().Query)
	_, cmd = update(t, dashboard.New(t.Context(), dashboard.Config{}), key)
	require.Nil(t, cmd)
}

func TestHelpListsNavigationShortcuts(t *testing.T) {
	t.Parallel()
	m := dashboard.New(t.Context(), dashboard.Config{Btop: &opener{}, Options: plain})
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = update(t, m, keyHelp)
	for _, label := range []string{"wheel", "pgup/pgdown", "home/end", "m / click"} {
		require.Contains(t, m.View().Content, label)
	}
	frame := m.Frame()
	frame.Btop = false
	require.NotContains(t, dashboard.Render(frame, 80, 24, plain), "m / click")
}

package dashboard_test

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

func TestOverviewColumnHeadings(t *testing.T) {
	t.Parallel()
	snap := fixture(t, "full")
	for _, tc := range []struct {
		name          string
		width, height int
		columns       [][2]string
		absent        []string
	}{
		{"wide", 120, 60, [][2]string{
			{"Worker", "pee01"},
			{"Status", "working"},
			{"Model", "opus"},
			{"Ctx", "▓▓░░░"},
			{"Item", "r2"},
			{"Stage", "J1 re-run"},
		}, nil},
		{"narrow", 50, 30, [][2]string{
			{"Worker", "pee01"}, {"Status", "working"}, {"Model", "opus"}, {"Ctx", "▓▓░░░"},
		}, []string{"Item", "Stage", "Age"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := strings.Split(dashboard.Render(dashboard.Frame{Snapshot: snap, Now: at}, tc.width, tc.height, plain), "\n")
			row := slices.IndexFunc(rows, func(line string) bool { return strings.Contains(line, "pee01") })
			require.Positive(t, row, "first worker and its heading")
			heading, worker := rows[row-1], rows[row]
			cell := func(line, field string) int {
				i := strings.Index(line, field)
				require.GreaterOrEqual(t, i, 0, "missing %q in %q", field, line)
				return ansi.StringWidth(line[:i])
			}
			for _, pair := range tc.columns {
				require.Equal(t, cell(worker, pair[1]), cell(heading, pair[0]), "%s", pair[0])
			}
			for _, label := range tc.absent {
				require.NotContains(t, heading, label)
			}
			if tc.name == "wide" {
				require.Equal(t, cell(worker, "12m"), cell(heading, "Age"))
				require.Equal(t, len(snap.Groups), strings.Count(strings.Join(rows, "\n"), "Worker"))
			}
		})
	}
}

func TestOverviewHeadingsUseTheSearchLayout(t *testing.T) {
	t.Parallel()
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, &recorder{}), 80, 24)
	m, _ = update(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m, _ = update(t, m, tea.KeyPressMsg{Text: "NERD-5255"})
	require.Equal(t, "NERD-5255", m.Frame().Query)
	view := m.View().Content
	require.Contains(t, view, "Worker")
	require.Contains(t, view, "pee04")
	require.NotContains(t, view, "pee01")
	m, _ = update(t, m, tea.KeyPressMsg{Text: "-missing"})
	require.Contains(t, m.View().Content, "no matches")
	require.NotContains(t, m.View().Content, "Worker")

	m, _ = update(t, m, keyEsc)
	m, _ = update(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m, _ = update(t, m, tea.KeyPressMsg{Text: "queue empty"})
	require.Contains(t, m.View().Content, "Worker", "a two-cell name still receives a full heading")
	require.Contains(t, m.View().Content, "queue empty")
}

func TestOverviewHeadingClickAndShortFrame(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, rec), 80, 24)
	x, y := locate(t, m.View().Content, "Worker")
	next, cmd := update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Nil(t, cmd)
	require.Equal(t, m.Frame(), next.Frame())
	require.Empty(t, rec.names())

	next, _ = update(t, next, tea.WindowSizeMsg{Width: 80, Height: 5})
	require.NotContains(t, next.View().Content, "Worker")
	require.Contains(t, next.View().Content, "pee01")
}

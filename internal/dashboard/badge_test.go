package dashboard_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// unread is the full fixture with n posts on its board the user has not
// seen (A60).
func unread(t *testing.T, n int) *snapshot.Snapshot {
	t.Helper()
	s := fixture(t, "full")
	s.Project.BoardUnread = n
	return s
}

// TestHeaderBoardBadge (A60): the header's first line shows "board N" just
// before [ btop ] while anything is unread. It is never dropped for width:
// the PRs shorten first, then the button gives way, then the project line
// is cut.
func TestHeaderBoardBadge(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		w, h int
		btop bool
		want string // the header's first line
	}{
		"97 columns, the PRs one by one": {w: 97, h: 40, btop: true, want: "│ agentisan · PRs #878⏳ #882💬 #884✗ #886✓ #887… · issues 14 · slots 2/2     board 3  [ btop ] │"},
		"80, the PRs a count":            {w: 80, h: 24, btop: true, want: "│ agentisan · PRs 5 · issues 14 · slots 2/2                  board 3  [ btop ] │"},
		"60, the project line cut":       {w: 60, h: 20, btop: true, want: "│ agentisan · PRs 5 · issues 14 · slot…  board 3  [ btop ] │"},
		"50, cut further":                {w: 50, h: 30, btop: true, want: "│ agentisan · PRs 5 · issues…  board 3  [ btop ] │"},
		"30, the button gives way":       {w: 30, h: 24, btop: true, want: "│ agentisan · PRs …  board 3 │"},
		"no opener, no button":           {w: 80, h: 24, want: "│ agentisan · PRs 5 · issues 14 · slots 2/2                            board 3 │"},
		"too narrow for the name":        {w: 12, h: 24, btop: true, want: "│  board 3 │"},
		"bare lines":                     {w: 80, h: 6, btop: true, want: " agentisan · PRs 5 · issues 14 · slots 2/2                    board 3  [ btop ]"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := dashboard.Render(dashboard.Frame{Snapshot: unread(t, 3), Now: at, Btop: tc.btop}, tc.w, tc.h, plain)
			first := lines(out)[1]
			if tc.h < 9 {
				first = lines(out)[0]
			}
			assert.Equal(t, tc.want, first)
		})
	}
}

// TestNoBadgeWithNothingUnread: with nothing unread, or no project, the
// header is as it was before the board, at every width.
func TestNoBadgeWithNothingUnread(t *testing.T) {
	t.Parallel()
	for _, w := range []int{97, 80, 60, 24} {
		before := dashboard.Render(dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Btop: true}, w, 24, plain)
		assert.Equal(t, before, dashboard.Render(dashboard.Frame{Snapshot: unread(t, 0), Now: at, Btop: true}, w, 24, plain), "%d columns", w)
		assert.NotContains(t, lines(before)[1], "board")
	}
	s := fixture(t, "full")
	s.Project = nil
	assert.NotContains(t, lines(dashboard.Render(dashboard.Frame{Snapshot: s, Now: at, Btop: true}, 80, 24, plain))[1], "board")
}

// TestBadgeHeaderStyle: the badge is drawn bold in the note colour, never in
// reverse video, and the button keeps its own style beside it.
func TestBadgeHeaderStyle(t *testing.T) {
	t.Parallel()
	out := dashboard.Render(dashboard.Frame{Snapshot: unread(t, 3), Now: at, Btop: true}, 80, 24, fullColour)
	first := lines(out)[1]
	assert.Contains(t, first, "\x1b[1;38;2;241;250;140mboard 3\x1b[m", "bold, in the note colour")
	assert.Contains(t, first, "[ btop ]")
}

// TestBadgeLeavesTheButtonClickable: what the header draws is what a click
// hits. Beside the badge the button opens btop; where the badge made it give
// way, a click there does nothing.
func TestBadgeLeavesTheButtonClickable(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		w, unread int
		opens     bool
	}{
		"room for both":          {w: 80, unread: 3, opens: true},
		"the button gave way":    {w: 30, unread: 3},
		"at 30 with none unread": {w: 30, opens: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			op := &opener{}
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(unread(t, tc.unread))}}, &recorder{})
			cfg.Btop = op
			m := started(t, cfg, tc.w, 24)
			x, y := tc.w-2-8, 1 // where the button sits when drawn
			if tc.opens {
				x, y = locate(t, m.View().Content, "[ btop ]")
			}
			_, cmd := update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			if !tc.opens {
				assert.Nil(t, cmd)
				return
			}
			require.NotNil(t, cmd)
			run(t, m, cmd)
			assert.Equal(t, 1, op.count())
		})
	}
}

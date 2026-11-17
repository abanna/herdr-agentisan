package dashboard_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestBoardRowsAreClosedByTheirBorder reads what a board frame draws, which
// the geometry sweep cannot see: it fails on a panic, a line count or a
// width, and Render's last cut hides everything else. Every row of the box
// is exactly w cells and ends in the box's border, so a state message
// longer than the box is cut to its inside, with its ellipsis (boardview.go:
// the truncate in boardBox, which a message is the only row to need: posts
// are cut before they are boxed). Without it the message runs past the
// border and the last cut takes the border off.
func TestBoardRowsAreClosedByTheirBorder(t *testing.T) {
	t.Parallel()
	posts := boardSweepPosts()
	for name, b := range map[string]dashboard.BoardFrame{
		"refused":  {Open: true, Err: daemon.ErrNotAllowed},
		"waiting":  {Open: true, Available: true},
		"missing":  {Open: true},
		"posts":    {Open: true, Available: true, Ready: true, Posts: posts, OpenMark: 6, Selected: 4},
		"expanded": {Open: true, Available: true, Ready: true, Posts: posts, OpenMark: 6, Selected: 4, Expanded: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// 64 and 65 are each side of the refusal's 61 cells and the box's 4.
			for _, w := range []int{20, 30, 40, 64, 65, 97} {
				f := dashboard.Frame{Snapshot: fixture(t, "full"), Now: at, Board: b}
				lines := strings.Split(dashboard.Render(f, w, 24, plain), "\n")
				title := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "BOARD") })
				require.GreaterOrEqual(t, title, 0, "the title is drawn at %d", w)
				rows := 0
				for _, l := range lines[title+2:] {
					if strings.HasPrefix(l, "╰") {
						break
					}
					rows++
					require.Equal(t, w, ansi.StringWidth(l), "a row of the box is the terminal's width at %d: %q", w, l)
					require.True(t, strings.HasSuffix(l, "│"), "and ends in the box's border at %d: %q", w, l)
				}
				require.Positive(t, rows, "the box has a row at %d", w)
			}
		})
	}
}

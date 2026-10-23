package dashboard

import (
	"strings"
	"time"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// ModelCells is the widest model ShortModel returns.
const ModelCells = modelCells

// ColumnWidths is how a frame w cells wide lays out the agent rows of groups
// at now: the name's width, then the state, model, item, stage and time
// columns' widths, 0 for a column dropped.
func ColumnWidths(groups []snapshot.Group, now time.Time, w int) (name int, cols []int) {
	c := planColumns(groups, now, rowWidth(w))
	return c.name, c.width[:]
}

// PlaceText draws over at column x on a line of base's segments, all plain
// text, and returns the text drawn: the seam the tests check place's two
// cuts through, wherever the segments break.
func PlaceText(base []string, x int, over string) string {
	l := make(line, 0, len(base))
	for _, s := range base {
		l = append(l, seg{s, plainStyle})
	}
	var b strings.Builder
	for _, s := range place(l, x, line{{over, plainStyle}}) {
		b.WriteString(s.text)
	}
	return b.String()
}

// ClipText clips a line of plain segments, those marked reverse in reverse
// video, to w cells as a frame does, and returns the text it keeps.
func ClipText(base []string, reverse []bool, w int) string {
	l := make(line, 0, len(base))
	for i, s := range base {
		st := plainStyle
		if reverse[i] {
			st = st.Reverse(true)
		}
		l = append(l, seg{s, st})
	}
	var b strings.Builder
	for _, s := range l.cut(0, w).trimRight() {
		b.WriteString(s.text)
	}
	return b.String()
}

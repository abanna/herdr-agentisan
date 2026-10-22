package dashboard

import "strings"

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

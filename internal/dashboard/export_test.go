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

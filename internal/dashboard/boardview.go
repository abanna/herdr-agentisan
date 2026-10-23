package dashboard

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/charmbracelet/x/ansi"
)

// boardView uses the same rows for drawing and hits. A row names a post,
// including its continuation lines, or -1 for a border or state message.
type boardView struct {
	rows   []line
	posts  []int
	offset int
	back   span
}

func (v *boardView) add(ln line, post int) {
	v.rows = append(v.rows, ln)
	v.posts = append(v.posts, post)
}

func planBoard(l layout, b BoardFrame) *boardView {
	v := &boardView{}
	v.back = span{x0: 1, x1: 1 + ansi.StringWidth(backButton), y: l.top, ok: l.avail > 0 && l.w >= 7}
	posts := b.posts()
	n := 0
	for _, p := range posts {
		if p.Seq > b.OpenMark {
			n++
		}
	}
	filter := b.Filter
	if filter == "" {
		filter = "team + daemon"
	}
	title := fmt.Sprintf(" %s  BOARD%s%d posts%s%d new%s%s", backButton, sep, len(posts), sep, n, sep, filter)
	v.add(line{{title, boldStyle}}, -1)
	v.add(line{{"╭" + strings.Repeat("─", max(0, l.w-2)) + "╮", dimStyle}}, -1)
	switch {
	case errors.Is(b.Err, daemon.ErrNotAllowed):
		posts = nil
		v.add(boardBox(line{{"the board reads only from the dashboard in a boss's tab (A60)", dimStyle}}, l.w), -1)
	case errors.Is(b.Err, daemon.ErrNotReady):
		posts = nil
		v.add(boardBox(line{{waiting, dimStyle}}, l.w), -1)
	case !b.Ready:
		text := waiting
		if !b.Available {
			text = "no board with this fixture"
		}
		v.add(boardBox(line{{text, dimStyle}}, l.w), -1)
	case len(posts) == 0:
		v.add(boardBox(line{{"no posts yet", dimStyle}}, l.w), -1)
	}
	selectedStart, selectedEnd := 0, 0
	for i, p := range posts {
		selected := i == b.Selected
		if selected {
			selectedStart = len(v.rows)
		}
		prefix, bodyWidth := boardPrefix(p, b.OpenMark, l.now, rowWidth(l.w))
		body := boardText(p.Body)
		bodies := []string{strings.SplitN(body, "\n", 2)[0]}
		if selected && b.Expanded {
			bodies = strings.Split(ansi.Wrap(body, max(1, bodyWidth), ""), "\n")
		}
		for j, text := range bodies {
			lead := prefix
			if j > 0 {
				lead = line{{strings.Repeat(" ", prefix.width()), plainStyle}}
			}
			ln := slices.Concat(lead, line{{text, plainStyle}}).truncate(rowWidth(l.w))
			if selected {
				for k := range ln {
					ln[k].style = ln[k].style.Reverse(true)
				}
			}
			v.add(boardBox(ln, l.w), i)
		}
		if selected {
			selectedEnd = len(v.rows)
		}
	}
	v.add(line{{"╰" + strings.Repeat("─", max(0, l.w-2)) + "╯", dimStyle}}, -1)
	// Keep title and top border fixed; scroll only the posts. A tall expanded
	// post starts at its first line, so the body is never opened mid-sentence.
	room := max(0, l.avail-3)
	if selectedEnd > 0 && room > 0 {
		v.offset = min(max(0, selectedEnd-2-room), max(0, selectedStart-2))
	}
	return v
}

func boardBox(ln line, w int) line {
	return slices.Concat(line{{"│ ", dimStyle}}, ln.truncate(rowWidth(w)).pad(rowWidth(w)), line{{" │", dimStyle}})
}

func boardText(s string) string {
	// Strip directional overrides and invisible separators; keep ZWJ and
	// variation selectors, which are part of emoji grapheme clusters.
	return strings.Map(func(r rune) rune {
		if r != '\n' && (unicode.IsControl(r) || (unicode.Is(unicode.Cf, r) && r != '\u200d')) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

func boardTime(at, now time.Time) string {
	if at.IsZero() {
		return "--:--"
	}
	at = at.In(now.Location())
	ay, am, ad := at.Date()
	ny, nm, nd := now.Date()
	if ay == ny && am == nm && ad == nd {
		return at.Format("15:04")
	}
	return at.Format("01-02")
}

func boardPrefix(p daemon.BoardPostView, mark int64, now time.Time, w int) (line, int) {
	who := p.Worker
	if who == "" {
		who = p.Subject
	}
	if who == "" {
		who = p.Author
	}
	dot := " "
	if p.Seq > mark {
		dot = "●"
	}
	cols := []struct {
		text  string
		width int
	}{{dot, 1}, {boardTime(p.At, now), 5}}
	// The body keeps 20 cells: channel, then kind give way before who.
	channel, kind, name := w >= 70, w >= 62, 14
	if w < 62 {
		name = 8
	}
	if channel {
		cols = append(cols, struct {
			text  string
			width int
		}{p.Channel, 6})
	}
	cols = append(cols, struct {
		text  string
		width int
	}{who, name})
	if kind {
		cols = append(cols, struct {
			text  string
			width int
		}{p.Kind, 11})
	}
	var out line
	for _, c := range cols {
		out = slices.Concat(out, line{{boardText(c.text), dimStyle}}.truncate(c.width).pad(c.width), line{{"  ", plainStyle}})
	}
	return out, max(1, w-out.width())
}

func (l layout) drawBoard(canvas []line) {
	v := l.board
	for i := range l.avail {
		idx := i
		if i >= 2 {
			idx += v.offset
		}
		if i == l.avail-1 && len(v.rows) > l.avail {
			idx = len(v.rows) - 1
		}
		if idx >= len(v.rows) {
			break
		}
		canvas[l.top+i] = v.rows[idx]
	}
}

func (l layout) boardHit(x, y int) (target, Selection) {
	v := l.board
	if v.back.ok && y == v.back.y && x >= v.back.x0 && x < v.back.x1 {
		return hitBack, Selection{}
	}
	i := y - l.top
	if i < 2 || i >= l.avail || x < 1 || x >= l.w-1 {
		return hitNothing, Selection{}
	}
	idx := i + v.offset
	if i == l.avail-1 && len(v.rows) > l.avail {
		return hitNothing, Selection{}
	}
	if idx < len(v.posts) && v.posts[idx] >= 0 {
		return hitBoard, Selection{Agent: v.posts[idx]}
	}
	return hitNothing, Selection{}
}

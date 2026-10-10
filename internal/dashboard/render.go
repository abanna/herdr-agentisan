package dashboard

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// Card geometry, in terminal cells.
const (
	minCardWidth = 26
	maxCardWidth = 40
	// modelCells, barCells and pctCells make up the right-hand block of an
	// agent's first line: "opus ▓▓▓░░  38%".
	modelCells = 4
	barCells   = 5
	pctCells   = 4
	rightBlock = modelCells + 1 + barCells + 1 + pctCells
	// bossBarCells is the boss line's wider ctx bar.
	bossBarCells = 8
	// narrowInner is the narrowest card interior that still fits the right
	// block beside a one-cell name: icon, space, name, gap, block, margin.
	narrowInner = 2 + 1 + 1 + rightBlock + 1
)

const (
	// sep separates the parts of a line: item and stage, footer notes.
	sep     = " · "
	hints   = "←↑↓→ move · enter focus · ? help"
	waiting = "waiting for the daemon…"
	noTeam  = "no agents in this team"
)

// Options tune the renderer. The zero value renders with DefaultThresholds
// and TrueColor, leaving it to bubbletea to downsample for the terminal.
type Options struct {
	// Thresholds colour ctx. The zero value means DefaultThresholds.
	Thresholds Thresholds
	// Profile is the colour profile the frame is written for. NoTTY strips
	// colour and attributes alike, which is what golden frames want; ASCII
	// keeps attributes such as reverse video. Unknown means TrueColor.
	Profile colorprofile.Profile
}

func (o Options) thresholds() Thresholds {
	if o.Thresholds == (Thresholds{}) {
		return DefaultThresholds()
	}
	return o.Thresholds
}

// Selection addresses one agent: the card's index in display order (see
// Order) and the agent's index within it.
type Selection struct {
	Card  int
	Agent int
}

// Frame is everything one frame draws. Render is a pure function of a Frame
// and the terminal's size.
type Frame struct {
	// Snapshot is nil until the first snapshot arrives.
	Snapshot  *snapshot.Snapshot
	Selection Selection
	// Now is the clock in the header, formatted as given (HH:MM).
	Now time.Time
	// Help draws the key help over the cards.
	Help bool
	// Note is a standing footer note, such as why focus is off.
	Note string
	// Errors are shown in the footer, first to last. Nil entries are skipped.
	Errors []error
}

// Render draws f for a w x h terminal: exactly h lines, none wider than w,
// none ending in spaces. A size of zero or less draws nothing.
func Render(f Frame, w, h int, opts Options) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	th := opts.thresholds()
	l := plan(f, w, h)

	canvas := make([]line, h)
	canvas[0] = header(f, w)
	if l.top > 1 {
		canvas[1] = bossLine(*f.Snapshot.Boss, w, th)
	}
	switch {
	case l.avail == 0:
	case f.Snapshot == nil:
		canvas[l.top] = line{{" ", plainStyle}, {waiting, dimStyle}}.truncate(w)
	case len(l.groups) == 0:
		canvas[l.top] = line{{" ", plainStyle}, {noTeam, dimStyle}}.truncate(w)
	default:
		for _, p := range l.places {
			for i, cl := range drawCard(l, p, th) {
				y := p.y + i
				if y >= l.top+l.avail {
					break
				}
				canvas[y] = place(canvas[y], p.x, cl)
			}
		}
	}
	if h > 1 {
		canvas[h-1] = footer(f, w)
	}
	if f.Help {
		overlayHelp(canvas, l, w)
	}

	var b strings.Builder
	for i, ln := range canvas {
		if i > 0 {
			b.WriteByte('\n')
		}
		// The last word on width: below a card's own minimum (its two
		// corners at one column) nothing else may spill past the terminal.
		b.WriteString(ln.cut(0, w).trimRight().render())
	}
	return downsample(b.String(), opts.Profile)
}

// downsample rewrites a TrueColor frame for profile p.
func downsample(s string, p colorprofile.Profile) string {
	if p == colorprofile.TrueColor || p == colorprofile.Unknown {
		return s
	}
	var b strings.Builder
	w := &colorprofile.Writer{Forward: &b, Profile: p}
	// A strings.Builder never fails a write.
	_, _ = w.WriteString(s)
	return b.String()
}

// placement is one card on screen.
type placement struct {
	card int // index into layout.groups
	x, y int // top-left cell
	// first and shown are the agents drawn, [first, first+shown); hidden is
	// how many are not.
	first, shown, hidden int
}

// layout is where everything goes. Render draws it; a click is mapped back
// through it, so drawing and hit-testing can never disagree.
type layout struct {
	groups      []snapshot.Group // display order
	cardW, cols int
	top, avail  int // the card area is lines [top, top+avail)
	places      []placement
	sel         Selection
	selOK       bool
}

// plan lays f out on a w x h terminal.
func plan(f Frame, w, h int) layout {
	var l layout
	if w <= 0 || h <= 0 {
		return l
	}
	l.top = 1
	if f.Snapshot != nil && f.Snapshot.Boss != nil && h >= 3 {
		l.top = 2
	}
	footerLines := 0
	if h > 1 {
		footerLines = 1
	}
	l.avail = max(0, h-l.top-footerLines)
	if f.Snapshot == nil {
		return l
	}
	l.groups = Order(f.Snapshot.Groups)
	l.sel, l.selOK = f.Selection, validSelection(l.groups, f.Selection)
	l.cols = columns(l.groups, w)
	l.cardW = stretched(l.cols, w)

	counts := make([]int, len(l.groups))
	for i, g := range l.groups {
		counts[i] = len(g.Agents)
	}
	selCard := -1
	if l.selOK {
		selCard = l.sel.Card
	}
	caps := fitCaps(counts, l.cols, selCard, l.avail)
	r0, r1 := visibleRows(counts, caps, l.cols, selCard, l.avail)

	y := l.top
	for r := r0; r <= r1; r++ {
		rowH := 0
		for c := range l.cols {
			i := r*l.cols + c
			if i >= len(counts) {
				break
			}
			p := placement{card: i, x: c * (l.cardW + 1), y: y, shown: min(caps[i], counts[i])}
			p.hidden = counts[i] - p.shown
			// The selected card scrolls just far enough to show the
			// selection on its last visible row.
			if i == selCard && l.sel.Agent >= p.shown {
				p.first = l.sel.Agent - p.shown + 1
			}
			l.places = append(l.places, p)
			rowH = max(rowH, cardHeight(counts[i], caps[i]))
		}
		y += rowH
	}
	return l
}

// hit maps a cell to the agent drawn there. Either of an agent's two lines
// counts, but not the card's borders.
func (l layout) hit(x, y int) (Selection, bool) {
	if y < l.top || y >= l.top+l.avail {
		return Selection{}, false
	}
	for _, p := range l.places {
		if x <= p.x || x >= p.x+l.cardW-1 {
			continue
		}
		row := y - p.y - 1
		if row < 0 || row >= 2*p.shown {
			continue
		}
		return Selection{Card: p.card, Agent: p.first + row/2}, true
	}
	return Selection{}, false
}

func validSelection(groups []snapshot.Group, s Selection) bool {
	return s.Card >= 0 && s.Card < len(groups) && s.Agent >= 0 && s.Agent < len(groups[s.Card].Agents)
}

// naturalWidth is the narrowest width every card can share: wide enough for
// the longest agent's first line and the longest title, at least
// minCardWidth, at most maxCardWidth, and never wider than the terminal. An
// agent's second line does not count; it is cut to fit. It decides how many
// columns fit; the cards are then stretched (see stretched).
func naturalWidth(groups []snapshot.Group, w int) int {
	need := minCardWidth
	for _, g := range groups {
		need = max(need, 2+ansi.StringWidth(" "+clean(g.Name)+" "+strconv.Itoa(len(g.Agents))+" ")+1)
		for _, a := range g.Agents {
			need = max(need, 2+2+ansi.StringWidth(clean(a.Name))+1+rightBlock+1)
		}
	}
	return min(need, maxCardWidth, w)
}

// columns is how many cards sit side by side: as many as fit at their
// natural width, one cell apart, but never more than there are cards.
func columns(groups []snapshot.Group, w int) int {
	return max(1, min(w/(naturalWidth(groups, w)+1), len(groups)))
}

// stretched is the width cards share once cols of them fill the terminal,
// one cell apart: at most maxCardWidth, never narrower than their natural
// width (cols was chosen so that it fits), and never wider than w.
func stretched(cols, w int) int {
	return max(1, min(maxCardWidth, (w-(cols-1))/cols, w))
}

// cardHeight is a card's height showing up to limit of its n agents: two
// borders, two lines per agent, and a "…N more" line when some are hidden.
func cardHeight(n, limit int) int {
	if n == 0 {
		return 3 // borders and "no agents"
	}
	shown := min(limit, n)
	h := 2 + 2*shown
	if shown < n {
		h++
	}
	return h
}

// totalHeight is the height of the grid: each row of cards is as tall as its
// tallest card.
func totalHeight(counts, caps []int, cols int) int {
	total := 0
	for start := 0; start < len(counts); start += cols {
		rowH := 0
		for i := start; i < min(start+cols, len(counts)); i++ {
			rowH = max(rowH, cardHeight(counts[i], caps[i]))
		}
		total += rowH
	}
	return total
}

// fitCaps decides how many agents each card shows so the grid fits avail
// lines, keeping the selected card (index sel, or -1) whole for as long as
// possible:
//
//  1. every other card shrinks, all to the same cap, down to one agent;
//  2. then the selected card shrinks, down to one agent (it scrolls to keep
//     the selection in view);
//  3. then every other card shrinks to its "…N more" line alone.
//
// After any step that fits, the other cards grow back one agent at a time,
// in turn, while the grid still fits: a card shorter than the tallest in its
// row costs nothing to grow. When nothing fits, visibleRows drops rows.
func fitCaps(counts []int, cols, sel, avail int) []int {
	caps := make([]int, len(counts))
	copy(caps, counts)
	fits := func() bool { return totalHeight(counts, caps, cols) <= avail }
	if fits() {
		return caps
	}
	others := func(limit int) {
		for i, n := range counts {
			if i != sel {
				caps[i] = min(n, limit)
			}
		}
	}

	largest := 0
	for i, n := range counts {
		if i != sel {
			largest = max(largest, n)
		}
	}
	for limit := largest - 1; limit >= 1; limit-- {
		if others(limit); fits() {
			return grow(counts, caps, cols, sel, avail)
		}
	}
	if sel >= 0 {
		for limit := counts[sel] - 1; limit >= 1; limit-- {
			if caps[sel] = limit; fits() {
				return grow(counts, caps, cols, sel, avail)
			}
		}
	}
	if others(0); fits() {
		return grow(counts, caps, cols, sel, avail)
	}
	return caps
}

// grow hands spare lines back to the cards other than sel, one agent per card
// per round, in display order, until no card can grow and still fit.
func grow(counts, caps []int, cols, sel, avail int) []int {
	for grew := true; grew; {
		grew = false
		for i := range caps {
			if i == sel || caps[i] >= counts[i] {
				continue
			}
			caps[i]++
			if totalHeight(counts, caps, cols) <= avail {
				grew = true
				continue
			}
			caps[i]--
		}
	}
	return caps
}

// visibleRows picks the rows of cards to draw, [r0, r1]: all of them when
// they fit, otherwise a run that includes the selected card's row.
func visibleRows(counts, caps []int, cols, sel, avail int) (int, int) {
	var heights []int
	for start := 0; start < len(counts); start += cols {
		rowH := 0
		for i := start; i < min(start+cols, len(counts)); i++ {
			rowH = max(rowH, cardHeight(counts[i], caps[i]))
		}
		heights = append(heights, rowH)
	}
	if len(heights) == 0 {
		return 0, -1
	}
	sum := func(a, b int) int {
		s := 0
		for _, h := range heights[a : b+1] {
			s += h
		}
		return s
	}
	selRow := 0
	if sel >= 0 {
		selRow = sel / cols
	}
	r0 := 0
	for r0 < selRow && sum(r0, selRow) > avail {
		r0++
	}
	r1 := selRow
	for r1+1 < len(heights) && sum(r0, r1+1) <= avail {
		r1++
	}
	return r0, r1
}

// drawCard draws one card, cardW wide.
func drawCard(l layout, p placement, th Thresholds) []line {
	g := l.groups[p.card]
	inner := l.cardW - 2
	selAgent := -1
	if l.selOK && l.sel.Card == p.card {
		selAgent = l.sel.Agent
	}
	border := dimStyle
	if selAgent >= 0 {
		border = accentStyle
	}
	edge := func(body line) line {
		out := line{{"│", border}}
		out = append(out, body.pad(inner)...)
		return append(out, seg{"│", border})
	}

	// "╭ coders 9 ───╮": the name gives way before the count does.
	count := " " + strconv.Itoa(len(g.Agents)) + " "
	title := line{{" ", border}}
	if room := inner - 1 - ansi.StringWidth(count) - 1; room > 0 {
		title = append(title, line{{clean(g.Name), boldStyle}}.truncate(room)...)
	}
	title = append(title, seg{count, border})
	title = title.truncate(inner)
	top := slices.Concat(line{{"╭", border}}, title, line{{strings.Repeat("─", max(0, inner-title.width())), border}, {"╮", border}})

	out := []line{top}
	for i := p.first; i < p.first+p.shown; i++ {
		a := g.Agents[i]
		out = append(out, edge(agentLine(a, inner, th, i == selAgent)), edge(detailLine(a, inner)))
	}
	switch {
	case len(g.Agents) == 0:
		out = append(out, edge(line{{"  no agents", dimStyle}}.truncate(inner)))
	case p.hidden > 0:
		out = append(out, edge(line{{"  …" + strconv.Itoa(p.hidden) + " more", dimStyle}}.truncate(inner)))
	}
	bottom := line{{"╰" + strings.Repeat("─", max(0, inner)) + "╯", border}}
	return append(out, bottom)
}

// agentLine is an agent's first line: "◐ pee01   opus ▓▓░░░  38% ". The
// right-hand block is aligned across the card; the name gives way to it.
func agentLine(a snapshot.Agent, inner int, th Thresholds, selected bool) line {
	l := line{{StatusIcon(a.Status), statusStyle(a.Status)}, {" ", plainStyle}}
	var right line
	if inner >= narrowInner {
		model := ShortModel(a.Model)
		right = slices.Concat(
			line{{model + strings.Repeat(" ", max(0, modelCells-ansi.StringWidth(model))), plainStyle}, {" ", plainStyle}},
			bar(a.Ctx, barCells, th),
			line{{" ", plainStyle}, pct(a.Ctx, pctCells, th)},
		)
	}
	room := inner - l.width() - 1
	if right != nil {
		room -= right.width() + 1
	}
	l = append(l, line{{clean(a.Name), plainStyle}}.truncate(room)...)
	if right != nil {
		l = append(l, seg{strings.Repeat(" ", max(1, inner-1-l.width()-right.width())), plainStyle})
		l = append(l, right...)
	}
	l = l.pad(inner)
	if selected {
		for i := range l {
			l[i].style = l[i].style.Reverse(true)
		}
	}
	return l
}

// detailLine is an agent's second line: "  item · stage", either alone, or
// blank.
func detailLine(a snapshot.Agent, inner int) line {
	item, stage := clean(a.Item), clean(a.Stage)
	var d line
	switch {
	case item != "" && stage != "":
		d = line{{item, plainStyle}, {sep, dimStyle}, {stage, dimStyle}}
	case item != "":
		d = line{{item, plainStyle}}
	case stage != "":
		d = line{{stage, dimStyle}}
	}
	return slices.Concat(line{{"  ", plainStyle}}, d.truncate(inner-3))
}

// bar is a ctx gauge, cells wide, coloured by th. A missing ctx is an empty
// gauge.
func bar(ctx *int, cells int, th Thresholds) line {
	if ctx == nil {
		return line{{strings.Repeat("░", cells), dimStyle}}
	}
	v := min(max(*ctx, 0), snapshot.MaxCtx)
	filled := (v*cells + snapshot.MaxCtx/2) / snapshot.MaxCtx
	var l line
	if filled > 0 {
		l = append(l, seg{strings.Repeat("▓", filled), lipgloss.NewStyle().Foreground(th.colour(v))})
	}
	if filled < cells {
		l = append(l, seg{strings.Repeat("░", cells-filled), dimStyle})
	}
	return l
}

// pct is ctx as "38%", right-aligned in width cells; "--" when missing.
func pct(ctx *int, width int, th Thresholds) seg {
	if ctx == nil {
		return seg{padLeft("--", width), dimStyle}
	}
	return seg{padLeft(strconv.Itoa(*ctx)+"%", width), lipgloss.NewStyle().Foreground(th.colour(*ctx))}
}

func padLeft(s string, width int) string {
	return strings.Repeat(" ", max(0, width-ansi.StringWidth(s))) + s
}

// header is " ◆ team · N agents   ◐w working ●i idle ⚠b blocked …   HH:MM".
// When the counts' labels do not fit, the counts drop them ("●6 ◐4 ⚠1")
// before anything is cut.
func header(f Frame, w int) line {
	clock := seg{f.Now.Format("15:04"), dimStyle}
	clockW := ansi.StringWidth(clock.text)
	base := line{{" ", plainStyle}, {"◆", accentStyle}}
	// One cell of margin right of the clock, as left of the diamond, and
	// at least two cells between the counts and the clock.
	if w < clockW+3 {
		return base.truncate(w)
	}
	room := w - 1 - clockW - 2

	body := base
	if s := f.Snapshot; s != nil {
		n, counts := tally(s)
		noun := " agents"
		if n == 1 {
			noun = " agent"
		}
		team := slices.Concat(base, line{
			{" ", plainStyle},
			{clean(s.Team), boldStyle},
			{sep, dimStyle},
			{strconv.Itoa(n) + noun, plainStyle},
			{"   ", plainStyle},
		})
		body = slices.Concat(team, statusCounts(counts, true))
		if body.width() > room {
			body = slices.Concat(team, statusCounts(counts, false))
		}
	}
	body = body.truncate(room)
	gap := w - 1 - body.width() - clockW
	return append(body, seg{strings.Repeat(" ", gap), plainStyle}, clock)
}

// statusCounts is "◐6 working ●4 idle ⚠1 blocked", with labels or without.
// Working, idle and blocked are always counted; done, ready and unknown only
// when some agent is.
func statusCounts(counts map[snapshot.Status]int, labels bool) line {
	statuses := []snapshot.Status{
		snapshot.StatusWorking, snapshot.StatusIdle, snapshot.StatusBlocked,
		snapshot.StatusDone, snapshot.StatusReady, snapshot.StatusUnknown,
	}
	var l line
	for i, st := range statuses {
		c := counts[st]
		if i >= 3 && c == 0 {
			continue
		}
		if i > 0 {
			l = append(l, seg{" ", plainStyle})
		}
		l = append(l, seg{StatusIcon(st) + strconv.Itoa(c), statusStyle(st)})
		if labels {
			l = append(l, seg{" " + statusLabel(st), plainStyle})
		}
	}
	return l
}

// tally counts the team's agents, the boss aside, by status.
func tally(s *snapshot.Snapshot) (int, map[snapshot.Status]int) {
	n := 0
	counts := map[snapshot.Status]int{}
	for _, g := range s.Groups {
		for _, a := range g.Agents {
			n++
			st := a.Status
			if !st.Valid() {
				st = snapshot.StatusUnknown
			}
			counts[st]++
		}
	}
	return n, counts
}

// bossLine is " boss ▸ item · stage · ctx ▓▓░░░░░░ 23%".
func bossLine(b snapshot.Agent, w int, th Thresholds) line {
	l := line{{" ", plainStyle}, {"boss", boldStyle}, {" ▸ ", dimStyle}}
	if item := clean(b.Item); item != "" {
		l = append(l, seg{item, plainStyle}, seg{sep, dimStyle})
	}
	if stage := clean(b.Stage); stage != "" {
		l = append(l, seg{stage, dimStyle}, seg{sep, dimStyle})
	}
	l = append(l, seg{"ctx ", dimStyle})
	l = append(l, bar(b.Ctx, bossBarCells, th)...)
	l = append(l, seg{" ", plainStyle}, pct(b.Ctx, 0, th))
	return l.truncate(w - 1)
}

// footer is the errors, then the note, then the key hints, on one line.
func footer(f Frame, w int) line {
	l := line{{" ", plainStyle}}
	for _, s := range footerParts(f) {
		if len(l) > 1 {
			l = append(l, seg{sep, dimStyle})
		}
		l = append(l, s)
	}
	return l.truncate(w - 1)
}

func footerParts(f Frame) []seg {
	var parts []seg
	for _, err := range f.Errors {
		if err != nil {
			parts = append(parts, seg{"✖ " + clean(err.Error()), errorStyle})
		}
	}
	if f.Note != "" {
		parts = append(parts, seg{clean(f.Note), noteStyle})
	}
	return append(parts, seg{hints, dimStyle})
}

// helpRows are the help overlay's keys and what they do.
var helpRows = [][2]string{
	{"↑ ↓", "move within a card"},
	{"← →", "move across cards"},
	{"enter", "focus and zoom"},
	{"click", "focus and zoom"},
	{"?", "close this help"},
	{"q", "quit"},
}

// helpLegend is the help overlay's status key, two statuses to a row.
var helpLegend = [][2]snapshot.Status{
	{snapshot.StatusWorking, snapshot.StatusIdle},
	{snapshot.StatusBlocked, snapshot.StatusDone},
	{snapshot.StatusReady, snapshot.StatusUnknown},
}

// overlayHelp draws the help box centred over the card area: the keys, then
// what each status glyph means.
func overlayHelp(canvas []line, l layout, w int) {
	const keyCells = 7
	const legendCells = 11
	rows := make([]line, 0, len(helpRows)+len(helpLegend))
	for _, r := range helpRows {
		rows = append(rows, line{
			{" ", plainStyle},
			{r[0] + strings.Repeat(" ", keyCells-ansi.StringWidth(r[0])), boldStyle},
			{r[1] + " ", plainStyle},
		})
	}
	for _, pair := range helpLegend {
		row := line{{" ", plainStyle}}
		for _, st := range pair {
			label := " " + statusLabel(st)
			row = append(row,
				seg{StatusIcon(st), statusStyle(st)},
				seg{label + strings.Repeat(" ", max(0, legendCells-ansi.StringWidth(label))), plainStyle})
		}
		rows = append(rows, row)
	}
	inner := 0
	for _, r := range rows {
		inner = max(inner, r.width())
	}
	title := line{{" ", accentStyle}, {"help", boldStyle}, {" ", accentStyle}}
	box := make([]line, 0, len(rows)+2)
	box = append(box, slices.Concat(line{{"╭", accentStyle}}, title, line{{strings.Repeat("─", inner-title.width()) + "╮", accentStyle}}))
	for _, r := range rows {
		row := append(line{{"│", accentStyle}}, r.pad(inner)...)
		box = append(box, append(row, seg{"│", accentStyle}))
	}
	box = append(box, line{{"╰" + strings.Repeat("─", inner) + "╯", accentStyle}})

	boxW, boxH := inner+2, len(box)
	x := max(0, (w-boxW)/2)
	y := l.top + max(0, (l.avail-boxH)/2)
	if l.avail < boxH {
		y = max(0, (len(canvas)-boxH)/2)
	}
	for i, bl := range box {
		if y+i >= len(canvas) {
			break
		}
		canvas[y+i] = place(canvas[y+i], x, bl.cut(0, w-x))
	}
}

// seg is a run of text drawn in one style. Lines are built from segments so
// widths are measured on plain text and styles are applied once, at the end.
type seg struct {
	text  string
	style lipgloss.Style
}

// line is one screen line.
type line []seg

func (l line) width() int {
	n := 0
	for _, s := range l {
		n += ansi.StringWidth(s.text)
	}
	return n
}

func (l line) render() string {
	var b strings.Builder
	for _, s := range l {
		if s.text != "" {
			b.WriteString(s.style.Render(s.text))
		}
	}
	return b.String()
}

// truncate cuts l to at most w cells, ending in "…" when it cut anything.
func (l line) truncate(w int) line {
	if w <= 0 {
		return nil
	}
	if l.width() <= w {
		return l
	}
	room := w - 1 // the ellipsis takes the last cell
	out := make(line, 0, len(l)+1)
	last := plainStyle
	for _, s := range l {
		last = s.style
		sw := ansi.StringWidth(s.text)
		if sw > room {
			if room > 0 {
				out = append(out, seg{ansi.Truncate(s.text, room, ""), s.style})
			}
			break
		}
		out = append(out, s)
		room -= sw
	}
	return append(out, seg{"…", last})
}

// pad extends l with spaces to w cells.
func (l line) pad(w int) line {
	if gap := w - l.width(); gap > 0 {
		return append(l, seg{strings.Repeat(" ", gap), plainStyle})
	}
	return l
}

// cut returns the cells [from, to) of l.
func (l line) cut(from, to int) line {
	var out line
	col := 0
	for _, s := range l {
		sw := ansi.StringWidth(s.text)
		if a, b := max(from, col), min(to, col+sw); a < b {
			out = append(out, seg{ansi.Cut(s.text, a-col, b-col), s.style})
		}
		col += sw
		if col >= to {
			break
		}
	}
	return out
}

// place draws over base at column x, padding base with spaces to reach it.
func place(base line, x int, over line) line {
	out := base.cut(0, x).pad(x)
	out = append(out, over...)
	return append(out, base.cut(x+over.width(), math.MaxInt)...)
}

// trimRight drops trailing spaces, except from reverse-video segments,
// whose spaces are visible.
func (l line) trimRight() line {
	out := l
	for len(out) > 0 {
		last := out[len(out)-1]
		if last.style.GetReverse() {
			break
		}
		t := strings.TrimRight(last.text, " ")
		if t != "" {
			out = append(out[:len(out)-1:len(out)-1], seg{t, last.style})
			break
		}
		out = out[:len(out)-1]
	}
	return out
}

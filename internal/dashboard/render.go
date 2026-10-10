package dashboard

import (
	"fmt"
	"image/color"
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

// Row geometry, in terminal cells.
const (
	// barCells and pctCells make up an agent's ctx column: "▓▓▓░░  38%".
	barCells = 5
	pctCells = 4
	ctxCells = barCells + 1 + pctCells
	// bossBarCells is the boss line's wider ctx bar.
	bossBarCells = 8
	// modelCells is the widest model ShortModel returns.
	modelCells = 4
	// nameCap and itemCap bound the name and item columns; longer text is
	// cut with "…".
	nameCap = 16
	itemCap = 14
	// stageMin is the narrowest the stage column gets before it drops.
	stageMin = 8
	// colGap separates two columns.
	colGap = 2
	// bossItemCap and bossStageCap bound the boss line's free text, so a
	// long stage cannot push the boss's ctx off the line.
	bossItemCap  = 24
	bossStageCap = 32
)

// Header geometry.
const (
	// boxedHeight is the shortest terminal whose header is a box with a
	// blank line under it: four lines of box, the gap, three lines of
	// groups and the footer. Shorter, the header is two bare lines.
	boxedHeight = 9
	// boxedTop is the first line of the groups under the boxed header.
	boxedTop = 5
	// btopButton is the header's btop button.
	btopButton = "[ btop ]"
	// btopMinText is the narrowest the project line gets beside the button;
	// narrower, the button is not drawn.
	btopMinText = 10
)

const (
	// sep separates the parts of a line.
	sep      = " · "
	waiting  = "waiting for the daemon…"
	noTeam   = "no agents in this team"
	noAgents = "no agents"
	noBoss   = "not running"
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

// Selection addresses one agent: its group's index in display order (see
// Order) and the agent's index within the group.
type Selection struct {
	Group int
	Agent int
}

// Frame is everything one frame draws. Render is a pure function of a Frame
// and the terminal's size.
type Frame struct {
	// Snapshot is nil until the first snapshot arrives.
	Snapshot  *snapshot.Snapshot
	Selection Selection
	// Scroll is the first line of the groups the previous frame showed. The
	// groups stay there unless the selection would leave the screen, so the
	// view does not jump while the selection moves within it. The model
	// keeps the value Render settles on (see Model).
	Scroll int
	// Now is the clock: drawn in the header (HH:MM, in Now's location) and
	// the end of every stage time and of the boss's runtime.
	Now time.Time
	// Help draws the key help over the groups.
	Help bool
	// Btop draws the [ btop ] button and its hint: herdr can open btop.
	Btop bool
	// Note is a standing footer note, such as why focus is off.
	Note string
	// Errors are shown in the footer, first to last. Nil entries are skipped.
	Errors []error
	// Search is set while a query is typed: the footer leads with Query, and
	// a Snapshot with no groups left reads "no matches". Snapshot is already
	// narrowed by the query (Filter).
	Search bool
	// Query is the search query as typed.
	Query string
}

// Render draws f for a w x h terminal: exactly h lines, none wider than w,
// none ending in spaces. A size of zero or less draws nothing.
//
// The header is a box across the terminal, then one blank line; under it
// the groups stack, each a box as tall as its agents with one line per
// agent; the footer is the last line. The groups scroll when they are taller
// than the space between.
func Render(f Frame, w, h int, opts Options) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	th := opts.thresholds()
	l := plan(f, w, h)

	canvas := make([]line, h)
	l.drawHeader(canvas, f, th)
	switch {
	case l.avail == 0:
	case f.Snapshot == nil:
		canvas[l.top] = line{{" ", plainStyle}, {waiting, dimStyle}}
	case len(l.groups) == 0 && f.Search && strings.TrimSpace(f.Query) != "":
		canvas[l.top] = line{{" ", plainStyle}, {noMatches, dimStyle}}
	case len(l.groups) == 0:
		canvas[l.top] = line{{" ", plainStyle}, {noTeam, dimStyle}}
	default:
		for i := range l.avail {
			if l.offset+i >= len(l.rows) {
				break
			}
			canvas[l.top+i] = l.drawRow(l.rows[l.offset+i], th)
		}
	}
	if l.footer >= 0 {
		canvas[l.footer] = footer(f, l)
	}
	if f.Help {
		overlayHelp(canvas, l, f.Btop)
	}

	var b strings.Builder
	for i, ln := range canvas {
		if i > 0 {
			b.WriteByte('\n')
		}
		// The last word on width: at a few columns a box's corners alone
		// are wider than the terminal, and nothing may spill past it.
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

// rowKind is what one line of the groups is.
type rowKind int

const (
	rowTop    rowKind = iota // a box's top border, with the group's title
	rowDesc                  // the group's description
	rowAgent                 // one agent
	rowEmpty                 // "no agents"
	rowBottom                // a box's bottom border
)

// row is one line of the stacked groups.
type row struct {
	kind  rowKind
	group int // index into layout.groups
	agent int // for rowAgent
}

// span is a run of cells on one line, [x0, x1).
type span struct {
	x0, x1, y int
	ok        bool
}

// layout is where everything goes. Render draws it; a click is mapped back
// through it, so drawing and hit-testing can never disagree.
type layout struct {
	w, h   int
	groups []snapshot.Group // display order
	// boxed is the header in a box with a blank line under it; otherwise
	// its lines are bare. line1 and line2 are where they are drawn, -1 for
	// a line that is not.
	boxed        bool
	line1, line2 int
	top, avail   int // the groups are drawn on lines [top, top+avail)
	footer       int // -1 when there is none
	rows         []row
	offset       int // the first row drawn
	sel          Selection
	selOK        bool
	cols         columns
	now          time.Time
	btop         span
}

// plan lays f out on a w x h terminal. The header gives way as h shrinks:
// a box and a gap from boxedHeight lines, two bare lines from 4, one line at
// 3 and 2 (with the footer), and the project line alone at 1.
func plan(f Frame, w, h int) layout {
	l := layout{w: w, h: h, line1: -1, line2: -1, footer: -1, now: f.Now}
	if w <= 0 || h <= 0 {
		return l
	}
	switch {
	case h >= boxedHeight:
		l.boxed, l.line1, l.line2, l.top = true, 1, 2, boxedTop
	case h >= 4:
		l.line1, l.line2, l.top = 0, 1, 2
	default:
		l.line1, l.top = 0, 1
	}
	footerLines := 0
	if h > 1 {
		l.footer, footerLines = h-1, 1
	}
	l.avail = max(0, h-l.top-footerLines)
	if f.Btop {
		l.btop = l.btopSpan()
	}
	if f.Snapshot == nil {
		return l
	}

	l.groups = Order(f.Snapshot.Groups)
	l.sel, l.selOK = f.Selection, validSelection(l.groups, f.Selection)
	l.cols = planColumns(l.groups, f.Now, rowWidth(w))
	selRow, groupTop, groupBottom := -1, 0, 0
	for g, grp := range l.groups {
		top := len(l.rows)
		l.rows = append(l.rows, row{kind: rowTop, group: g})
		if grp.Description != "" {
			l.rows = append(l.rows, row{kind: rowDesc, group: g})
		}
		if len(grp.Agents) == 0 {
			l.rows = append(l.rows, row{kind: rowEmpty, group: g})
		}
		for a := range grp.Agents {
			if l.selOK && l.sel == (Selection{Group: g, Agent: a}) {
				selRow, groupTop = len(l.rows), top
			}
			l.rows = append(l.rows, row{kind: rowAgent, group: g, agent: a})
		}
		l.rows = append(l.rows, row{kind: rowBottom, group: g})
		if l.selOK && l.sel.Group == g {
			groupBottom = len(l.rows) - 1
		}
	}
	l.offset = l.scroll(f.Scroll, selRow, groupTop, groupBottom)
	return l
}

// scroll is the first row to draw: prev, moved just far enough to show the
// selected row (sel, or -1 for none), with its group's title when it is the
// group's first agent and its bottom border when it is the last, and never
// past the end, so the last rows fill the view rather than blank lines.
func (l layout) scroll(prev, sel, groupTop, groupBottom int) int {
	end := max(0, len(l.rows)-l.avail)
	off := min(max(prev, 0), end)
	if sel < 0 || l.avail == 0 {
		return off
	}
	lo, hi := sel-l.avail+1, sel
	if l.sel.Agent == 0 {
		hi = max(lo, groupTop)
	}
	if l.sel.Agent == len(l.groups[l.sel.Group].Agents)-1 {
		lo = min(hi, groupBottom-l.avail+1)
	}
	off = min(max(off, lo), hi)
	// lo <= end always (sel is a row), so clamping to the end keeps the
	// selection in view.
	return min(max(off, 0), end)
}

// btopSpan is where the button goes: the right end of the header's first
// line, inside the box when there is one. It is not drawn when the project
// line beside it would be narrower than btopMinText.
func (l layout) btopSpan() span {
	right := 1 // the bare lines' right margin
	if l.boxed {
		right = 2 // " │"
	}
	x1 := l.w - right
	x0 := x1 - ansi.StringWidth(btopButton)
	if x0-colGap-(l.w-x1) < btopMinText {
		return span{}
	}
	return span{x0: x0, x1: x1, y: l.line1, ok: true}
}

// target is what a click landed on.
type target int

const (
	hitNothing target = iota
	hitAgent
	hitBtop
)

// hit maps a cell to what is drawn there: an agent's row inside its box's
// borders, the btop button, or nothing.
func (l layout) hit(x, y int) (target, Selection) {
	if l.btop.ok && y == l.btop.y && x >= l.btop.x0 && x < l.btop.x1 {
		return hitBtop, Selection{}
	}
	if y < l.top || y >= l.top+l.avail || x < 1 || x > l.w-2 {
		return hitNothing, Selection{}
	}
	i := l.offset + y - l.top
	if i >= len(l.rows) || l.rows[i].kind != rowAgent {
		return hitNothing, Selection{}
	}
	return hitAgent, Selection{Group: l.rows[i].group, Agent: l.rows[i].agent}
}

// outOfView counts the agents above and below what is drawn.
func (l layout) outOfView() (above, below int) {
	for i, r := range l.rows {
		switch {
		case r.kind != rowAgent:
		case i < l.offset:
			above++
		case i >= l.offset+l.avail:
			below++
		}
	}
	return above, below
}

func validSelection(groups []snapshot.Group, s Selection) bool {
	return s.Group >= 0 && s.Group < len(groups) && s.Agent >= 0 && s.Agent < len(groups[s.Group].Agents)
}

// rowWidth is the text width inside a box: its borders and one cell of
// padding each side.
func rowWidth(w int) int {
	return max(0, w-4)
}

// boxed is body between a box's side borders, padded or cut to inner cells.
func boxed(body line, inner int, border lipgloss.Style) line {
	return slices.Concat(line{{"│ ", border}}, body.truncate(inner).pad(inner), line{{" │", border}})
}

// drawHeader draws the project line and the boss line, boxed or bare.
func (l layout) drawHeader(canvas []line, f Frame, th Thresholds) {
	var project *snapshot.Project
	if f.Snapshot != nil {
		project = f.Snapshot.Project
	}
	border := dimStyle
	if c := projectColour(project); c != nil {
		border = lipgloss.NewStyle().Foreground(c)
	}
	margin := 1
	if l.boxed {
		margin = 2
	}
	text := max(0, l.w-2*margin)
	oneW := text
	if l.btop.ok {
		oneW = l.btop.x0 - colGap - margin
	}
	one := projectLine(f.Snapshot, oneW)
	if l.btop.ok {
		one = slices.Concat(one.pad(oneW), line{{strings.Repeat(" ", colGap), plainStyle}, {btopButton, buttonStyle}})
	}
	two := bossLine(f, text, th)

	if l.boxed {
		canvas[0] = line{{"╭" + strings.Repeat("─", max(0, l.w-2)) + "╮", border}}
		canvas[l.line1] = boxed(one, text, border)
		canvas[l.line2] = boxed(two, text, border)
		canvas[3] = line{{"╰" + strings.Repeat("─", max(0, l.w-2)) + "╯", border}}
		return
	}
	canvas[l.line1] = slices.Concat(line{{" ", plainStyle}}, one)
	if l.line2 >= 0 {
		canvas[l.line2] = slices.Concat(line{{" ", plainStyle}}, two)
	}
}

// projectColour is the project's colour, or nil when it has none the
// contract allows.
func projectColour(p *snapshot.Project) color.Color {
	if p == nil || !snapshot.ValidColor(p.Color) {
		return nil
	}
	return lipgloss.Color(p.Color)
}

// prDetail is how much of each PR the project line shows.
type prDetail int

const (
	prFull    prDetail = iota // "#886 ✓👍 clean": CI, Codex and merge state
	prCompact                 // "#886✓👍": CI and Codex
	prCount                   // "PRs 5"
)

// projectLine is "agentisan · PRs #878 ⏳… blocked  #886 ✓👍 clean · issues
// 14 · slots 2/2", at most width cells. The PRs give way first: to their
// compact form, then to a count. What the snapshot does not report is not
// shown, never shown as zero.
func projectLine(s *snapshot.Snapshot, width int) line {
	if s == nil || width <= 0 {
		return nil
	}
	nameStyle := boldStyle
	if c := projectColour(s.Project); c != nil {
		nameStyle = nameStyle.Foreground(c)
	}
	name := line{{clean(s.Team), nameStyle}}
	p := s.Project
	if p == nil {
		return name.truncate(width)
	}
	var tail []line
	if p.Issues != nil {
		tail = append(tail, line{{"issues " + strconv.Itoa(*p.Issues), plainStyle}})
	}
	if p.Slots != nil {
		st := plainStyle
		if p.Slots.Total > 0 && p.Slots.Used >= p.Slots.Total {
			st = noteStyle // no slot free
		}
		tail = append(tail, line{{fmt.Sprintf("slots %d/%d", p.Slots.Used, p.Slots.Total), st}})
	}
	var out line
	for _, d := range []prDetail{prFull, prCompact, prCount} {
		parts := []line{name}
		if prs := prsLine(p.PRs, d); prs != nil {
			parts = append(parts, prs)
		}
		out = joinParts(append(parts, tail...))
		if out.width() <= width {
			break
		}
	}
	return out.truncate(width)
}

// prsLine is the PRs at detail d; nil when they are not reported.
func prsLine(prs []snapshot.PR, d prDetail) line {
	switch {
	case prs == nil:
		return nil
	case len(prs) == 0:
		return line{{"PRs none", plainStyle}}
	case d == prCount:
		return line{{"PRs " + strconv.Itoa(len(prs)), plainStyle}}
	}
	out := line{{"PRs ", plainStyle}}
	for i, pr := range prs {
		if i > 0 {
			gap := " "
			if d == prFull {
				gap = "  "
			}
			out = append(out, seg{gap, plainStyle})
		}
		num := "#" + strconv.Itoa(pr.Number)
		if d == prFull {
			num += " "
		}
		out = append(out, seg{num, plainStyle}, ciGlyph(pr.CI), codexGlyph(pr.Codex))
		if d == prFull {
			out = append(out, seg{" ", plainStyle}, mergeWord(pr.Merge))
		}
	}
	return out
}

// joinParts joins parts with sep.
func joinParts(parts []line) line {
	var out line
	for i, p := range parts {
		if i > 0 {
			out = append(out, seg{sep, dimStyle})
		}
		out = append(out, p...)
	}
	return out
}

// bossLine is "boss ▸ ◐ working · #867 release · review · watchers 4 · ctx
// ▓▓░░░░░░ 23% · up 2h14m", with the clock at the right, width cells in
// all. Watchers give way first, then the runtime, the stage and the item;
// the boss's state and ctx stay.
func bossLine(f Frame, width int, th Thresholds) line {
	if width <= 0 {
		return nil
	}
	clock := seg{f.Now.Format("15:04"), dimStyle}
	left := bossParts(f, th)
	clockW := ansi.StringWidth(clock.text)
	room := width - clockW - colGap
	if room < 1 {
		return joinParts(left.keep(-1)).truncate(width)
	}
	body := joinParts(left.keep(-1))
	for drop := 0; body.width() > room && drop < len(bossDropOrder); drop++ {
		body = joinParts(left.keep(drop))
	}
	body = body.truncate(room)
	return slices.Concat(body, line{{strings.Repeat(" ", width-body.width()-clockW), plainStyle}, clock})
}

// bossPart names a part of the boss line that can give way.
type bossPart int

const (
	partFixed bossPart = iota // never dropped
	partItem
	partStage
	partWatchers
	partRuntime
)

// bossDropOrder is the order the boss line's parts give way.
var bossDropOrder = []bossPart{partWatchers, partRuntime, partStage, partItem}

// piece is one part of the boss line and what kind of part it is.
type piece struct {
	kind bossPart
	l    line
}

// parts are the boss line's parts, in order.
type parts []piece

// keep is the parts left once the first drop+1 of bossDropOrder have
// dropped; drop -1 keeps them all.
func (p parts) keep(drop int) []line {
	var out []line
	for _, part := range p {
		if slices.Contains(bossDropOrder[:drop+1], part.kind) {
			continue
		}
		out = append(out, part.l)
	}
	return out
}

func bossParts(f Frame, th Thresholds) parts {
	if f.Snapshot == nil {
		return nil
	}
	head := line{{"boss", boldStyle}, {" ▸ ", dimStyle}}
	b := f.Snapshot.Boss
	if b == nil {
		return parts{{partFixed, append(head, seg{noBoss, dimStyle})}}
	}
	out := parts{{partFixed, append(head, seg{StatusIcon(b.Status) + " " + statusLabel(b.Status), statusStyle(b.Status)})}}
	if item := clean(b.Item); item != "" {
		out = append(out, piece{partItem, line{{item, plainStyle}}.truncate(bossItemCap)})
	}
	if stage := clean(b.Stage); stage != "" {
		out = append(out, piece{partStage, line{{stage, dimStyle}}.truncate(bossStageCap)})
	}
	if b.Watchers != nil {
		out = append(out, piece{partWatchers, line{{"watchers " + strconv.Itoa(*b.Watchers), plainStyle}}})
	}
	ctx := slices.Concat(line{{"ctx ", dimStyle}}, bar(b.Ctx, bossBarCells, th), line{{" ", plainStyle}, pct(b.Ctx, 0, th)})
	out = append(out, piece{partFixed, ctx})
	if up := Elapsed(b.StartedAt, f.Now); up != "" {
		out = append(out, piece{partRuntime, line{{"up " + up, plainStyle}}})
	}
	return out
}

// titleOrder is the order a group's title counts its statuses.
var titleOrder = []snapshot.Status{
	snapshot.StatusDone, snapshot.StatusWorking, snapshot.StatusIdle,
	snapshot.StatusBlocked, snapshot.StatusReady, snapshot.StatusUnknown,
}

// groupTitle is "CODERS · 9 ✔1 ◐4 ●2 ⚠1": the name, the agent count, and
// each status some agent has.
func groupTitle(g snapshot.Group) line {
	out := line{{strings.ToUpper(clean(g.Name)), boldStyle}, {sep + strconv.Itoa(len(g.Agents)), plainStyle}}
	counts := statusCounts(g.Agents)
	for _, st := range titleOrder {
		if c := counts[st]; c > 0 {
			out = append(out, seg{" ", plainStyle}, seg{StatusIcon(st) + strconv.Itoa(c), statusStyle(st)})
		}
	}
	return out
}

// statusCounts counts agents by status; an invalid status counts as unknown.
func statusCounts(agents []snapshot.Agent) map[snapshot.Status]int {
	counts := map[snapshot.Status]int{}
	for _, a := range agents {
		st := a.Status
		if !st.Valid() {
			st = snapshot.StatusUnknown
		}
		counts[st]++
	}
	return counts
}

// borderStyle is a group's box: red when an agent in it is blocked, the
// accent when it holds the selection, dim otherwise.
func (l layout) borderStyle(g int) lipgloss.Style {
	switch {
	case statusCounts(l.groups[g].Agents)[snapshot.StatusBlocked] > 0:
		return errorStyle
	case l.selOK && l.sel.Group == g:
		return accentStyle
	default:
		return dimStyle
	}
}

// drawRow draws one line of the groups, w cells wide.
func (l layout) drawRow(r row, th Thresholds) line {
	g := l.groups[r.group]
	border := l.borderStyle(r.group)
	inner := rowWidth(l.w)
	switch r.kind {
	case rowTop:
		between := max(0, l.w-2)
		title := slices.Concat(line{{"─ ", border}}, groupTitle(g), line{{" ", border}}).truncate(between)
		return slices.Concat(line{{"╭", border}}, title, line{{strings.Repeat("─", max(0, between-title.width())) + "╮", border}})
	case rowBottom:
		return line{{"╰" + strings.Repeat("─", max(0, l.w-2)) + "╯", border}}
	case rowDesc:
		return boxed(line{{clean(g.Description), dimStyle}}, inner, border)
	case rowEmpty:
		return boxed(line{{noAgents, dimStyle}}, inner, border)
	default:
		body := l.cols.row(g.Agents[r.agent], l.now, th).truncate(inner).pad(inner)
		if l.selOK && l.sel == (Selection{Group: r.group, Agent: r.agent}) {
			for i := range body {
				body[i].style = body[i].style.Reverse(true)
			}
		}
		return boxed(body, inner, border)
	}
}

// column is one of an agent row's optional columns. Glyph, name and ctx are
// always drawn.
type column int

const (
	colState column = iota
	colModel
	colItem
	colStage
	colTime
	numColumns
)

// dropOrder is the order the optional columns give way as the terminal
// narrows: from the row's right edge leftwards, then, past ctx, which
// stays, the model and the state.
var dropOrder = []column{colTime, colStage, colItem, colModel, colState}

// columns are the widths of an agent row's columns, shared by every row of
// every group so the columns align from box to box. A width of zero is a
// column not drawn: dropped, or empty in every row.
type columns struct {
	name  int
	width [numColumns]int
}

// need is the row width the columns take, the stage at its narrowest.
func (c columns) need() int {
	n := 2 + c.name + colGap + ctxCells // glyph, space, name, ctx
	for col, w := range c.width {
		if w == 0 {
			continue
		}
		if column(col) == colStage {
			w = stageMin
		}
		n += colGap + w
	}
	return n
}

// planColumns sizes the columns to the widest value in each, within its cap,
// then drops columns in dropOrder until the row fits inner cells, then cuts
// the name. The stage takes whatever width is left.
func planColumns(groups []snapshot.Group, now time.Time, inner int) columns {
	var c columns
	stage := false
	for _, g := range groups {
		for _, a := range g.Agents {
			c.name = max(c.name, ansi.StringWidth(clean(a.Name)))
			c.width[colState] = max(c.width[colState], ansi.StringWidth(statusLabel(a.Status)))
			c.width[colModel] = max(c.width[colModel], ansi.StringWidth(ShortModel(a.Model)))
			c.width[colItem] = max(c.width[colItem], ansi.StringWidth(clean(a.Item)))
			c.width[colTime] = max(c.width[colTime], ansi.StringWidth(Elapsed(a.StageStartedAt, now)))
			stage = stage || clean(a.Stage) != ""
		}
	}
	c.name = min(max(c.name, 1), nameCap)
	c.width[colItem] = min(c.width[colItem], itemCap)
	if stage {
		c.width[colStage] = stageMin
	}
	for _, col := range dropOrder {
		if c.need() <= inner {
			break
		}
		c.width[col] = 0
	}
	if over := c.need() - inner; over > 0 {
		c.name = max(1, c.name-over)
	}
	if c.width[colStage] > 0 {
		c.width[colStage] = stageMin + max(0, inner-c.need())
	}
	return c
}

// row is one agent's line: "◐ pee04  working  opus  ▓▓▓░░  61%  NERD-5255
// go-review  3m", each column cut with "…" to its width.
func (c columns) row(a snapshot.Agent, now time.Time, th Thresholds) line {
	gap := seg{strings.Repeat(" ", colGap), plainStyle}
	out := slices.Concat(line{{StatusIcon(a.Status), statusStyle(a.Status)}, {" ", plainStyle}}, cell(clean(a.Name), boldStyle, c.name))
	if w := c.width[colState]; w > 0 {
		out = slices.Concat(out, line{gap}, cell(statusLabel(a.Status), statusStyle(a.Status), w))
	}
	if w := c.width[colModel]; w > 0 {
		out = slices.Concat(out, line{gap}, cell(ShortModel(a.Model), plainStyle, w))
	}
	out = slices.Concat(out, line{gap}, bar(a.Ctx, barCells, th), line{{" ", plainStyle}, pct(a.Ctx, pctCells, th)})
	if w := c.width[colItem]; w > 0 {
		out = slices.Concat(out, line{gap}, cell(clean(a.Item), plainStyle, w))
	}
	if w := c.width[colStage]; w > 0 {
		out = slices.Concat(out, line{gap}, cell(clean(a.Stage), dimStyle, w))
	}
	if w := c.width[colTime]; w > 0 {
		out = slices.Concat(out, line{gap, {padLeft(Elapsed(a.StageStartedAt, now), w), dimStyle}})
	}
	return out
}

// cell is text cut to w cells, ending in "…" when cut, then padded to w.
func cell(text string, st lipgloss.Style, w int) line {
	return line{{text, st}}.truncate(w).pad(w)
}

// Elapsed is the time from from to now as one short token: "12m", "1h05m",
// "3d04h", or ">99d" from 100 days. It is empty when from is nil or after
// now: a clock ahead of the frame's has spent no time in the stage yet.
func Elapsed(from *time.Time, now time.Time) string {
	if from == nil || from.After(now) {
		return ""
	}
	const day = 24 * time.Hour
	d := now.Sub(*from) // saturates, never wraps, for times centuries apart
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < day:
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	case d < 100*day:
		return fmt.Sprintf("%dd%02dh", d/day, d%day/time.Hour)
	default:
		return ">99d"
	}
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

// footer is the errors, then the note, then the key hints, on one line, and
// at its right the count of agents out of view when the groups scroll.
func footer(f Frame, l layout) line {
	left := line{{" ", plainStyle}}
	for _, s := range footerParts(f) {
		if len(left) > 1 {
			left = append(left, seg{sep, dimStyle})
		}
		left = append(left, s)
	}
	above, below := l.outOfView()
	var marks []string
	if above > 0 {
		marks = append(marks, "↑"+strconv.Itoa(above))
	}
	if below > 0 {
		marks = append(marks, "↓"+strconv.Itoa(below))
	}
	mark := strings.Join(marks, " ")
	markW := ansi.StringWidth(mark)
	// One cell of margin at the right, as at the left, and one between.
	if mark == "" || l.w-1-markW-1 < 1 {
		return left.truncate(l.w - 1)
	}
	room := l.w - 1 - markW - 1
	return slices.Concat(left.truncate(room).pad(l.w-1-markW), line{{mark, dimStyle}})
}

// searchHints are the keys that work while a query is typed.
const searchHints = "enter jump · ↑↓ move · esc clear"

// hints are the keys that work, as the footer names them. ctrl-b m is
// herdr's own popup key for btop in this setup, named only where herdr is
// reachable.
func hints(btop bool) string {
	if btop {
		return "enter jump · ↑↓ move · ctrl-b m btop · ? help · q quit"
	}
	return "enter jump · ↑↓ move · ? help · q quit"
}

func footerParts(f Frame) []seg {
	var parts []seg
	if f.Search {
		// The query leads, so errors never push it off the line.
		parts = append(parts, seg{"/" + clean(f.Query) + "▏", boldStyle})
	}
	for _, err := range f.Errors {
		if err != nil {
			parts = append(parts, seg{"✖ " + clean(err.Error()), errorStyle})
		}
	}
	if f.Note != "" {
		parts = append(parts, seg{clean(f.Note), noteStyle})
	}
	if f.Search {
		return append(parts, seg{searchHints, dimStyle})
	}
	return append(parts, seg{hints(f.Btop), dimStyle})
}

// helpRows are the help overlay's keys and what they do: only keys that
// work, and the btop button only where it is drawn.
func helpRows(btop bool) [][2]string {
	rows := [][2]string{
		{"↑ ↓", "move through the agents"},
		{keyEnter, "jump: focus and zoom"},
		{"click", "jump: focus and zoom"},
		{"/", "search name, group, item, stage"},
	}
	if btop {
		rows = append(rows, [2]string{btopButton, "btop in a herdr popup"})
	}
	return append(rows, [2]string{"?", "close this help"}, [2]string{"q", "quit"})
}

// helpLegend is the help overlay's status key, two statuses to a row.
var helpLegend = [][2]snapshot.Status{
	{snapshot.StatusWorking, snapshot.StatusIdle},
	{snapshot.StatusBlocked, snapshot.StatusDone},
	{snapshot.StatusReady, snapshot.StatusUnknown},
}

// overlayHelp draws the help box centred over the groups: the keys, then
// what each status glyph means.
func overlayHelp(canvas []line, l layout, btop bool) {
	const keyCells = 10
	const legendCells = 11
	keys := helpRows(btop)
	rows := make([]line, 0, len(keys)+len(helpLegend))
	for _, r := range keys {
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
	x := max(0, (l.w-boxW)/2)
	y := l.top + max(0, (l.avail-boxH)/2)
	if l.avail < boxH {
		y = max(0, (len(canvas)-boxH)/2)
	}
	for i, bl := range box {
		if y+i >= len(canvas) {
			break
		}
		canvas[y+i] = place(canvas[y+i], x, bl.cut(0, l.w-x))
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

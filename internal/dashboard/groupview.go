package dashboard

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// The group view (ADR-001 A37): 1-9, or a click on a group's title, opens
// one group as a box per agent, with what the agent reports and the daemon
// times: model, ctx, item, the whole stage, time in stage, time at rest, and
// the pane and tab it runs in. Nothing in it is read from a screen (D4): the
// stage each agent reports takes the place of v12's screen tail. Esc, ← or a
// click on ◀ back returns to the overview; Enter or a click jumps, as there.

// Group view geometry, in terminal cells.
const (
	// boxHeight is an agent box: its two borders and six lines (the model
	// and ctx, the item, two of stage, the times, the ids).
	boxHeight = 8
	// twoColumns is the narrowest terminal that draws the boxes two to a
	// row, as v12 did.
	twoColumns = 110
	// boxBarCells is a box's ctx gauge.
	boxBarCells = 10
	// stageLines is how many lines of a box the stage wraps to.
	stageLines = 2
)

const (
	// backButton is the view's way back, at the left of its title line.
	backButton = "◀ back"
	// noStage stands in for a stage the agent has not reported, so a missing
	// report shows: the team rule is to report one.
	noStage = "no stage reported"
	// readyBadge marks an agent ready to restart, at a box's top right.
	readyBadge = "⟳ READY"
)

// readyStyle is the ready badge: the ready status's colour, in bold.
var readyStyle = statusStyle(snapshot.StatusReady).Bold(true)

// groupView is where the open group's view goes. Its boxes are laid out in
// lines of their own: line 0 is the top of the first row of boxes, and only
// [offset, offset+avail) of them is drawn, on the screen lines [top,
// top+avail).
type groupView struct {
	group  int // index into layout.groups
	agents []snapshot.Agent
	// desc is the screen line of the group's description, -1 when it has
	// none or there is no room for it.
	desc       int
	top, avail int
	// cols is 1 or 2; x0 and width are each column's box.
	cols      int
	x0, width [2]int
	offset    int
	back      span
	// sel is the selected agent's index in the group, -1 for none.
	sel int
}

// groupIndex is the index of the first group named name, or -1.
func groupIndex(groups []snapshot.Group, name string) int {
	return slices.IndexFunc(groups, func(g snapshot.Group) bool { return g.Name == name })
}

// planView lays out group g's view in l's body: the title line, then the
// description when there is one and room for it, then the boxes, scrolled
// from prev just far enough to show the selected box whole, or its top when
// it is taller than the room.
func planView(l layout, g, prev int) *groupView {
	grp := l.groups[g]
	v := &groupView{group: g, agents: grp.Agents, desc: -1, cols: 1, sel: -1}
	v.width[0] = l.w
	if l.w >= twoColumns && len(grp.Agents) > 1 {
		w := (l.w - 1) / 2
		v.cols, v.x0, v.width = 2, [2]int{0, l.w - w}, [2]int{w, w}
	}
	if l.selOK && l.sel.Group == g {
		v.sel = l.sel.Agent
	}
	if l.avail == 0 {
		return v
	}
	x1 := 1 + ansi.StringWidth(backButton)
	v.back = span{x0: 1, x1: x1, y: l.top, ok: x1 <= l.w}
	next := l.top + 1
	end := l.top + l.avail
	if grp.Description != "" && next < end {
		v.desc, next = next, next+1
	}
	v.top, v.avail = next, max(0, end-next)
	v.offset = v.scroll(prev)
	return v
}

// lines is how many lines the boxes take.
func (v *groupView) lines() int {
	return (len(v.agents) + v.cols - 1) / v.cols * boxHeight
}

// boxTop is the first line of agent i's box.
func (v *groupView) boxTop(i int) int {
	return i / v.cols * boxHeight
}

// scroll is the first line of the boxes to draw: prev, moved just far enough
// to show the selected box, and never past the end.
func (v *groupView) scroll(prev int) int {
	end := max(0, v.lines()-v.avail)
	off := min(max(prev, 0), end)
	if v.sel < 0 || v.avail == 0 {
		return off
	}
	y0 := v.boxTop(v.sel)
	// A box taller than the room shows its top: hi wins over lo.
	off = min(max(off, y0+boxHeight-v.avail), y0)
	return min(max(off, 0), end)
}

// hit maps a cell to what the view draws there: ◀ back, an agent's box, or
// nothing.
func (v *groupView) hit(x, y int) (target, Selection) {
	if v.back.ok && y == v.back.y && x >= v.back.x0 && x < v.back.x1 {
		return hitBack, Selection{}
	}
	if y < v.top || y >= v.top+v.avail {
		return hitNothing, Selection{}
	}
	ln := v.offset + y - v.top
	if ln >= v.lines() {
		return hitNothing, Selection{}
	}
	for c := range v.cols {
		if x < v.x0[c] || x >= v.x0[c]+v.width[c] {
			continue
		}
		if i := ln/boxHeight*v.cols + c; i < len(v.agents) {
			return hitAgent, Selection{Group: v.group, Agent: i}
		}
	}
	return hitNothing, Selection{}
}

// outOfView counts the agents whose boxes are wholly above and below what
// is drawn.
func (v *groupView) outOfView() (above, below int) {
	for i := range v.agents {
		switch y0 := v.boxTop(i); {
		case y0+boxHeight <= v.offset:
			above++
		case y0 >= v.offset+v.avail:
			below++
		}
	}
	return above, below
}

// drawView draws the open group's view into the body of canvas.
func (l layout) drawView(canvas []line, th Thresholds) {
	v := l.view
	if l.avail == 0 {
		return
	}
	grp := l.groups[v.group]
	canvas[l.top] = slices.Concat(line{{" ", plainStyle}, {backButton, accentStyle.Bold(true)}, {"  ", plainStyle}}, groupTitle(grp))
	if v.desc >= 0 {
		canvas[v.desc] = line{{" ", plainStyle}, {clean(grp.Description), dimStyle}}
	}
	if v.avail == 0 {
		return
	}
	if len(v.agents) == 0 {
		canvas[v.top] = line{{" ", plainStyle}, {noAgents, dimStyle}}
		return
	}
	for i := range v.avail {
		ln := v.offset + i
		if ln >= v.lines() {
			break
		}
		var out line
		for c := range v.cols {
			if a := ln/boxHeight*v.cols + c; a < len(v.agents) {
				out = place(out, v.x0[c], l.boxLine(a, ln%boxHeight, v.width[c], th))
			}
		}
		canvas[v.top+i] = out
	}
}

// boxLine is line k of agent i's box, w cells wide.
func (l layout) boxLine(i, k, w int, th Thresholds) line {
	v := l.view
	a := v.agents[i]
	sel := i == v.sel
	border := dimStyle
	switch {
	case a.Status == snapshot.StatusBlocked:
		border = errorStyle
	case sel:
		border = accentStyle
	}
	inner := rowWidth(w)
	var body line
	switch k {
	case 0:
		return boxTitleLine(a, w, sel, border)
	case boxHeight - 1:
		return line{{"╰" + strings.Repeat("─", max(0, w-2)) + "╯", border}}
	case 1:
		body = modelAndCtx(a, th)
	case 2:
		body = line{{clean(a.Item), plainStyle}}
	case 3, 4:
		body = wrapStage(a.Stage, inner)[k-3]
	case 5:
		body = boxTimes(a, l)
	default:
		body = boxIDs(a)
	}
	return boxed(body, inner, border)
}

// boxTitleLine is a box's top border: "╭─ ◐ pee04 · working ───╮", with the
// ready badge at its right when the agent is ready to restart. The badge
// gives way first, then the title is cut.
func boxTitleLine(a snapshot.Agent, w int, sel bool, border lipgloss.Style) line {
	between := max(0, w-2)
	title := line{
		{StatusIcon(a.Status), statusStyle(a.Status)},
		{" ", plainStyle},
		{clean(a.Name), boldStyle},
		{sep, dimStyle},
		{statusLabel(a.Status), statusStyle(a.Status)},
	}
	if sel {
		for i := range title {
			title[i].style = title[i].style.Reverse(true)
		}
	}
	head := slices.Concat(line{{"─ ", border}}, title, line{{" ", border}})
	var badge line
	if a.Status == snapshot.StatusReady {
		badge = line{{" ", border}, {readyBadge, readyStyle}, {" ─", border}}
	}
	if head.width()+badge.width() > between {
		badge = nil
	}
	head = head.truncate(between)
	fill := strings.Repeat("─", max(0, between-head.width()-badge.width()))
	return slices.Concat(line{{"╭", border}}, head, line{{fill, border}}, badge, line{{"╮", border}})
}

// modelAndCtx is "opus  ctx ▓▓▓▓░░░░░░ 38%": the model when it is reported
// (G1), then the ctx gauge.
func modelAndCtx(a snapshot.Agent, th Thresholds) line {
	var out line
	if m := ShortModel(a.Model); m != "" {
		out = line{{m, plainStyle}, {strings.Repeat(" ", colGap), plainStyle}}
	}
	return slices.Concat(out, line{{"ctx ", dimStyle}}, bar(a.Ctx, boxBarCells, th), line{{" ", plainStyle}, pct(a.Ctx, 0, th)})
}

// wrapStage is the stage as stageLines lines of at most w cells, wrapped at
// words, the last ending in "…" when the stage is longer. A stage not
// reported is noStage, dim, so the gap shows.
func wrapStage(stage string, w int) [stageLines]line {
	var out [stageLines]line
	s := strings.Join(strings.Fields(clean(stage)), " ")
	if s == "" {
		out[0] = line{{noStage, dimStyle}}
		return out
	}
	if w <= 0 {
		return out
	}
	parts := strings.Split(ansi.Wrap(s, w, ""), "\n")
	for i := range min(len(parts), stageLines-1) {
		out[i] = line{{strings.TrimRight(parts[i], " "), plainStyle}}
	}
	if len(parts) >= stageLines {
		rest := strings.Join(strings.Fields(strings.Join(parts[stageLines-1:], " ")), " ")
		out[stageLines-1] = line{{rest, plainStyle}}.truncate(w)
	}
	return out
}

// boxTimes is "in stage 12m · idle 3m": each part only when it is known.
func boxTimes(a snapshot.Agent, l layout) line {
	var parts []line
	if e := Elapsed(a.StageStartedAt, l.now); e != "" {
		parts = append(parts, line{{"in stage " + e, plainStyle}})
	}
	if e := Elapsed(a.IdleSince, l.now); e != "" {
		parts = append(parts, line{{"idle " + e, plainStyle}})
	}
	return joinParts(parts)
}

// boxIDs is "pane w2:p4 · tab w2:t1", dim: each part only when it is known.
func boxIDs(a snapshot.Agent) line {
	var parts []line
	if a.PaneID != "" {
		parts = append(parts, line{{"pane " + clean(a.PaneID), dimStyle}})
	}
	if a.TabID != "" {
		parts = append(parts, line{{"tab " + clean(a.TabID), dimStyle}})
	}
	return joinParts(parts)
}

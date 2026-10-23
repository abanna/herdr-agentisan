// Package dashboard is the team dashboard (ADR-001 D5): a bubbletea program
// that draws, from snapshots, a header box (the project's PRs, issues and
// test slots; the boss), then one box per group stacked under it with one
// line per agent. Enter or a click focuses an agent; / narrows the agents to
// a search query (search.go); 1-9, or a click on a group's title, opens that
// group's view, a box per agent (groupview.go); a click on [ btop ] opens
// btop in a herdr popup.
//
// The model polls a snapshot.Source on a tea.Tick. Its View is Render applied
// to the model's Frame and size, a pure function, so frames are golden-tested
// as text. Clicks are mapped back through the same layout Render draws.
package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// Defaults for Config's zero values.
const (
	// DefaultInterval is how often the source is polled.
	DefaultInterval = time.Second
	// DefaultFocusTimeout bounds one herdr action: a focus (agent.focus plus
	// pane.zoom), or opening btop.
	DefaultFocusTimeout = 3 * time.Second
	// DefaultFetchTimeout bounds one poll, so a hung source shows up as an
	// error instead of a dashboard that silently stops refreshing.
	DefaultFetchTimeout = 5 * time.Second
)

// fallbackWidth and fallbackHeight size the frame until the terminal reports
// its size, and for good when it never does (output that is not a terminal).
const (
	fallbackWidth  = 80
	fallbackHeight = 24
)

// Sentinel errors. Callers branch on these with errors.Is.
var (
	// ErrNoSource means no snapshot source was given.
	ErrNoSource = errors.New("no snapshot source")
	// ErrTerminal means the terminal program failed.
	ErrTerminal = errors.New("dashboard terminal failed")
)

// noter is a source with a footer note of its own, read after each poll.
type noter interface {
	Note() string
}

// Config is what a dashboard needs.
type Config struct {
	// Source is polled for snapshots.
	Source snapshot.Source
	// Board supplies the board panel; nil means no board with this fixture.
	Board BoardSource
	// Focuser is called on Enter and a click. Nil turns focus off; say why
	// in Note.
	Focuser Focuser
	// Btop is called on a click of [ btop ]. Nil draws neither the button
	// nor its hint.
	Btop BtopOpener
	// Interval between polls. Zero means DefaultInterval.
	Interval time.Duration
	// FocusTimeout bounds one focus, and one btop open. Zero means
	// DefaultFocusTimeout.
	FocusTimeout time.Duration
	// FetchTimeout bounds one poll. Zero means DefaultFetchTimeout.
	FetchTimeout time.Duration
	// Now is the header clock. Nil means time.Now.
	Now func() time.Time
	// Options tune the renderer.
	Options Options
	// Note is a standing footer note.
	Note string
}

// HerdrConfig is the dashboard herdr runs: it reads src, and focuses and
// opens btop through the herdr socket at socketPath. Without a socket both
// are off, and the footer says why.
func HerdrConfig(src snapshot.Source, socketPath string) Config {
	cfg := Config{Source: src}
	if socketPath == "" {
		cfg.Note = "focus off: HERDR_SOCKET_PATH is not set"
		return cfg
	}
	client := herdr.Client{SocketPath: socketPath}
	cfg.Focuser = HerdrFocuser{Client: client}
	cfg.Btop = HerdrBtop{Client: client}
	return cfg
}

// Where says which team a daemon dashboard shows (ADR-001 A24): Project's
// when set; else the team of the workspace its own pane is in, which it
// finds by PID through herdr, with PaneID (HERDR_PANE_ID) as its first
// guess and WorkspaceID (HERDR_WORKSPACE_ID) for when herdr cannot be asked.
type Where struct {
	Project     string
	PID         int
	PaneID      string
	WorkspaceID string
}

// DaemonConfig is HerdrConfig reading the snapshots of the team where
// selects from the daemon of the herdr server at herdrSocket, whose state
// lives under stateDir: the socket `daemon health` asks (daemon.PathsFor).
// A state dir or herdr socket that names no daemon socket is the caller's
// mistake, reported before the terminal is taken over; a daemon that is not
// running is not, and shows in the footer while the dashboard keeps polling.
func DaemonConfig(stateDir, herdrSocket string, where Where) (Config, error) {
	paths, err := daemon.PathsFor(stateDir, herdrSocket)
	if err != nil {
		return Config{}, fmt.Errorf("daemon socket: %w", err)
	}
	src := &TeamSource{Socket: paths.Socket, Project: where.Project}
	if where.Project == "" {
		src.Locator = &Locator{
			Herdr: herdr.Client{SocketPath: herdrSocket}, PID: where.PID,
			PaneID: where.PaneID, WorkspaceID: where.WorkspaceID,
		}
	}
	cfg := HerdrConfig(src, herdrSocket)
	cfg.Board = DaemonBoard{Socket: paths.Socket}
	return cfg, nil
}

// FixtureConfig is HerdrConfig reading snapshots from the JSON file at path,
// again on every poll. The file is read once first: a fixture that cannot be
// read is the caller's mistake, reported before the terminal is taken over,
// not a footer to squint at.
func FixtureConfig(ctx context.Context, path, boardPath, socketPath string) (Config, error) {
	if path == "" {
		return Config{}, ErrNoSource
	}
	src := snapshot.FileSource{Path: path}
	if _, err := src.Snapshot(ctx); err != nil {
		return Config{}, fmt.Errorf("fixture: %w", err)
	}
	cfg := HerdrConfig(src, socketPath)
	if boardPath != "" {
		board := &FileBoard{Path: boardPath}
		if _, err := board.Board(ctx); err != nil {
			return Config{}, err
		}
		cfg.Board = board
		cfg.Source = fixtureBoardSource{Source: src, board: board}
	}
	return cfg, nil
}

// Model is the dashboard's bubbletea model.
type Model struct {
	// ctx bounds every poll and focus the model starts; it is the
	// program's context, held because tea.Cmd takes none.
	ctx context.Context
	cfg Config

	// raw is the last snapshot as it arrived; snap is the one drawn: raw,
	// narrowed by the query while one is typed (search.go).
	raw    *snapshot.Snapshot
	snap   *snapshot.Snapshot
	search search
	sel    Selection
	// anchor is the selected row's identity (rowKey). The selection follows
	// the row, not its position, across refreshes.
	anchor rowKey
	// scroll is the first line of the groups the last frame drew (see
	// Frame.Scroll). It is settled after every change that can move the
	// selection or the layout, so the frame a click lands on is the frame
	// that was drawn.
	scroll int

	clock         time.Time
	width, height int
	help          bool
	sourceErr     error
	focusErr      error
	btopErr       error
	// noTeam is set while the source says there is no team here
	// (ErrNoTeamHere): no board is drawn then, not the last one.
	noTeam error
	// sourceNote is the source's own footer note, such as herdr not
	// answering where the dashboard runs.
	sourceNote string
	// group names the group whose view is open, "" for the overview. The
	// view follows the group by name across refreshes; while it is open
	// the selection stays in that group.
	group string
	// groupNote says the open group's view closed because the group is
	// gone. The next key or click clears it.
	groupNote string
	board     boardPanel
}

// The arrow keys, as tea.KeyPressMsg.String names them. Left leaves a
// group's view, and is unbound on the overview, as right is everywhere.
const (
	keyUp   = "up"
	keyDown = "down"
	keyLeft = "left"
)

// keyEnter is Enter, as tea.KeyPressMsg.String names it: the jump.
const (
	keyEnter = "enter"
	keyEsc   = "esc"
	keyQuit  = "ctrl+c"
)

// Messages the model sends itself.
type (
	snapshotMsg struct {
		snap snapshot.Snapshot
		err  error
		note string
	}
	tickMsg  struct{}
	focusMsg struct {
		agent snapshot.Agent
		err   error
	}
	btopMsg struct {
		err error
	}
)

// New builds a model. ctx bounds the polls and focuses it starts.
func New(ctx context.Context, cfg Config) Model {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.FocusTimeout <= 0 {
		cfg.FocusTimeout = DefaultFocusTimeout
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = DefaultFetchTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return Model{ctx: ctx, cfg: cfg, clock: cfg.Now()}
}

// Init fetches the first snapshot at once.
func (m Model) Init() tea.Cmd {
	return m.fetch()
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.settle()
		return m, nil
	case boardReply:
		cmd := m.boardAnswer(msg)
		return m, cmd
	case boardTick:
		if !m.board.Open || msg.gen != m.board.gen || msg.serial != m.board.serial {
			return m, nil
		}
		cmd := m.boardRead()
		return m, cmd
	case snapshotMsg:
		// Each answer schedules exactly one tick, and each tick exactly one
		// fetch, so polls never overlap or multiply.
		m.clock = m.cfg.Now()
		m.sourceNote = msg.note
		if msg.err != nil {
			m.sourceErr = msg.err // keep drawing the last good snapshot
			if errors.Is(msg.err, ErrNoTeamHere) {
				// Not this place's team: draw none, never the last one,
				// and no view of one of its groups.
				m.noTeam = msg.err
				m.group = ""
				m.show(nil)
				m.settle()
			}
			return m, m.tick()
		}
		s := msg.snap
		m.sourceErr, m.noTeam = nil, nil
		m.show(&s)
		m.refollow()
		m.settle()
		return m, m.tick()
	case tickMsg:
		m.clock = m.cfg.Now()
		return m, m.fetch()
	case focusMsg:
		m.focusErr = nil
		if msg.err != nil {
			err := msg.err
			if !errors.Is(err, ErrFocus) {
				err = fmt.Errorf("%w: %w", ErrFocus, err)
			}
			m.focusErr = fmt.Errorf("%s: %w", msg.agent.Name, err)
		}
		return m, nil
	case btopMsg:
		m.btopErr = msg.err
		if msg.err != nil && !errors.Is(msg.err, ErrBtop) {
			m.btopErr = fmt.Errorf("%w: %w", ErrBtop, msg.err)
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.MouseClickMsg:
		return m.click(msg)
	}
	return m, nil
}

func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.groupNote = ""
	if m.search.on {
		return m.searchKey(msg)
	}
	if m.board.Open {
		return m.boardKey(msg)
	}
	switch k := msg.String(); k {
	case "b":
		cmd := m.openBoard()
		return m, cmd
	case "q", keyQuit:
		return m, tea.Quit
	case "/":
		m.closeGroup()
		m.startSearch()
	case "?":
		m.help = !m.help
	case keyEsc:
		// Help closes first; then the view.
		if !m.help {
			m.closeGroup()
		}
		m.help = false
	case keyLeft:
		if m.group != "" {
			m.help = false
			m.closeGroup()
		}
	case keyUp, keyDown:
		m.move(k)
		m.settle()
	case keyEnter:
		return m, m.focus()
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if m.raw != nil && int(k[0]-'1') < len(m.raw.Groups) {
			m.openGroup(m.raw.Groups[k[0]-'1'].Name)
		}
	}
	return m, nil
}

func (m Model) click(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}
	m.groupNote = ""
	if m.help {
		// The overlay hides the groups: a click closes it, and acts on
		// nothing the reader cannot see.
		m.help = false
		return m, nil
	}
	l := plan(m.Frame(), m.Width(), m.Height())
	what, sel := l.hit(msg.X, msg.Y)
	switch what {
	case hitAgent:
		m.choose(sel)
		cmd := m.focus()
		m.endSearch() // a jump ends a search, as Enter does
		m.settle()
		return m, cmd
	case hitGroup:
		// By name: a search narrows the groups, so the index the click
		// found is not the group's index once the search ends.
		m.endSearch()
		m.openGroup(l.groups[sel.Group].Name)
		return m, nil
	case hitBoard:
		m.board.Selected = sel.Agent
		m.board.Expanded = false
		m.board.Scroll = 0
		return m, nil
	case hitBack:
		if m.board.Open {
			m.closeBoard()
			return m, nil
		}
		m.closeGroup()
		return m, nil
	case hitBtop:
		return m, m.openBtop()
	default:
		return m, nil
	}
}

// View draws the frame full-screen, asking for mouse clicks so herdr
// forwards them.
func (m Model) View() tea.View {
	v := tea.NewView(Render(m.Frame(), m.Width(), m.Height(), m.cfg.Options))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// Frame is the model's state as Render draws it.
func (m Model) Frame() Frame {
	f := Frame{
		Snapshot: m.snap, Selection: m.sel, Scroll: m.scroll, Now: m.clock,
		Board: m.board.BoardFrame, Help: m.help, Btop: m.cfg.Btop != nil, Note: m.cfg.Note, NoTeam: m.noTeam,
		Search: m.search.on, Query: m.search.query, Group: m.group,
	}
	for _, note := range []string{m.sourceNote, m.groupNote} {
		if note != "" {
			f.Note = strings.TrimPrefix(f.Note+sep+note, sep)
		}
	}
	for _, err := range []error{m.sourceErr, m.focusErr, m.btopErr, m.board.Err} {
		if err != nil {
			f.Errors = append(f.Errors, err)
		}
	}
	return f
}

// Width is the frame width: the terminal's, or 80 until it reports one.
func (m Model) Width() int {
	if m.width > 0 {
		return m.width
	}
	return fallbackWidth
}

// Height is the frame height: the terminal's, or 24 until it reports one.
func (m Model) Height() int {
	if m.height > 0 {
		return m.height
	}
	return fallbackHeight
}

// Selected is the selected agent, if there is one.
func (m Model) Selected() (snapshot.Agent, bool) {
	if m.snap == nil {
		return snapshot.Agent{}, false
	}
	groups := m.snap.Groups
	if !validSelection(groups, m.sel) {
		return snapshot.Agent{}, false
	}
	return groups[m.sel.Group].Agents[m.sel.Agent], true
}

func (m Model) fetch() tea.Cmd {
	ctx, src, timeout := m.ctx, m.cfg.Source, m.cfg.FetchTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		s, err := src.Snapshot(ctx)
		msg := snapshotMsg{snap: s, err: err}
		if n, ok := src.(noter); ok {
			msg.note = n.Note()
		}
		return msg
	}
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.cfg.Interval, func(time.Time) tea.Msg { return tickMsg{} })
}

// focus returns the command focusing the selected agent, or nil when there
// is none or focus is off. The call runs outside Update, bounded by
// FocusTimeout.
func (m Model) focus() tea.Cmd {
	a, ok := m.Selected()
	if !ok || m.cfg.Focuser == nil {
		return nil
	}
	ctx, f, timeout := m.ctx, m.cfg.Focuser, m.cfg.FocusTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return focusMsg{agent: a, err: f.Focus(ctx, a)}
	}
}

// openBtop returns the command opening btop, or nil when there is no
// opener. Like a focus, it runs outside Update, bounded by FocusTimeout.
func (m Model) openBtop() tea.Cmd {
	if m.cfg.Btop == nil {
		return nil
	}
	ctx, b, timeout := m.ctx, m.cfg.Btop, m.cfg.FocusTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return btopMsg{err: b.OpenBtop(ctx)}
	}
}

// settle keeps the scroll Render settles on for the current frame: the
// previous one, moved only as far as the selection needs.
func (m *Model) settle() {
	if m.board.Open {
		return
	}
	m.scroll = plan(m.Frame(), m.Width(), m.Height()).offset
}

// rowKey is the identity the selection follows across refreshes: a named
// agent by its name, which herdr keeps unique and which outlives a handoff
// to a new pane; an unnamed row by its pane id, since labels repeat (every
// dashboard split carries the manifest's title) and a label can echo an
// agent's name (ADR-001 A18). The zero key anchors nothing.
type rowKey struct {
	unnamed bool
	id      string
}

// keyOf is a's rowKey: zero for a row with no name, or an unnamed row with
// no pane id.
func keyOf(a snapshot.Agent) rowKey {
	k := rowKey{id: a.Name}
	if a.Unnamed {
		k = rowKey{unnamed: true, id: a.PaneID}
	}
	if k.id == "" {
		return rowKey{}
	}
	return k
}

// choose selects sel and anchors the selection to its row.
func (m *Model) choose(sel Selection) {
	m.sel = sel
	if a, ok := m.Selected(); ok {
		m.anchor = keyOf(a)
	}
}

// reanchor puts the selection back on the anchored row after a refresh.
// When that row is gone, the selection keeps its place, clamped to an
// agent that exists.
func (m *Model) reanchor() {
	groups := m.snap.Groups
	for g, grp := range groups {
		for a, ag := range grp.Agents {
			if m.anchor != (rowKey{}) && keyOf(ag) == m.anchor {
				m.sel = Selection{Group: g, Agent: a}
				return
			}
		}
	}
	m.choose(clamp(groups, m.sel))
}

// clamp moves sel onto an agent that exists: the nearest group that has
// agents, then the nearest agent in it.
func clamp(groups []snapshot.Group, sel Selection) Selection {
	if len(groups) == 0 {
		return Selection{}
	}
	start := min(max(sel.Group, 0), len(groups)-1)
	for d := range len(groups) {
		for _, g := range []int{start + d, start - d} {
			if g >= 0 && g < len(groups) && len(groups[g].Agents) > 0 {
				return Selection{Group: g, Agent: min(max(sel.Agent, 0), len(groups[g].Agents)-1)}
			}
		}
	}
	return Selection{}
}

// move applies one arrow key. Up and down walk the agents in the snapshot's
// order, crossing into the previous or next group, and stop at either end.
// In a group's view they stay in the group.
func (m *Model) move(key string) {
	if m.snap == nil {
		return
	}
	groups := m.snap.Groups
	if !validSelection(groups, m.sel) {
		return
	}
	sel := m.sel
	switch key {
	case keyDown:
		if sel.Agent+1 < len(groups[sel.Group].Agents) {
			sel.Agent++
		} else if g := nextGroup(groups, sel.Group, 1); g >= 0 && m.group == "" {
			sel = Selection{Group: g}
		}
	case keyUp:
		if sel.Agent > 0 {
			sel.Agent--
		} else if g := nextGroup(groups, sel.Group, -1); g >= 0 && m.group == "" {
			sel = Selection{Group: g, Agent: len(groups[g].Agents) - 1}
		}
	}
	m.choose(sel)
}

// openGroup opens the view of the group named name, from the overview or
// another group's view, closing the help, which would hide it. The
// selection stays when it is in that group, and moves to the group's first
// agent otherwise.
func (m *Model) openGroup(name string) {
	g := -1
	if m.snap != nil {
		g = groupIndex(m.snap.Groups, name)
	}
	if g < 0 {
		return
	}
	m.help = false
	m.group = name
	if m.sel.Group != g || !validSelection(m.snap.Groups, m.sel) {
		m.choose(Selection{Group: g})
	}
	m.scroll = 0
	m.settle()
}

// closeGroup returns to the overview, keeping the selection: the agent
// selected in the view, or, from a group with none, the one before.
func (m *Model) closeGroup() {
	if m.group == "" {
		return
	}
	m.group = ""
	if m.snap != nil {
		m.reanchor()
	}
	m.scroll = 0
	m.settle()
}

// refollow puts the selection back after a refresh. On the overview it
// follows its row anywhere (reanchor). In a group's view the view follows
// the group by name, and the selection its row within the group, or keeps
// its place there, clamped; a group that is gone returns to the overview,
// with a note.
func (m *Model) refollow() {
	if m.group == "" {
		m.reanchor()
		return
	}
	g := groupIndex(m.snap.Groups, m.group)
	if g < 0 {
		m.groupNote = fmt.Sprintf("group %q is gone", clean(m.group))
		m.group = ""
		m.reanchor()
		return
	}
	agents := m.snap.Groups[g].Agents
	for a, ag := range agents {
		if m.anchor != (rowKey{}) && keyOf(ag) == m.anchor {
			m.sel = Selection{Group: g, Agent: a}
			return
		}
	}
	m.sel = Selection{Group: g, Agent: min(max(m.sel.Agent, 0), max(len(agents)-1, 0))}
	if a, ok := m.Selected(); ok {
		m.anchor = keyOf(a)
	}
}

// nextGroup is the next group from g in direction step that has agents, or
// -1.
func nextGroup(groups []snapshot.Group, g, step int) int {
	for g += step; g >= 0 && g < len(groups); g += step {
		if len(groups[g].Agents) > 0 {
			return g
		}
	}
	return -1
}

// Run runs the dashboard on in and out until q, Ctrl-C or ctx ends it. opts
// come after Run's own options, so a caller can override them.
func Run(ctx context.Context, cfg Config, in io.Reader, out io.Writer, opts ...tea.ProgramOption) error {
	if cfg.Source == nil {
		return ErrNoSource
	}
	base := []tea.ProgramOption{
		tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out),
		// Only the size of output that is not a terminal: bubbletea asks a
		// terminal for its real size and uses that instead. Without it, a
		// pipe gets a 0x0 screen and nothing is drawn.
		tea.WithWindowSize(fallbackWidth, fallbackHeight),
	}
	_, err := tea.NewProgram(New(ctx, cfg), append(base, opts...)...).Run()
	switch {
	case err == nil, errors.Is(err, tea.ErrInterrupted):
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("dashboard stopped: %w", ctx.Err())
	default:
		return fmt.Errorf("%w: %w", ErrTerminal, err)
	}
}

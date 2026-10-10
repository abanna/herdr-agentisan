// Package dashboard is the team dashboard (ADR-001 D5): a bubbletea program
// that draws, from snapshots, a header box (the project's PRs, issues and
// test slots; the boss), then one box per group stacked under it with one
// line per agent. Enter or a click focuses an agent; a click on [ btop ]
// opens btop in a herdr popup.
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
	"time"

	tea "charm.land/bubbletea/v2"

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

// Config is what a dashboard needs.
type Config struct {
	// Source is polled for snapshots.
	Source snapshot.Source
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

// FixtureConfig is HerdrConfig reading snapshots from the JSON file at path,
// again on every poll. The file is read once first: a fixture that cannot be
// read is the caller's mistake, reported before the terminal is taken over,
// not a footer to squint at.
func FixtureConfig(ctx context.Context, path, socketPath string) (Config, error) {
	if path == "" {
		return Config{}, ErrNoSource
	}
	src := snapshot.FileSource{Path: path}
	if _, err := src.Snapshot(ctx); err != nil {
		return Config{}, fmt.Errorf("fixture: %w", err)
	}
	return HerdrConfig(src, socketPath), nil
}

// Model is the dashboard's bubbletea model.
type Model struct {
	// ctx bounds every poll and focus the model starts; it is the
	// program's context, held because tea.Cmd takes none.
	ctx context.Context
	cfg Config

	snap *snapshot.Snapshot
	sel  Selection
	// anchor is the selected agent's name. The selection follows the agent,
	// not its position, across refreshes.
	anchor string
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
}

// The arrow keys, as tea.KeyPressMsg.String names them. Left and right are
// unbound: the groups stack, so there is nothing beside the selection.
const (
	keyUp   = "up"
	keyDown = "down"
)

// Messages the model sends itself.
type (
	snapshotMsg struct {
		snap snapshot.Snapshot
		err  error
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
	case snapshotMsg:
		// Each answer schedules exactly one tick, and each tick exactly one
		// fetch, so polls never overlap or multiply.
		m.clock = m.cfg.Now()
		if msg.err != nil {
			m.sourceErr = msg.err // keep drawing the last good snapshot
			return m, m.tick()
		}
		s := msg.snap
		m.snap, m.sourceErr = &s, nil
		m.reanchor()
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
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "esc":
		m.help = false
	case keyUp, keyDown:
		m.move(msg.String())
		m.settle()
	case "enter":
		return m, m.focus()
	}
	return m, nil
}

func (m Model) click(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}
	if m.help {
		// The overlay hides the groups: a click closes it, and acts on
		// nothing the reader cannot see.
		m.help = false
		return m, nil
	}
	what, sel := plan(m.Frame(), m.Width(), m.Height()).hit(msg.X, msg.Y)
	switch what {
	case hitAgent:
		m.choose(sel)
		m.settle()
		return m, m.focus()
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
		Help: m.help, Btop: m.cfg.Btop != nil, Note: m.cfg.Note,
	}
	for _, err := range []error{m.sourceErr, m.focusErr, m.btopErr} {
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
	groups := Order(m.snap.Groups)
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
		return snapshotMsg{snap: s, err: err}
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
	m.scroll = plan(m.Frame(), m.Width(), m.Height()).offset
}

// choose selects sel and anchors the selection to its agent.
func (m *Model) choose(sel Selection) {
	m.sel = sel
	if a, ok := m.Selected(); ok {
		m.anchor = a.Name
	}
}

// reanchor puts the selection back on the anchored agent after a refresh.
// When that agent is gone, the selection keeps its place, clamped to an
// agent that exists.
func (m *Model) reanchor() {
	groups := Order(m.snap.Groups)
	for g, grp := range groups {
		for a, ag := range grp.Agents {
			if m.anchor != "" && ag.Name == m.anchor {
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

// move applies one arrow key. Up and down walk the agents in display order,
// crossing into the previous or next group, and stop at either end.
func (m *Model) move(key string) {
	if m.snap == nil {
		return
	}
	groups := Order(m.snap.Groups)
	if !validSelection(groups, m.sel) {
		return
	}
	sel := m.sel
	switch key {
	case keyDown:
		if sel.Agent+1 < len(groups[sel.Group].Agents) {
			sel.Agent++
		} else if g := nextGroup(groups, sel.Group, 1); g >= 0 {
			sel = Selection{Group: g}
		}
	case keyUp:
		if sel.Agent > 0 {
			sel.Agent--
		} else if g := nextGroup(groups, sel.Group, -1); g >= 0 {
			sel = Selection{Group: g, Agent: len(groups[g].Agents) - 1}
		}
	}
	m.choose(sel)
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

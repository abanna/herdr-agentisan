// Package dashboard is the team dashboard (ADR-001 D5): a bubbletea program
// that draws one rounded card per group from snapshots, and focuses an agent
// on Enter or a click.
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
	// DefaultFocusTimeout bounds one focus: agent.focus plus pane.zoom.
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
	// Interval between polls. Zero means DefaultInterval.
	Interval time.Duration
	// FocusTimeout bounds one focus. Zero means DefaultFocusTimeout.
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

// HerdrConfig is the dashboard herdr runs: it reads src and focuses through
// the herdr socket at socketPath. Without a socket, focus is off and the
// footer says why.
func HerdrConfig(src snapshot.Source, socketPath string) Config {
	cfg := Config{Source: src}
	if socketPath == "" {
		cfg.Note = "focus off: HERDR_SOCKET_PATH is not set"
		return cfg
	}
	cfg.Focuser = HerdrFocuser{Client: herdr.Client{SocketPath: socketPath}}
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

	clock         time.Time
	width, height int
	help          bool
	sourceErr     error
	focusErr      error
}

// The arrow keys, as tea.KeyPressMsg.String names them.
const (
	keyUp    = "up"
	keyDown  = "down"
	keyLeft  = "left"
	keyRight = "right"
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
	case keyUp, keyDown, keyLeft, keyRight:
		m.move(msg.String())
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
		// The overlay hides the cards: a click closes it, and focuses no
		// one the reader cannot see.
		m.help = false
		return m, nil
	}
	sel, ok := plan(m.Frame(), m.Width(), m.Height()).hit(msg.X, msg.Y)
	if !ok {
		return m, nil
	}
	m.choose(sel)
	return m, m.focus()
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
	f := Frame{Snapshot: m.snap, Selection: m.sel, Now: m.clock, Help: m.help, Note: m.cfg.Note}
	for _, err := range []error{m.sourceErr, m.focusErr} {
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
	return groups[m.sel.Card].Agents[m.sel.Agent], true
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
	for c, g := range groups {
		for a, ag := range g.Agents {
			if m.anchor != "" && ag.Name == m.anchor {
				m.sel = Selection{Card: c, Agent: a}
				return
			}
		}
	}
	m.choose(clamp(groups, m.sel))
}

// clamp moves sel onto an agent that exists: the nearest card that has
// agents, then the nearest agent in it.
func clamp(groups []snapshot.Group, sel Selection) Selection {
	if len(groups) == 0 {
		return Selection{}
	}
	start := min(max(sel.Card, 0), len(groups)-1)
	for d := range len(groups) {
		for _, c := range []int{start + d, start - d} {
			if c >= 0 && c < len(groups) && len(groups[c].Agents) > 0 {
				return Selection{Card: c, Agent: min(max(sel.Agent, 0), len(groups[c].Agents)-1)}
			}
		}
	}
	return Selection{}
}

// move applies one arrow key. Up and down walk the agents in display order,
// crossing into the previous or next card. Left and right step to the card
// beside this one in the grid, keeping the agent index, clamped.
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
		if sel.Agent+1 < len(groups[sel.Card].Agents) {
			sel.Agent++
		} else if c := nextCard(groups, sel.Card, 1); c >= 0 {
			sel = Selection{Card: c}
		}
	case keyUp:
		if sel.Agent > 0 {
			sel.Agent--
		} else if c := nextCard(groups, sel.Card, -1); c >= 0 {
			sel = Selection{Card: c, Agent: len(groups[c].Agents) - 1}
		}
	case keyLeft, keyRight:
		step := 1
		if key == keyLeft {
			step = -1
		}
		cols := columns(groups, m.Width())
		row := sel.Card / cols
		for col := sel.Card%cols + step; col >= 0 && col < cols; col += step {
			c := row*cols + col
			if c >= len(groups) {
				break
			}
			if n := len(groups[c].Agents); n > 0 {
				sel = Selection{Card: c, Agent: min(sel.Agent, n-1)}
				break
			}
		}
	}
	m.choose(sel)
}

// nextCard is the next card from c in direction step that has agents, or -1.
func nextCard(groups []snapshot.Group, c, step int) int {
	for c += step; c >= 0 && c < len(groups); c += step {
		if len(groups[c].Agents) > 0 {
			return c
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

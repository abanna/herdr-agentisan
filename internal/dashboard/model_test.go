package dashboard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// script is a Source that answers from a list, repeating the last answer, and
// counts its calls.
type script struct {
	mu    sync.Mutex
	steps []func() (snapshot.Snapshot, error)
	calls int
}

func (s *script) Snapshot(context.Context) (snapshot.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	step := s.steps[min(s.calls, len(s.steps)-1)]
	s.calls++
	return step()
}

func (s *script) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func serve(snap *snapshot.Snapshot) func() (snapshot.Snapshot, error) {
	return func() (snapshot.Snapshot, error) { return *snap, nil }
}

func fail(err error) func() (snapshot.Snapshot, error) {
	return func() (snapshot.Snapshot, error) { return snapshot.Snapshot{}, err }
}

// recorder is a Focuser that records whom it was asked to focus.
type recorder struct {
	mu  sync.Mutex
	got []snapshot.Agent
	err error
}

func (r *recorder) Focus(_ context.Context, a snapshot.Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, a)
	return r.err
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.got {
		out = append(out, a.Name)
	}
	return out
}

// opener is a BtopOpener that counts its calls and answers err.
type opener struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (o *opener) OpenBtop(context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	return o.err
}

func (o *opener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func config(src snapshot.Source, f dashboard.Focuser) dashboard.Config {
	return dashboard.Config{
		Source:   src,
		Focuser:  f,
		Interval: time.Hour, // no test waits on a real tick unless it asks for one
		Now:      func() time.Time { return at },
		Options:  plain,
	}
}

func update(t *testing.T, m dashboard.Model, msg tea.Msg) (dashboard.Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	got, ok := next.(dashboard.Model)
	require.True(t, ok, "Update returned %T", next)
	return got, cmd
}

// started is a model sized w x h that has received its first snapshot.
func started(t *testing.T, cfg dashboard.Config, w, h int) dashboard.Model {
	t.Helper()
	m := dashboard.New(t.Context(), cfg)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	cmd := m.Init()
	require.NotNil(t, cmd, "Init must fetch the first snapshot")
	m, _ = update(t, m, cmd())
	return m
}

var (
	keyUp    = tea.KeyPressMsg{Code: tea.KeyUp}
	keyDown  = tea.KeyPressMsg{Code: tea.KeyDown}
	keyLeft  = tea.KeyPressMsg{Code: tea.KeyLeft}
	keyRight = tea.KeyPressMsg{Code: tea.KeyRight}
	keyEnter = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyHelp  = tea.KeyPressMsg{Code: '?', Text: "?"}
	keyQ     = tea.KeyPressMsg{Code: 'q', Text: "q"}
	keyCtrlC = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	keyEsc   = tea.KeyPressMsg{Code: tea.KeyEscape}
)

func repeat(k tea.KeyPressMsg, n int) []tea.KeyPressMsg {
	out := make([]tea.KeyPressMsg, n)
	for i := range out {
		out[i] = k
	}
	return out
}

func keys(groups ...[]tea.KeyPressMsg) []tea.KeyPressMsg {
	var out []tea.KeyPressMsg
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func selected(t *testing.T, m dashboard.Model) string {
	t.Helper()
	a, ok := m.Selected()
	require.True(t, ok, "nothing is selected")
	return a.Name
}

// TestArrowKeysMoveTheSelection: up and down walk the agents in display
// order (coders, precheck, qa, codex, research, clerk), across groups,
// stopping at both ends. Left and right do nothing: the groups stack.
func TestArrowKeysMoveTheSelection(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		w, h int
		keys []tea.KeyPressMsg
		want string
	}{
		"starts on the first agent":         {w: 120, h: 40, want: "pee01"},
		"down moves within a group":         {w: 120, h: 40, keys: keys(repeat(keyDown, 1)), want: "pee02"},
		"up at the very first agent stays":  {w: 120, h: 40, keys: keys(repeat(keyUp, 1)), want: "pee01"},
		"down crosses into the next group":  {w: 120, h: 40, keys: keys(repeat(keyDown, 9)), want: "precheck"},
		"up crosses into the previous one":  {w: 120, h: 40, keys: keys(repeat(keyDown, 9), repeat(keyUp, 1)), want: "pee09"},
		"down walks every group in order":   {w: 120, h: 40, keys: keys(repeat(keyDown, 13)), want: "research"},
		"down at the very last agent stays": {w: 120, h: 40, keys: keys(repeat(keyDown, 30)), want: "clerk"},
		"right does nothing":                {w: 120, h: 40, keys: keys(repeat(keyDown, 1), repeat(keyRight, 3)), want: "pee02"},
		"left does nothing":                 {w: 120, h: 40, keys: keys(repeat(keyDown, 10), repeat(keyLeft, 2)), want: "precheck2"},
		// The groups scroll: the selection is drawn wherever it is.
		"down past the screen scrolls": {w: 80, h: 24, keys: keys(repeat(keyDown, 14)), want: "clerk"},
		"and back up":                  {w: 80, h: 24, keys: keys(repeat(keyDown, 14), repeat(keyUp, 14)), want: "pee01"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, &recorder{}), tc.w, tc.h)
			for _, k := range tc.keys {
				m, _ = update(t, m, k)
			}
			assert.Equal(t, tc.want, selected(t, m))
			assert.Contains(t, selectedLine(t, renderAttrs(t, m)), tc.want, "the selection is drawn where it is")
		})
	}
}

// contentTop is the first line of the groups in a frame the model drew.
func contentTop(t *testing.T, m dashboard.Model) string {
	t.Helper()
	return lines(ansi.Strip(m.View().Content))[5]
}

// TestScrollFollowsTheSelection: the groups scroll only as far as the
// selection needs, and stay where they are while it moves within view, so
// stepping back up from the bottom does not jump the view.
func TestScrollFollowsTheSelection(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, &recorder{}), 80, 24)
	assert.True(t, strings.HasPrefix(contentTop(t, m), "╭─ CODERS"))

	for range 14 {
		m, _ = update(t, m, keyDown)
	}
	require.Equal(t, "clerk", selected(t, m))
	bottom := contentTop(t, m)
	assert.True(t, strings.HasPrefix(bottom, "╭─ PRECHECK"), "scrolled to the end: %q", bottom)

	m, _ = update(t, m, keyUp)
	m, _ = update(t, m, keyUp)
	require.Equal(t, "codex", selected(t, m))
	assert.Equal(t, bottom, contentTop(t, m), "moving within view keeps the view")

	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 12})
	assert.Contains(t, selectedLine(t, renderAttrs(t, m)), "codex", "a smaller pane scrolls to keep the selection")

	for range 14 {
		m, _ = update(t, m, keyUp)
	}
	require.Equal(t, "pee01", selected(t, m))
	assert.True(t, strings.HasPrefix(contentTop(t, m), "╭─ CODERS"), "back at the top")
}

// renderAttrs draws the model's current frame keeping attributes, so the
// reverse-video line can be found.
func renderAttrs(t *testing.T, m dashboard.Model) string {
	t.Helper()
	return dashboard.Render(m.Frame(), m.Width(), m.Height(), dashboard.Options{Profile: colorprofile.ASCII})
}

// run executes cmd and feeds its message back, returning the model after.
func run(t *testing.T, m dashboard.Model, cmd tea.Cmd) dashboard.Model {
	t.Helper()
	require.NotNil(t, cmd)
	m, _ = update(t, m, cmd())
	return m
}

func TestEnterFocusesTheSelectedAgent(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, rec), 120, 40)
	m, _ = update(t, m, keyDown)
	m, _ = update(t, m, keyDown)

	m, cmd := update(t, m, keyEnter)
	assert.Empty(t, rec.names(), "focus runs in a command, never inside Update")
	m = run(t, m, cmd)
	assert.Equal(t, []string{"pee03"}, rec.names())
	assert.Equal(t, "w2:p3", rec.got[0].PaneID)
	assert.NotContains(t, m.View().Content, "✖", "a successful focus leaves no error")
}

// locate finds text in a rendered frame and returns its cell coordinates.
func locate(t *testing.T, frame, text string) (int, int) {
	t.Helper()
	for y, line := range strings.Split(ansi.Strip(frame), "\n") {
		if before, _, ok := strings.Cut(line, text); ok {
			return ansi.StringWidth(before), y
		}
	}
	require.Failf(t, "not on screen", "%q", text)
	return 0, 0
}

func TestClickFocusesTheAgentUnderThePointer(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		target string // whose name to click near
		dx, dy int    // offset from the start of the name
		button tea.MouseButton
		want   string // focused and selected; "" for no focus
	}{
		"a row":                             {target: "pee04", button: tea.MouseLeft, want: "pee04"},
		"far right of a row":                {target: "pee05", dx: 100, button: tea.MouseLeft, want: "pee05"},
		"the padding inside the border":     {target: "pee06", dx: -3, button: tea.MouseLeft, want: "pee06"},
		"a group further down":              {target: "● research", button: tea.MouseLeft, want: "research"},
		"the right button does not":         {target: "pee04", button: tea.MouseRight},
		"the header does not":               {target: "agentisan", button: tea.MouseLeft},
		"the boss line does not":            {target: "#867", button: tea.MouseLeft},
		"the gap under the header does not": {target: "agentisan", dy: 3, button: tea.MouseLeft},
		"a group's title does not":          {target: "CODERS", button: tea.MouseLeft},
		"a group's description does not":    {target: "write the code", button: tea.MouseLeft},
		"a box's bottom border does not":    {target: "pee09", dy: 1, button: tea.MouseLeft},
		"a box's left border does not":      {target: "pee01", dx: -100, button: tea.MouseLeft},
		"a box's right border does not":     {target: "pee01", dx: 115, button: tea.MouseLeft},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, rec), 120, 40)
			x, y := locate(t, m.View().Content, tc.target)

			m, cmd := update(t, m, tea.MouseClickMsg{X: max(0, x+tc.dx), Y: y + tc.dy, Button: tc.button})
			if tc.want == "" {
				assert.Nil(t, cmd)
				assert.Equal(t, "pee01", selected(t, m), "a click on nothing keeps the selection")
				return
			}
			assert.Equal(t, tc.want, selected(t, m))
			run(t, m, cmd)
			assert.Equal(t, []string{tc.want}, rec.names())
		})
	}
}

// TestClickAfterScrollHitsWhatIsDrawn: a click maps through the scrolled
// frame, the one on screen, not the unscrolled one.
func TestClickAfterScrollHitsWhatIsDrawn(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, rec), 80, 24)
	for range 14 {
		m, _ = update(t, m, keyDown)
	}
	x, y := locate(t, m.View().Content, "● research")
	m, cmd := update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Equal(t, "research", selected(t, m))
	run(t, m, cmd)
	assert.Equal(t, []string{"research"}, rec.names())
}

// TestClickOnBtopOpensIt: a left click on any cell of [ btop ] asks the
// opener for btop, once, and focuses no one; a click beside it does nothing.
func TestClickOnBtopOpensIt(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dx     int
		button tea.MouseButton
		opens  bool
	}{
		"its first cell":            {dx: 0, button: tea.MouseLeft, opens: true},
		"its last cell":             {dx: 7, button: tea.MouseLeft, opens: true},
		"left of it":                {dx: -1, button: tea.MouseLeft},
		"right of it":               {dx: 8, button: tea.MouseLeft},
		"the right button does not": {dx: 2, button: tea.MouseRight},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec, op := &recorder{}, &opener{}
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, rec)
			cfg.Btop = op
			m := started(t, cfg, 120, 40)
			x, y := locate(t, m.View().Content, "[ btop ]")

			m, cmd := update(t, m, tea.MouseClickMsg{X: x + tc.dx, Y: y, Button: tc.button})
			if !tc.opens {
				assert.Nil(t, cmd)
				assert.Zero(t, op.count())
				return
			}
			assert.Zero(t, op.count(), "btop opens in a command, never inside Update")
			run(t, m, cmd)
			assert.Equal(t, 1, op.count())
			assert.Empty(t, rec.names(), "the button focuses no agent")
			assert.Equal(t, "pee01", selected(t, m))
		})
	}
}

// TestNoBtopWithoutAnOpener: without an opener the button is not drawn, the
// hint is not shown, and a click where the button would be does nothing.
func TestNoBtopWithoutAnOpener(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, &recorder{}), 120, 40)
	assert.NotContains(t, m.View().Content, "btop")
	_, cmd := update(t, m, tea.MouseClickMsg{X: 112, Y: 1, Button: tea.MouseLeft})
	assert.Nil(t, cmd)
}

// TestBtopErrorShowsInTheFooter: a failed open is one line in the footer,
// never a crash, and the next successful open clears it.
func TestBtopErrorShowsInTheFooter(t *testing.T) {
	t.Parallel()

	op := &opener{err: fmt.Errorf("%w: plugin not found", herdr.ErrAPI)}
	cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, &recorder{})
	cfg.Btop = op
	m := started(t, cfg, 120, 40)
	x, y := locate(t, m.View().Content, "[ btop ]")
	click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}

	_, cmd := update(t, m, click)
	m = run(t, m, cmd)
	footer := lines(m.View().Content)[39]
	assert.Contains(t, footer, "✖ btop failed:")
	assert.Contains(t, footer, "plugin not found")

	op.mu.Lock()
	op.err = nil
	op.mu.Unlock()
	_, cmd = update(t, m, click)
	m = run(t, m, cmd)
	assert.NotContains(t, m.View().Content, "btop failed")
}

// TestHerdrBtopOpensThePopup drives the button end to end against the fake
// herdr: one plugin.pane.open for this plugin's btop pane, as a popup at 92%
// of the terminal each way. The plugin itself starts no process.
func TestHerdrBtopOpensThePopup(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		reply herdrtest.Reply
		err   error
	}{
		"herdr opens it": {reply: herdrtest.Reply{Result: map[string]any{"type": "ok"}}},
		"herdr refuses":  {reply: herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "plugin_not_found", Message: "plugin not found"}}, err: herdr.ErrAPI},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, func(herdrtest.Request) herdrtest.Reply { return tc.reply })
			err := dashboard.HerdrBtop{Client: herdr.Client{SocketPath: srv.Path}}.OpenBtop(t.Context())
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				require.ErrorIs(t, err, dashboard.ErrBtop)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, []sent{{Method: "plugin.pane.open", Params: `{"plugin_id":"nerdsrun.agentisan","entrypoint":"btop","width":"92%","height":"92%","placement":"popup"}`}}, requests(srv))
		})
	}
	err := dashboard.HerdrBtop{}.OpenBtop(t.Context())
	require.ErrorIs(t, err, herdr.ErrNoSocket, "no socket fails without dialling")
	require.ErrorIs(t, err, dashboard.ErrBtop)
}

// focusHerdr answers agent.focus and pane.zoom like herdr 0.9.3, or fails
// agent.focus when failFocus is set.
func focusHerdr(failFocus bool) herdrtest.Handler {
	return func(r herdrtest.Request) herdrtest.Reply {
		var p struct {
			Target string `json:"target"`
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(r.Params, &p)
		switch r.Method {
		case "agent.focus":
			if failFocus {
				return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "agent_not_found", Message: "agent target " + p.Target + " not found"}}
			}
			return herdrtest.Reply{Result: map[string]any{"type": "agent_info", "agent": map[string]any{"pane_id": "w9:p9", "name": p.Target}}}
		case "pane.zoom":
			return herdrtest.Reply{Result: map[string]any{"type": "pane_zoom", "zoom": map[string]any{"pane_id": p.PaneID, "zoomed": true, "changed": true}}}
		default:
			return herdrtest.Reply{Error: &herdrtest.ErrorBody{Code: "unknown_method", Message: r.Method}}
		}
	}
}

type sent struct {
	Method string
	Params string
}

func requests(srv *herdrtest.Server) []sent {
	var out []sent
	for _, r := range srv.Requests() {
		out = append(out, sent{Method: r.Method, Params: string(r.Params)})
	}
	return out
}

// TestHerdrFocuserFocusesThenZooms drives Enter end to end against the fake
// herdr: agent.focus by name, then pane.zoom on the pane herdr answered with,
// not the snapshot's (w2:p2 here), which can be stale.
func TestHerdrFocuserFocusesThenZooms(t *testing.T) {
	t.Parallel()

	srv := herdrtest.Start(t, focusHerdr(false))
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, dashboard.HerdrFocuser{Client: herdr.Client{SocketPath: srv.Path}}), 120, 40)
	m, _ = update(t, m, keyDown)

	_, cmd := update(t, m, keyEnter)
	m = run(t, m, cmd)

	assert.Equal(t, []sent{
		{Method: "agent.focus", Params: `{"target":"pee02"}`},
		{Method: "pane.zoom", Params: `{"pane_id":"w9:p9","mode":"on"}`},
	}, requests(srv))
	assert.NotContains(t, m.View().Content, "✖")
}

func TestHerdrFocuser(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		failFocus bool
		noSocket  bool
		agent     snapshot.Agent
		want      []sent
		err       error
	}{
		// pane.zoom also focuses its target, so zooming a stale snapshot id
		// (a moved pane, a reissued id) would move the user off the agent.
		// Only herdr's answer names the pane it just focused.
		"zooms the pane herdr focused, not the snapshot's": {
			agent: snapshot.Agent{Name: "qa", PaneID: "w5:p1"},
			want:  []sent{{"agent.focus", `{"target":"qa"}`}, {"pane.zoom", `{"pane_id":"w9:p9","mode":"on"}`}},
		},
		// A snapshot without a pane id zooms the same pane: never an empty
		// id, which herdr would refuse.
		"a snapshot without a pane id still zooms": {
			agent: snapshot.Agent{Name: "qa"},
			want:  []sent{{"agent.focus", `{"target":"qa"}`}, {"pane.zoom", `{"pane_id":"w9:p9","mode":"on"}`}},
		},
		"a failed focus does not zoom": {
			failFocus: true,
			agent:     snapshot.Agent{Name: "ghost", PaneID: "w5:p1"},
			want:      []sent{{"agent.focus", `{"target":"ghost"}`}},
			err:       herdr.ErrAPI,
		},
		"no socket fails without dialling": {
			noSocket: true,
			agent:    snapshot.Agent{Name: "qa", PaneID: "w5:p1"},
			err:      herdr.ErrNoSocket,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := herdrtest.Start(t, focusHerdr(tc.failFocus))
			client := herdr.Client{SocketPath: srv.Path}
			if tc.noSocket {
				client = herdr.Client{}
			}

			err := dashboard.HerdrFocuser{Client: client}.Focus(t.Context(), tc.agent)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				require.ErrorIs(t, err, dashboard.ErrFocus)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, requests(srv))
		})
	}
}

// TestFocusErrorShowsInTheFooter: a failed focus is one line in the footer,
// never a crash, and the next successful focus clears it.
func TestFocusErrorShowsInTheFooter(t *testing.T) {
	t.Parallel()

	rec := &recorder{err: fmt.Errorf("%w: agent target pee01 not found", herdr.ErrAPI)}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, rec), 120, 40)

	_, cmd := update(t, m, keyEnter)
	m = run(t, m, cmd)
	lines := strings.Split(m.View().Content, "\n")
	footer := lines[len(lines)-1]
	assert.Contains(t, footer, "✖ pee01: focus failed:")
	assert.Contains(t, footer, "agent target pee01 not found")
	assert.Contains(t, m.View().Content, "pee09", "the groups stay drawn")

	rec.mu.Lock()
	rec.err = nil
	rec.mu.Unlock()
	_, cmd = update(t, m, keyEnter)
	m = run(t, m, cmd)
	assert.NotContains(t, m.View().Content, "not found")
}

func TestFocusDisabledWithoutAFocuser(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	cfg := config(src, nil)
	cfg.Note = "focus off: HERDR_SOCKET_PATH is not set"
	m := started(t, cfg, 120, 40)

	_, cmd := update(t, m, keyEnter)
	assert.Nil(t, cmd)
	x, y := locate(t, m.View().Content, "pee03")
	m, cmd = update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, cmd)
	assert.Equal(t, "pee03", selected(t, m), "a click still selects")
	assert.Contains(t, m.View().Content, "focus off: HERDR_SOCKET_PATH is not set")
}

func TestQuitKeys(t *testing.T) {
	t.Parallel()

	for name, k := range map[string]tea.KeyPressMsg{"q": keyQ, "ctrl+c": keyCtrlC} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, &recorder{}), 80, 24)

			_, cmd := update(t, m, k)
			require.NotNil(t, cmd)
			assert.IsType(t, tea.QuitMsg{}, cmd())
		})
	}
}

func TestHelpToggles(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		keys []tea.KeyPressMsg
		want bool
	}{
		"? opens":                  {keys: []tea.KeyPressMsg{keyHelp}, want: true},
		"? again closes":           {keys: []tea.KeyPressMsg{keyHelp, keyHelp}},
		"esc closes":               {keys: []tea.KeyPressMsg{keyHelp, keyEsc}},
		"moving keeps it open":     {keys: []tea.KeyPressMsg{keyHelp, keyDown}, want: true},
		"esc without help is fine": {keys: []tea.KeyPressMsg{keyEsc}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, &recorder{}), 80, 24)
			for _, k := range tc.keys {
				m, _ = update(t, m, k)
			}
			assert.Equal(t, tc.want, strings.Contains(m.View().Content, "╭ help"))
		})
	}
}

// TestClickWhileHelpIsOpenClosesIt: the overlay covers the groups, so a
// click must neither focus an agent nor open btop the reader cannot see.
func TestClickWhileHelpIsOpenClosesIt(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"pee04", "[ btop ]"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			rec, op := &recorder{}, &opener{}
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, rec)
			cfg.Btop = op
			m := started(t, cfg, 120, 40)
			x, y := locate(t, m.View().Content, target)
			m, _ = update(t, m, keyHelp)

			m, cmd := update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			assert.Nil(t, cmd)
			assert.NotContains(t, m.View().Content, "╭ help")
			assert.Empty(t, rec.names())
			assert.Zero(t, op.count())
		})
	}
}

// TestPolling: Init fetches at once; each answer schedules exactly one tick;
// each tick fetches exactly once. So the ticks never multiply.
func TestPolling(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	cfg := config(src, &recorder{})
	cfg.Interval = time.Millisecond
	m := dashboard.New(t.Context(), cfg)

	cmd := m.Init()
	require.NotNil(t, cmd)
	msg := cmd()
	assert.Equal(t, 1, src.count(), "Init fetches immediately")

	for i := 2; i <= 4; i++ {
		var tick tea.Cmd
		m, tick = update(t, m, msg)
		require.NotNil(t, tick, "an answer schedules the next tick")
		assert.Equal(t, i-1, src.count(), "scheduling a tick fetches nothing")

		var fetch tea.Cmd
		m, fetch = update(t, m, tick())
		require.NotNil(t, fetch, "a tick fetches")
		msg = fetch()
		assert.Equal(t, i, src.count())
	}
}

// TestSourceErrorKeepsTheLastGoodFrame: a failed poll shows its error in the
// footer and keeps drawing the last snapshot; the next good poll clears it.
func TestSourceErrorKeepsTheLastGoodFrame(t *testing.T) {
	t.Parallel()

	full := fixture(t, "full")
	src := &script{steps: []func() (snapshot.Snapshot, error){
		serve(full),
		fail(fmt.Errorf("%w: connection refused", snapshot.ErrUnavailable)),
		serve(full),
	}}
	cfg := config(src, &recorder{})
	cfg.Interval = time.Millisecond
	m, tick := startedPolling(t, cfg, 120, 40)
	before := m.View().Content

	m, tick = pollOnce(t, m, tick)
	after := m.View().Content
	assert.Contains(t, after, "connection refused")
	assert.Equal(t, groupsOnly(before), groupsOnly(after), "the groups are the last good frame's")

	m, _ = pollOnce(t, m, tick)
	assert.NotContains(t, m.View().Content, "connection refused")
	assert.Equal(t, 3, src.count())
}

// startedPolling is started, also returning the tick the first answer
// scheduled.
func startedPolling(t *testing.T, cfg dashboard.Config, w, h int) (dashboard.Model, tea.Cmd) {
	t.Helper()
	m := dashboard.New(t.Context(), cfg)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	m, tick := update(t, m, m.Init()())
	require.NotNil(t, tick)
	return m, tick
}

// pollOnce waits out tick, runs the fetch it triggers and feeds the answer
// back, returning the next tick.
func pollOnce(t *testing.T, m dashboard.Model, tick tea.Cmd) (dashboard.Model, tea.Cmd) {
	t.Helper()
	m, fetch := update(t, m, tick())
	require.NotNil(t, fetch)
	m, tick = update(t, m, fetch())
	require.NotNil(t, tick)
	return m, tick
}

func groupsOnly(frame string) string {
	lines := strings.Split(frame, "\n")
	return strings.Join(lines[:len(lines)-1], "\n")
}

func TestWaitingForTheFirstSnapshot(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){fail(fmt.Errorf("%w: refused", snapshot.ErrUnavailable))}}
	m := dashboard.New(t.Context(), config(src, &recorder{}))
	assert.Contains(t, m.View().Content, "waiting for the daemon…", "an unsized model draws at 80x24")
	assert.Len(t, lines(m.View().Content), 24)

	m, _ = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = run(t, m, m.Init())
	out := m.View().Content
	assert.Contains(t, out, "waiting for the daemon…")
	assert.Contains(t, out, "refused")
	_, ok := m.Selected()
	assert.False(t, ok)

	_, cmd := update(t, m, keyEnter)
	assert.Nil(t, cmd, "nothing to focus yet")
}

// TestUnusableWindowSizesDrawAtTheFallback: a reported size of zero or less
// is no size; the model draws at 80x24 until a usable one arrives.
func TestUnusableWindowSizesDrawAtTheFallback(t *testing.T) {
	t.Parallel()

	for name, size := range map[string]tea.WindowSizeMsg{
		"zero":     {Width: 0, Height: 0},
		"negative": {Width: -3, Height: -1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, &recorder{}), 120, 40)
			m, _ = update(t, m, size)
			assert.Equal(t, 80, m.Width())
			assert.Equal(t, 24, m.Height())
			require.Len(t, lines(m.View().Content), 24)
			assert.Contains(t, selectedLine(t, renderAttrs(t, m)), "pee01")
		})
	}
}

// TestSelectionFollowsTheAgent: the selection is anchored to the agent, not
// its position, so it survives agents coming and going around it.
func TestSelectionFollowsTheAgent(t *testing.T) {
	t.Parallel()

	full := fixture(t, "full")
	shifted := *full
	shifted.Groups = make([]snapshot.Group, len(full.Groups))
	copy(shifted.Groups, full.Groups)
	for i, g := range shifted.Groups {
		if g.Name == "coders" {
			g.Agents = g.Agents[1:] // pee01 leaves
			shifted.Groups[i] = g
		}
	}
	gone := shifted
	gone.Groups = []snapshot.Group{{Name: "qa", Agents: []snapshot.Agent{{Name: "qa", Status: snapshot.StatusIdle}}}}

	tests := map[string]struct {
		next *snapshot.Snapshot
		want string
	}{
		"an agent before it leaves": {next: &shifted, want: "pee03"},
		"the selected agent leaves": {next: &gone, want: "qa"},
		"the same snapshot again":   {next: full, want: "pee03"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(full), serve(tc.next)}}
			cfg := config(src, &recorder{})
			m := started(t, cfg, 120, 40)
			m, _ = update(t, m, keyDown)
			m, _ = update(t, m, keyDown)
			require.Equal(t, "pee03", selected(t, m))

			m = run(t, m, m.Init())
			assert.Equal(t, tc.want, selected(t, m))
		})
	}
}

func TestViewAsksForTheWholeScreenAndTheMouse(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	v := started(t, config(src, &recorder{}), 80, 24).View()
	assert.True(t, v.AltScreen)
	assert.Equal(t, tea.MouseModeCellMotion, v.MouseMode, "herdr forwards clicks only to a pane that asks")
	assert.Contains(t, v.Content, "17:19", "the header shows the injected clock")
}

func TestHerdrConfig(t *testing.T) {
	t.Parallel()

	src := snapshot.FileSource{Path: "x.json"}

	off := dashboard.HerdrConfig(src, "")
	assert.Nil(t, off.Focuser)
	assert.Nil(t, off.Btop, "no socket, no btop")
	assert.Contains(t, off.Note, "HERDR_SOCKET_PATH")
	assert.Equal(t, src, off.Source)

	on := dashboard.HerdrConfig(src, "/run/herdr.sock")
	assert.Equal(t, dashboard.HerdrFocuser{Client: herdr.Client{SocketPath: "/run/herdr.sock"}}, on.Focuser)
	assert.Equal(t, dashboard.HerdrBtop{Client: herdr.Client{SocketPath: "/run/herdr.sock"}}, on.Btop)
	assert.Empty(t, on.Note)
}

// TestFixtureConfig: a fixture is read once before the terminal is taken
// over, so a bad path fails the command instead of filling the footer.
func TestFixtureConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"v":1,"team":"t","groups":[],"x":1}`), 0o600))
	good := filepath.Join("testdata", "full.json")

	tests := map[string]struct {
		path   string
		socket string
		want   error
		check  func(t *testing.T, cfg dashboard.Config)
	}{
		"no fixture":      {path: "", want: dashboard.ErrNoSource},
		"missing fixture": {path: filepath.Join(dir, "nope.json"), want: snapshot.ErrUnavailable},
		"invalid fixture": {path: bad, want: snapshot.ErrInvalid},
		"focus through herdr": {
			path: good, socket: "/run/herdr.sock",
			check: func(t *testing.T, cfg dashboard.Config) {
				t.Helper()
				assert.Equal(t, snapshot.FileSource{Path: good}, cfg.Source)
				assert.Equal(t, dashboard.HerdrFocuser{Client: herdr.Client{SocketPath: "/run/herdr.sock"}}, cfg.Focuser)
				assert.Equal(t, dashboard.HerdrBtop{Client: herdr.Client{SocketPath: "/run/herdr.sock"}}, cfg.Btop)
			},
		},
		"focus off without a socket": {
			path: good,
			check: func(t *testing.T, cfg dashboard.Config) {
				t.Helper()
				assert.Nil(t, cfg.Focuser)
				assert.Nil(t, cfg.Btop)
				assert.Contains(t, cfg.Note, "HERDR_SOCKET_PATH")
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := dashboard.FixtureConfig(t.Context(), tc.path, tc.socket)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

// syncBuffer is a bytes.Buffer safe to read while the program writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRunDrawsAndQuits runs the real program on pipes: it draws the fixture,
// then quits on q. Stdin stays open until Run returns, as a terminal's would.
func TestRunDrawsAndQuits(t *testing.T) {
	t.Parallel()

	in, keys := io.Pipe()
	defer in.Close()
	var out syncBuffer
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}

	done := make(chan error, 1)
	go func() {
		done <- dashboard.Run(t.Context(), config(src, &recorder{}), in, &out, tea.WithWindowSize(120, 40))
	}()
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "pee09") }, 5*time.Second, 10*time.Millisecond)
	_, err := keys.Write([]byte("q"))
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("q did not quit")
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	t.Parallel()

	in, keys := io.Pipe()
	defer in.Close()
	defer keys.Close()
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- dashboard.Run(ctx, config(src, &recorder{}), in, io.Discard, tea.WithWindowSize(80, 24))
	}()
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not stop the program")
	}
}

// blockingSource answers only when its context ends.
type blockingSource struct{}

func (blockingSource) Snapshot(ctx context.Context) (snapshot.Snapshot, error) {
	<-ctx.Done()
	return snapshot.Snapshot{}, ctx.Err()
}

// blockingBtop returns only when its context ends.
type blockingBtop struct{}

func (blockingBtop) OpenBtop(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// blockingFocuser returns only when its context ends.
type blockingFocuser struct{}

func (blockingFocuser) Focus(ctx context.Context, _ snapshot.Agent) error {
	<-ctx.Done()
	return ctx.Err()
}

// within runs cmd and returns its message, or fails if it takes longer than d.
func within(t *testing.T, d time.Duration, cmd tea.Cmd) tea.Msg {
	t.Helper()
	require.NotNil(t, cmd)
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	select {
	case msg := <-got:
		return msg
	case <-time.After(d):
		require.FailNow(t, "the command did not return in time", "%s", d)
		return nil
	}
}

// TestCallsAreBounded: a source or a herdr that never answers costs one
// footer error after its timeout, never a dashboard that stops refreshing
// or a focus that never comes back.
func TestCallsAreBounded(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cfg  func(t *testing.T) dashboard.Config
		cmd  func(t *testing.T, m dashboard.Model) (dashboard.Model, tea.Cmd)
		want []string
	}{
		"a source that never answers": {
			cfg: func(*testing.T) dashboard.Config {
				cfg := config(blockingSource{}, &recorder{})
				cfg.FetchTimeout = 20 * time.Millisecond
				return cfg
			},
			cmd:  func(_ *testing.T, m dashboard.Model) (dashboard.Model, tea.Cmd) { return m, m.Init() },
			want: []string{"waiting for the daemon…", "deadline exceeded"},
		},
		"a focus that never returns": {
			cfg: func(t *testing.T) dashboard.Config {
				t.Helper()
				cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, blockingFocuser{})
				cfg.FocusTimeout = 20 * time.Millisecond
				return cfg
			},
			cmd: func(t *testing.T, m dashboard.Model) (dashboard.Model, tea.Cmd) {
				t.Helper()
				m = run(t, m, m.Init())
				return update(t, m, keyEnter)
			},
			want: []string{"pee01: focus failed", "deadline exceeded"},
		},
		"a btop that never opens": {
			cfg: func(t *testing.T) dashboard.Config {
				t.Helper()
				cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, &recorder{})
				cfg.Btop = blockingBtop{}
				cfg.FocusTimeout = 20 * time.Millisecond
				return cfg
			},
			cmd: func(t *testing.T, m dashboard.Model) (dashboard.Model, tea.Cmd) {
				t.Helper()
				m = run(t, m, m.Init())
				x, y := locate(t, m.View().Content, "[ btop ]")
				return update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			},
			want: []string{"btop failed", "deadline exceeded"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := dashboard.New(t.Context(), tc.cfg(t))
			m, _ = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
			m, cmd := tc.cmd(t, m)

			m, _ = update(t, m, within(t, 5*time.Second, cmd))
			for _, s := range tc.want {
				assert.Contains(t, m.View().Content, s)
			}
		})
	}
}

// TestZeroConfigMeansDefaults: a zero or negative interval or timeout is the
// default, never "no wait at all", which would spin the poll or cut off
// every focus at once.
func TestZeroConfigMeansDefaults(t *testing.T) {
	t.Parallel()

	const quiet = 200 * time.Millisecond // far below every default
	tests := map[string]func(t *testing.T) tea.Cmd{
		"a zero interval": func(t *testing.T) tea.Cmd {
			t.Helper()
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
			cfg.Interval = 0
			_, tick := startedPolling(t, cfg, 80, 24)
			return tick
		},
		"a negative interval": func(t *testing.T) tea.Cmd {
			t.Helper()
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
			cfg.Interval = -time.Second
			_, tick := startedPolling(t, cfg, 80, 24)
			return tick
		},
		"a zero fetch timeout": func(t *testing.T) tea.Cmd {
			t.Helper()
			return dashboard.New(t.Context(), config(blockingSource{}, nil)).Init()
		},
		"a zero focus timeout": func(t *testing.T) tea.Cmd {
			t.Helper()
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, blockingFocuser{})
			_, cmd := update(t, started(t, cfg, 80, 24), keyEnter)
			return cmd
		},
		"a negative focus timeout": func(t *testing.T) tea.Cmd {
			t.Helper()
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, blockingFocuser{})
			cfg.FocusTimeout = -time.Second
			_, cmd := update(t, started(t, cfg, 80, 24), keyEnter)
			return cmd
		},
		"a zero timeout bounds btop by the default": func(t *testing.T) tea.Cmd {
			t.Helper()
			cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
			cfg.Btop = blockingBtop{}
			m := started(t, cfg, 120, 40)
			x, y := locate(t, m.View().Content, "[ btop ]")
			_, cmd := update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			return cmd
		},
		"a negative fetch timeout": func(t *testing.T) tea.Cmd {
			t.Helper()
			cfg := config(blockingSource{}, nil)
			cfg.FetchTimeout = -time.Second
			return dashboard.New(t.Context(), cfg).Init()
		},
	}
	for name, cmdOf := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := cmdOf(t)
			require.NotNil(t, cmd)
			got := make(chan tea.Msg, 1)
			go func() { got <- cmd() }()
			select {
			case msg := <-got:
				t.Fatalf("returned %T at once: the zero value was not replaced by the default", msg)
			case <-time.After(quiet):
			}
		})
	}
}

// TestTheDefaultClockIsTheLocalTime: without Config.Now the header shows the
// wall clock, in the process's local time zone.
func TestTheDefaultClockIsTheLocalTime(t *testing.T) {
	t.Parallel()

	cfg := config(&script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}, nil)
	cfg.Now = nil
	before := time.Now()
	m := started(t, cfg, 120, 40)
	after := time.Now()

	boss := lineWith(t, m.View().Content, "boss ▸")
	assert.True(t, strings.HasSuffix(boss, before.Format("15:04")+" │") || strings.HasSuffix(boss, after.Format("15:04")+" │"), "%q", boss)
}

// TestUnboundKeysDoNothing: only the documented keys act. A capital Q, q
// with a modifier, a burst of text and every other key change nothing.
func TestUnboundKeysDoNothing(t *testing.T) {
	t.Parallel()

	tests := map[string]tea.KeyPressMsg{
		"a letter":              {Code: 'x', Text: "x"},
		"a non-ASCII letter":    {Code: 'é', Text: "é"},
		"a capital Q":           {Code: 'q', ShiftedCode: 'Q', Mod: tea.ModShift, Text: "Q"},
		"alt+q":                 {Code: 'q', Mod: tea.ModAlt},
		"ctrl+q":                {Code: 'q', Mod: tea.ModCtrl},
		"a burst of q":          {Code: 'q', Text: "qqqq"},
		"tab":                   {Code: tea.KeyTab},
		"space":                 {Code: tea.KeySpace, Text: " "},
		"a function key":        {Code: tea.KeyF1},
		"enter with a modifier": {Code: tea.KeyEnter, Mod: tea.ModAlt},
		"an empty key event":    {},
		"left":                  keyLeft,
		"right":                 keyRight,
		"m":                     {Code: 'm', Text: "m"},
		"b":                     {Code: 'b', Text: "b"},
	}
	for name, k := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, rec), 120, 40)
			before := m.View().Content

			m, cmd := update(t, m, k)
			assert.Nil(t, cmd)
			assert.Equal(t, before, m.View().Content)
			assert.Empty(t, rec.names())
		})
	}
}

// TestClicksOffTheAgentsDoNothing: positions outside the frame, negative or
// huge, select and focus no one.
func TestClicksOffTheAgentsDoNothing(t *testing.T) {
	t.Parallel()

	tests := map[string]tea.MouseClickMsg{
		"a negative position":       {X: -1, Y: -1, Button: tea.MouseLeft},
		"a negative x on a row":     {X: -5, Y: 3, Button: tea.MouseLeft},
		"a position past the frame": {X: 1 << 30, Y: 1 << 30, Button: tea.MouseLeft},
		"the footer":                {X: 3, Y: 39, Button: tea.MouseLeft},
		"the wheel":                 {X: 3, Y: 3, Button: tea.MouseWheelDown},
		"an empty click event":      {},
	}
	for name, click := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, rec), 120, 40)

			m, cmd := update(t, m, click)
			assert.Nil(t, cmd)
			assert.Equal(t, "pee01", selected(t, m))
			assert.Empty(t, rec.names())
		})
	}
}

// TestRunWithClosedStdin: with no input the dashboard still draws and
// refreshes — it is a view — and stops when its context ends (main cancels
// it on SIGINT or SIGTERM).
func TestRunWithClosedStdin(t *testing.T) {
	t.Parallel()

	var out syncBuffer
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- dashboard.Run(ctx, config(src, &recorder{}), strings.NewReader(""), &out, tea.WithWindowSize(120, 40))
	}()
	require.Eventually(t, func() bool { return strings.Contains(out.String(), "pee09") }, 5*time.Second, 10*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("stopped on its own: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not stop the program")
	}
}

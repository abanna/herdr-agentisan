package dashboard_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/dashboard"
	"github.com/abanna/herdr-agentisan/internal/herdr"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

var (
	keySlash     = tea.KeyPressMsg{Code: '/', Text: "/"}
	keyBackspace = tea.KeyPressMsg{Code: tea.KeyBackspace}
)

// typing is text as its key presses, one per rune.
func typing(text string) []tea.KeyPressMsg {
	var out []tea.KeyPressMsg
	for _, r := range text {
		if r == ' ' {
			out = append(out, tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
			continue
		}
		out = append(out, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return out
}

// press sends keys in order and returns the model and the last command.
func press(t *testing.T, m dashboard.Model, ks ...tea.KeyPressMsg) (dashboard.Model, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, k := range ks {
		m, cmd = update(t, m, k)
	}
	return m, cmd
}

// TestMatch: every word of the query, case folded, appears in order with
// gaps allowed somewhere in the agent's name, group, item and stage. Words
// match independently, so they may come from different fields and in any
// order. Go To's substring match misses "pe4" and "cod 5255"; this does not.
func TestMatch(t *testing.T) {
	t.Parallel()

	pee04 := snapshot.Agent{Name: "pee04", Item: "NERD-5255", Stage: "go-review"}
	tests := map[string]struct {
		query, group string
		agent        snapshot.Agent
		want         bool
	}{
		"an empty query":                       {query: "", group: "coders", agent: pee04, want: true},
		"white space only":                     {query: " \t ", group: "coders", agent: pee04, want: true},
		"the whole name":                       {query: "pee04", group: "coders", agent: pee04, want: true},
		"any case":                             {query: "PEE04", group: "coders", agent: pee04, want: true},
		"gaps allowed":                         {query: "pe4", group: "coders", agent: pee04, want: true},
		"the group":                            {query: "cod", group: "coders", agent: pee04, want: true},
		"the item":                             {query: "5255", group: "coders", agent: pee04, want: true},
		"the stage":                            {query: "review", group: "coders", agent: pee04, want: true},
		"words from different fields":          {query: "cod 5255", group: "coders", agent: pee04, want: true},
		"words in any order":                   {query: "5255 cod", group: "coders", agent: pee04, want: true},
		"letters out of order":                 {query: "40eep", group: "coders", agent: pee04},
		"one word missing":                     {query: "cod zzz", group: "coders", agent: pee04},
		"nothing like it":                      {query: "qa", group: "coders", agent: pee04},
		"only the name set":                    {query: "clerk", agent: snapshot.Agent{Name: "clerk"}, want: true},
		"a field that is not searched":         {query: "w2p4", group: "coders", agent: snapshot.Agent{Name: "pee04", PaneID: "w2:p4", Model: "opus"}},
		"non-ASCII, case folded":               {query: "ÜNÏ", group: "qa", agent: snapshot.Agent{Name: "ünïcode"}, want: true},
		"a rune a word needs twice":            {query: "ee", group: "", agent: snapshot.Agent{Name: "pe"}},
		"a query longer than the agent's text": {query: strings.Repeat("a", 500), agent: snapshot.Agent{Name: "aaa"}},
		"invalid UTF-8 in the query":           {query: "\xff", group: "coders", agent: pee04},
		"invalid UTF-8 on both sides":          {query: "\xff", agent: snapshot.Agent{Name: "x\xfey"}, want: true},
		"invalid UTF-8 in the group":           {query: "\xff", group: "\xfe", agent: snapshot.Agent{Name: "a"}, want: true},
		"CR and LF part words like spaces":     {query: "cod\r\n5255", group: "coders", agent: pee04, want: true},
		"a non-ASCII group":                    {query: "çod", group: "ÇODERS", agent: pee04, want: true},
		"an agent with no text":                {query: "a", agent: snapshot.Agent{}},
		"every letter of the text, in order":   {query: "abc", agent: snapshot.Agent{Name: "abc"}, want: true},
		"one letter past the text":             {query: "abcd", agent: snapshot.Agent{Name: "abc"}},
		"NFC does not find NFD":                {query: "\u00e9", agent: snapshot.Agent{Name: "e\u0301"}},
		"NFD in the group":                     {query: "\u00e9", group: "e\u0301", agent: snapshot.Agent{Name: "a"}},
		"a group of two words":                 {query: "two words", group: "two words", agent: snapshot.Agent{Name: "a"}, want: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, dashboard.Match(tc.query, tc.group, tc.agent))
		})
	}
}

// TestFilter: groups keep their order, a group with no match drops out, and
// what is not an agent (the boss, the project) is untouched. The snapshot
// given is never modified, and an empty query hands it back as is.
func TestFilter(t *testing.T) {
	t.Parallel()

	full := fixture(t, "full")

	assert.Nil(t, dashboard.Filter(nil, "pee"), "nothing to filter")
	assert.Same(t, full, dashboard.Filter(full, ""), "an empty query filters nothing")
	assert.Same(t, full, dashboard.Filter(full, "  "), "white space filters nothing")

	got := dashboard.Filter(full, "q")
	require.NotNil(t, got)
	var names []string
	for _, g := range got.Groups {
		for _, a := range g.Agents {
			names = append(names, g.Name+"/"+a.Name)
		}
	}
	assert.Equal(t, []string{"qa/qa", "coders/pee02"}, names, "qa's queue is empty, pee02 is re-queued; the snapshot's order")
	assert.Same(t, full.Boss, got.Boss)
	assert.Same(t, full.Project, got.Project)
	assert.Equal(t, full.Team, got.Team)
	assert.Equal(t, fixture(t, "full"), full, "the snapshot given is never modified")

	none := dashboard.Filter(full, "zzz")
	require.NotNil(t, none)
	assert.Empty(t, none.Groups)
	assert.Same(t, full.Boss, none.Boss)

	empty := dashboard.Filter(fixture(t, "empty"), "a")
	require.NotNil(t, empty)
	assert.Empty(t, empty.Groups, "a group with no agents has nothing to match")
	assert.Empty(t, dashboard.Filter(&snapshot.Snapshot{}, "a").Groups)
}

// TestFilterSizes: a filter is linear in the agents and their text, and
// keeps exactly the matches however many agents or how much text there is.
func TestFilterSizes(t *testing.T) {
	t.Parallel()

	many := snapshot.Group{Name: "coders"}
	for i := range 10000 {
		many.Agents = append(many.Agents, snapshot.Agent{Name: fmt.Sprintf("pee%05d", i)})
	}
	hugeGroup := snapshot.Group{Name: strings.Repeat("x", 1<<20) + "needle", Agents: []snapshot.Agent{{Name: "qa"}}}
	huge := snapshot.Group{Name: "qa", Agents: []snapshot.Agent{
		{Name: "qa", Stage: strings.Repeat("x", 1<<20) + "needle"},
		{Name: "qa2", Stage: strings.Repeat("x", 1<<20)},
	}}
	tests := map[string]struct {
		group snapshot.Group
		query string
		want  []string
	}{
		"many agents":          {group: many, query: "pee09999", want: []string{"pee09999"}},
		"one huge field":       {group: huge, query: "needle", want: []string{"qa"}},
		"a huge field, no hit": {group: huge, query: "needles"},
		"a huge group name":    {group: hugeGroup, query: "needle", want: []string{"qa"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := dashboard.Filter(&snapshot.Snapshot{Groups: []snapshot.Group{tc.group}}, tc.query)
			var names []string
			for _, g := range got.Groups {
				for _, a := range g.Agents {
					names = append(names, a.Name)
				}
			}
			assert.Equal(t, tc.want, names)
		})
	}
}

// TestSearchFollowsAnyName: names are compared exactly, so the selection
// follows its agent across a poll whatever the name holds; case is folded
// only for matching. A query in NFC does not find a name in NFD: matching
// does not normalize.
func TestSearchFollowsAnyName(t *testing.T) {
	t.Parallel()

	team := func(names ...string) *snapshot.Snapshot {
		g := snapshot.Group{Name: "qa"} // no e: the NFC case must not match on the group
		for _, n := range names {
			g.Agents = append(g.Agents, snapshot.Agent{Name: n, Status: snapshot.StatusIdle})
		}
		return &snapshot.Snapshot{V: snapshot.Version, Team: "t", Groups: []snapshot.Group{g}}
	}
	tests := map[string]struct {
		before, after *snapshot.Snapshot
		query         string
		down          int
		want          string
	}{
		"non-ASCII":             {before: team("ünï", "ünï2"), query: "ÜNÏ", down: 1, want: "ünï2"},
		"case variants":         {before: team("Pee", "pee"), query: "pee", down: 1, want: "pee"},
		"NFC and NFD":           {before: team("\u00e9", "e\u0301"), query: "e", want: "e\u0301"},
		"white space":           {before: team("a b", "a  b"), query: "a b", down: 1, want: "a  b"},
		"a leading dash":        {before: team("-x", "--help"), query: "-", down: 1, want: "--help"},
		"renamed between polls": {before: team("pee1", "pee2"), after: team("pee1", "pee3"), query: "pee", down: 1, want: "pee1"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			after := tc.after
			if after == nil {
				after = tc.before
			}
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(tc.before), serve(after)}}
			m := started(t, config(src, &recorder{}), 80, 24)
			m, _ = press(t, m, keys([]tea.KeyPressMsg{keySlash}, typing(tc.query), repeat(keyDown, tc.down))...)

			m = run(t, m, m.Init())
			assert.Equal(t, tc.want, selected(t, m))
		})
	}
}

// TestSearchJumpsToTheMatch: / starts a query, the groups narrow to what it
// matches as it is typed, and the selection goes to the first match. Enter
// jumps there and ends the search: every agent is back, the jumped-to one
// still selected.
func TestSearchJumpsToTheMatch(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, rec), 120, 40)

	m, cmd := press(t, m, keySlash)
	assert.Nil(t, cmd)
	assert.True(t, m.Frame().Search)
	assert.Equal(t, "pee01", selected(t, m), "/ alone moves nothing")

	m, cmd = press(t, m, typing("5255")...)
	assert.Nil(t, cmd)
	assert.Equal(t, "5255", m.Frame().Query)
	assert.Equal(t, "pee04", selected(t, m))
	view := m.View().Content
	assert.Contains(t, view, "pee04")
	for _, gone := range []string{"pee01", "pee05", "precheck2", "research", "clerk"} {
		assert.NotContains(t, view, gone)
	}
	assert.Contains(t, view, "/5255", "the footer shows the query")

	m, cmd = press(t, m, keyEnter)
	assert.False(t, m.Frame().Search, "a jump ends the search")
	assert.Empty(t, m.Frame().Query)
	assert.Contains(t, m.View().Content, "pee01", "every agent is back")
	assert.Equal(t, "pee04", selected(t, m))
	m = run(t, m, cmd)
	assert.Equal(t, []string{"pee04"}, rec.names())
	assert.NotContains(t, m.View().Content, "✖")
}

// TestSearchJumpsThroughHerdr drives a search end to end against the fake
// herdr: the match is focused by name, then its pane zoomed.
func TestSearchJumpsThroughHerdr(t *testing.T) {
	t.Parallel()

	srv := herdrtest.Start(t, focusHerdr(false))
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, dashboard.HerdrFocuser{Client: herdr.Client{SocketPath: srv.Path}}), 120, 40)

	m, _ = press(t, m, keys([]tea.KeyPressMsg{keySlash}, typing("cod 5255"))...)
	_, cmd := press(t, m, keyEnter)
	m = run(t, m, cmd)

	assert.Equal(t, []sent{
		{Method: "agent.focus", Params: `{"target":"pee04"}`},
		{Method: "pane.zoom", Params: `{"pane_id":"w9:p9","mode":"on"}`},
	}, requests(srv))
	assert.NotContains(t, m.View().Content, "✖")
}

// TestSearchKeys: while a query is typed every printable key is text, q and
// ? included, so neither quits nor opens help; the arrows move among the
// matches; Backspace edits; Esc clears the query and leaves the search,
// keeping the selection.
func TestSearchKeys(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		keys     []tea.KeyPressMsg
		search   bool
		query    string
		selected string // "" for no selection
		hidden   []string
	}{
		"q is text": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("q")), search: true, query: "q",
			selected: "pee02", hidden: []string{"pee01", "clerk"},
		},
		"? is text, and q? matches nothing": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("q?")), search: true, query: "q?",
			hidden: []string{"pee02", "qa"},
		},
		"down moves within the matches": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("pee0"), []tea.KeyPressMsg{keyDown}), search: true,
			query: "pee0", selected: "pee02", hidden: []string{"precheck2", "clerk"},
		},
		"up stops at the first match": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("pee0"), []tea.KeyPressMsg{keyUp}), search: true,
			query: "pee0", selected: "pee01", hidden: []string{"precheck2"},
		},
		"down stops at the last match": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("5255"), repeat(keyDown, 3)), search: true,
			query: "5255", selected: "pee04", hidden: []string{"pee05"},
		},
		"backspace edits": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("qa?"), []tea.KeyPressMsg{keyBackspace}), search: true,
			query: "qa", selected: "qa", hidden: []string{"pee02"},
		},
		"backspace past the start stays in the search": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("q"), repeat(keyBackspace, 3)), search: true,
			selected: "pee01",
		},
		"esc clears and leaves, keeping the selection": {
			keys:     keys([]tea.KeyPressMsg{keySlash}, typing("pee0"), []tea.KeyPressMsg{keyDown, keyEsc}),
			selected: "pee02",
		},
		"esc with no match goes back to the last selection": {
			keys: keys([]tea.KeyPressMsg{keySlash}, typing("pee03zz"), []tea.KeyPressMsg{keyEsc}), selected: "pee03",
		},
		"a burst of text is all typed": {
			keys:   []tea.KeyPressMsg{keySlash, {Code: 'q', Text: "qa q"}},
			search: true, query: "qa q", selected: "qa",
		},
		"a capital letter is text": {
			keys:   []tea.KeyPressMsg{keySlash, {Code: 'q', ShiftedCode: 'Q', Mod: tea.ModShift, Text: "Q"}},
			search: true, query: "Q", selected: "pee02",
		},
		"a non-ASCII letter is text": {
			keys: []tea.KeyPressMsg{keySlash, {Code: 'é', Text: "é"}}, search: true, query: "é",
		},
		"/ while searching is text": {
			keys: []tea.KeyPressMsg{keySlash, keySlash}, search: true, query: "/",
		},
		"alt and ctrl keys are not text": {
			keys: []tea.KeyPressMsg{
				keySlash,
				{Code: 'x', Mod: tea.ModAlt, Text: "x"},
				{Code: 'x', Mod: tea.ModCtrl},
				{Code: 'x', Mod: tea.ModSuper, Text: "x"},
			},
			search: true, selected: "pee01",
		},
		"keys without text are not text": {
			keys:   []tea.KeyPressMsg{keySlash, {Code: tea.KeyTab}, {Code: tea.KeyF1}, keyLeft, keyRight, {}},
			search: true, selected: "pee01",
		},
		"control characters and line breaks are dropped": {
			keys:   []tea.KeyPressMsg{keySlash, {Code: 'q', Text: "q\t\r\na\x00\x1b"}},
			search: true, query: "qa", selected: "qa",
		},
		"invalid UTF-8 is dropped": {
			keys:   []tea.KeyPressMsg{keySlash, {Code: 'q', Text: "q\xffa"}},
			search: true, query: "qa", selected: "qa",
		},
		"a truncated multibyte character is dropped": {
			keys:   []tea.KeyPressMsg{keySlash, {Code: 'q', Text: "qa\xc3"}},
			search: true, query: "qa", selected: "qa",
		},
		"backspace drops a whole multibyte character": {
			keys:   keys([]tea.KeyPressMsg{keySlash}, typing("éé"), []tea.KeyPressMsg{keyBackspace}),
			search: true, query: "é",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
			m := started(t, config(src, rec), 120, 40)

			m, cmd := press(t, m, tc.keys...)
			assert.Nil(t, cmd, "no key quits, focuses or fetches")
			f := m.Frame()
			assert.Equal(t, tc.search, f.Search)
			assert.Equal(t, tc.query, f.Query)
			assert.False(t, f.Help, "? never opens help while searching")
			a, ok := m.Selected()
			if tc.selected == "" {
				assert.False(t, ok, "selected %q", a.Name)
			} else {
				assert.Equal(t, tc.selected, a.Name)
			}
			view := m.View().Content
			for _, h := range tc.hidden {
				assert.NotContains(t, view, h)
			}
			assert.Empty(t, rec.names())
		})
	}
}

// TestSearchWithNoMatch: the groups give way to "no matches", and Enter
// jumps nowhere but still ends the search.
func TestSearchWithNoMatch(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, rec), 80, 24)

	m, _ = press(t, m, keys([]tea.KeyPressMsg{keySlash}, typing("zzz"))...)
	assert.Contains(t, m.View().Content, "no matches")
	assert.NotContains(t, m.View().Content, "no agents in this team")

	m, cmd := press(t, m, keyEnter)
	assert.Nil(t, cmd)
	assert.False(t, m.Frame().Search)
	assert.Equal(t, "pee01", selected(t, m))
	assert.Empty(t, rec.names())
}

// TestSearchQueryIsCapped: a held key cannot grow the query without end.
func TestSearchQueryIsCapped(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, &recorder{}), 80, 24)

	e := tea.KeyPressMsg{Code: 'é', Text: "é"}
	m, _ = press(t, m, keys([]tea.KeyPressMsg{keySlash}, repeat(e, dashboard.MaxQuery-1))...)
	assert.Equal(t, dashboard.MaxQuery-1, utf8.RuneCountInString(m.Frame().Query))
	m, _ = press(t, m, e)
	assert.Equal(t, dashboard.MaxQuery, utf8.RuneCountInString(m.Frame().Query), "the limit itself is kept")
	m, _ = press(t, m, repeat(e, 10)...)
	assert.Equal(t, dashboard.MaxQuery, utf8.RuneCountInString(m.Frame().Query), "past it, keys are dropped")

	m, _ = press(t, m, tea.KeyPressMsg{Code: 'x', Text: strings.Repeat("x", 3*dashboard.MaxQuery)})
	assert.Equal(t, strings.Repeat("é", dashboard.MaxQuery), m.Frame().Query, "a full query takes no more")
}

// TestCtrlCQuitsWhileSearching: ctrl+c is not text, so it still quits.
func TestCtrlCQuitsWhileSearching(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, &recorder{}), 80, 24)

	m, _ = press(t, m, keys([]tea.KeyPressMsg{keySlash}, typing("pee"))...)
	_, cmd := press(t, m, keyCtrlC)
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd())
}

// TestSlashClosesHelp: the help overlay would hide the matches.
func TestSlashClosesHelp(t *testing.T) {
	t.Parallel()

	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, &recorder{}), 80, 24)

	m, _ = press(t, m, keyHelp, keySlash)
	assert.False(t, m.Frame().Help)
	assert.True(t, m.Frame().Search)
	assert.NotContains(t, m.View().Content, "╭ help")
}

// TestSearchRefiltersEveryPoll: each snapshot is narrowed by the query as it
// arrives. The selection stays on its agent while that agent still matches,
// and moves to the first match when it no longer does.
func TestSearchRefiltersEveryPoll(t *testing.T) {
	t.Parallel()

	full := fixture(t, "full")
	// moved is full with NERD-5255 handed from pee04 to pee07.
	moved := *full
	moved.Groups = append([]snapshot.Group(nil), full.Groups...)
	for i, g := range moved.Groups {
		if g.Name != "coders" {
			continue
		}
		g.Agents = append([]snapshot.Agent(nil), g.Agents...)
		for j := range g.Agents {
			switch g.Agents[j].Name {
			case "pee04":
				g.Agents[j].Item = "NERD-5300"
			case "pee07":
				g.Agents[j].Item = "NERD-5255"
			}
		}
		moved.Groups[i] = g
	}

	tests := map[string]struct {
		query string
		down  int
		next  *snapshot.Snapshot
		want  string
		shown []string
	}{
		"the same snapshot again keeps the selection": {
			query: "pee0", down: 2, next: full, want: "pee03",
		},
		"a match that stops matching gives way to the first match": {
			query: "5255", next: &moved, want: "pee07", shown: []string{"pee07"},
		},
		"a selected agent that still matches stays selected": {
			query: "nerd", down: 1, next: &moved, want: "pee05",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := &script{steps: []func() (snapshot.Snapshot, error){serve(full), serve(tc.next)}}
			m := started(t, config(src, &recorder{}), 120, 40)
			m, _ = press(t, m, keys([]tea.KeyPressMsg{keySlash}, typing(tc.query), repeat(keyDown, tc.down))...)

			m = run(t, m, m.Init())
			assert.True(t, m.Frame().Search)
			assert.Equal(t, tc.query, m.Frame().Query)
			assert.Equal(t, tc.want, selected(t, m))
			for _, s := range tc.shown {
				assert.Contains(t, m.View().Content, s)
			}
		})
	}
}

// TestSearchBeforeTheFirstSnapshot: a query typed while the dashboard still
// waits is kept, and narrows the first snapshot when it comes.
func TestSearchBeforeTheFirstSnapshot(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := dashboard.New(t.Context(), config(src, rec))
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})

	m, cmd := press(t, m, keys([]tea.KeyPressMsg{keySlash}, typing("x"), []tea.KeyPressMsg{keyDown, keyBackspace})...)
	assert.Nil(t, cmd)
	m, cmd = press(t, m, typing("x")...)
	assert.Nil(t, cmd)
	_, ok := m.Selected()
	assert.False(t, ok)
	assert.Contains(t, m.View().Content, "waiting for the daemon")

	m = run(t, m, m.Init())
	assert.Equal(t, "codex", selected(t, m), "x matches only codex")
	assert.NotContains(t, m.View().Content, "pee01")

	m, cmd = press(t, m, keyEsc)
	assert.Nil(t, cmd)
	assert.Equal(t, "codex", selected(t, m))
	assert.Empty(t, rec.names())
}

// TestClickJumpEndsTheSearch: a click is a jump like Enter, so it ends the
// search too.
func TestClickJumpEndsTheSearch(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	src := &script{steps: []func() (snapshot.Snapshot, error){serve(fixture(t, "full"))}}
	m := started(t, config(src, rec), 120, 40)
	m, _ = press(t, m, keys([]tea.KeyPressMsg{keySlash}, typing("nerd"))...)
	x, y := locate(t, m.View().Content, "pee07")

	m, cmd := update(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.False(t, m.Frame().Search)
	assert.Equal(t, "pee07", selected(t, m))
	assert.Contains(t, m.View().Content, "pee01")
	run(t, m, cmd)
	assert.Equal(t, []string{"pee07"}, rec.names())
}

// TestRenderSearchFooter: the query is drawn safely, within the width, and
// only a query with words can find nothing.
func TestRenderSearchFooter(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		query   string
		w       int
		want    string
		wantNot []string
	}{
		"control bytes, line breaks and invalid UTF-8 are cleaned": {
			query: "a\x1b[2J\x00\r\n\xffb\xc3", w: 80, want: "/a [2J   �b�▏", wantNot: []string{"\x1b", "\x00"},
		},
		"a long query is cut at the width": {
			query: strings.Repeat("é", dashboard.MaxQuery), w: 40, want: "/" + strings.Repeat("é", 30),
		},
		"white space and line breaks alone on an empty team is no team": {
			query: "  \r\n\t ", w: 80, want: "no agents in this team", wantNot: []string{"no matches"},
		},
		"words on an empty team find nothing": {
			query: "a", w: 80, want: "no matches", wantNot: []string{"no agents in this team"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := dashboard.Frame{Snapshot: &snapshot.Snapshot{}, Now: at, Search: true, Query: tc.query}
			out := dashboard.Render(f, tc.w, 24, plain)
			assert.Contains(t, out, tc.want)
			for _, n := range tc.wantNot {
				assert.NotContains(t, out, n)
			}
			for line := range strings.Lines(out) {
				assert.LessOrEqual(t, ansi.StringWidth(strings.TrimRight(line, "\n")), tc.w)
			}
		})
	}
}

// TestRenderSearchGolden: the footer leads with the query while one is
// typed, and an empty result reads "no matches".
func TestRenderSearchGolden(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		query string
		w, h  int
	}{
		"started_80x24":  {query: "", w: 80, h: 24},
		"cod_120x40":     {query: "cod", w: 120, h: 40},
		"5255_80x24":     {query: "5255", w: 80, h: 24},
		"no_match_80x24": {query: "zzz", w: 80, h: 24},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := dashboard.Frame{
				Snapshot: dashboard.Filter(fixture(t, "full"), tc.query), Now: at, Btop: true,
				Search: true, Query: tc.query,
			}
			golden.RequireEqual(t, dashboard.Render(f, tc.w, tc.h, plain)+"\n")
		})
	}
}

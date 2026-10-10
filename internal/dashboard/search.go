package dashboard

import (
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/abanna/herdr-agentisan/internal/snapshot"
)

// Search (ADR-001 D5, A20): `/` starts a query, and the groups narrow in
// place to the agents it matches as it is typed. Every printable key is
// query text while it runs, q and ? included; the arrows move among the
// matches, Backspace edits, Enter jumps to the selected match and ends the
// search, and Esc ends it where it stands. Anywhere else, prefix+/ opens
// herdr's own Go To (docs/config/herdr-keys.example.toml).

// MaxQuery is the longest query, in runes. Keys past it are dropped, so a
// held key cannot grow the query without end.
const MaxQuery = 64

// noMatches stands in for the groups when a query matches no agent.
const noMatches = "no matches"

// search is the model's search state. The query is kept as typed; matching
// folds its case.
type search struct {
	on    bool
	query string
}

// Match reports whether query matches agent a of group group: each word of
// query, split on white space, appears with its letters in order, gaps
// allowed, case folded, somewhere in the agent's name, group, item and stage
// joined by spaces. Words match independently of one another, so "cod 5255"
// finds the coder on NERD-5255. A query with no words matches every agent.
func Match(query, group string, a snapshot.Agent) bool {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return true
	}
	hay := []rune(strings.ToLower(strings.Join([]string{a.Name, group, a.Item, a.Stage}, " ")))
	for _, w := range words {
		if !subsequence([]rune(w), hay) {
			return false
		}
	}
	return true
}

// subsequence reports whether word's runes all appear in hay, in order.
func subsequence(word, hay []rune) bool {
	i := 0
	for _, r := range hay {
		if i < len(word) && r == word[i] {
			i++
		}
	}
	return i == len(word)
}

// Filter is s narrowed to the agents query matches (see Match). Groups keep
// the snapshot's order and a group left with no match is dropped; the rest
// of the snapshot, the boss and the project, is kept as is. s itself is
// never modified, and a query with no words returns s.
func Filter(s *snapshot.Snapshot, query string) *snapshot.Snapshot {
	if s == nil || len(strings.Fields(query)) == 0 {
		return s
	}
	out := *s
	out.Groups = []snapshot.Group{}
	for _, g := range s.Groups {
		var agents []snapshot.Agent
		for _, a := range g.Agents {
			if Match(query, g.Name, a) {
				agents = append(agents, a)
			}
		}
		if len(agents) > 0 {
			g.Agents = agents
			out.Groups = append(out.Groups, g)
		}
	}
	return &out
}

// searchKey handles a key while a query is typed.
func (m Model) searchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.endSearch()
	case keyEnter:
		cmd := m.focus()
		m.endSearch()
		m.settle()
		return m, cmd
	case keyUp, keyDown:
		m.move(msg.String())
	case "backspace":
		q := m.search.query
		if _, size := utf8.DecodeLastRuneInString(q); size > 0 {
			m.edit(q[:len(q)-size])
		}
	default:
		if text := typed(msg); text != "" {
			m.edit(m.search.query + text)
		}
	}
	m.settle()
	return m, nil
}

// typed is the text a key types into a query: its printable characters, or
// nothing for a key held with ctrl, alt or another modifier but shift.
func typed(msg tea.KeyPressMsg) string {
	if msg.Mod&^(tea.ModShift|tea.ModCapsLock|tea.ModNumLock|tea.ModScrollLock) != 0 {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, msg.Text)
}

// startSearch begins an empty query. It moves nothing: an empty query
// matches every agent. The help overlay would hide the matches, so it
// closes.
func (m *Model) startSearch() {
	m.help = false
	m.search = search{on: true}
	m.refilter()
}

// endSearch drops the query, so every agent is back, and puts the selection
// on the agent it was on.
func (m *Model) endSearch() {
	m.search = search{}
	m.refilter()
	if m.snap != nil {
		m.reanchor()
	}
}

// edit sets the query to q, at most MaxQuery runes of it, and selects the
// first match.
func (m *Model) edit(q string) {
	if utf8.RuneCountInString(q) > MaxQuery {
		q = string([]rune(q)[:MaxQuery])
	}
	m.search.query = q
	m.refilter()
	m.first()
}

// show takes a snapshot as it arrives: it is kept whole, and drawn narrowed
// by the query while one is typed. A selected agent the query no longer
// matches gives way to the first match.
func (m *Model) show(s *snapshot.Snapshot) {
	m.raw = s
	m.refilter()
	if m.search.on && !m.shows(m.anchor) {
		m.first()
	}
}

// refilter draws the last snapshot through the query, if one is typed.
func (m *Model) refilter() {
	m.snap = m.raw
	if m.search.on {
		m.snap = Filter(m.raw, m.search.query)
	}
}

// first selects the first agent drawn, if any. With none, nothing is
// selected, and the anchor stays for when the search ends.
func (m *Model) first() {
	m.sel = Selection{}
	if m.snap == nil {
		return
	}
	for g, grp := range Order(m.snap.Groups) {
		if len(grp.Agents) > 0 {
			m.choose(Selection{Group: g})
			return
		}
	}
}

// shows reports whether an agent named name is drawn.
func (m *Model) shows(name string) bool {
	if m.snap == nil || name == "" {
		return false
	}
	for _, g := range m.snap.Groups {
		for _, a := range g.Agents {
			if a.Name == name {
				return true
			}
		}
	}
	return false
}

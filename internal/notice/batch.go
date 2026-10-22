package notice

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxBody is the most a message's body holds: the messages store's cap
// (store.MaxBody), so a message the log sink writes today fits the inbox
// part 2 sends it to.
const MaxBody = 8 << 10

// MaxLine is the most one notice's line holds; a longer one is cut, so a
// message always has room for its newest line.
const MaxLine = 512

// Message is one team's notices, oldest first, sent to its boss at once.
type Message struct {
	Project string
	Lines   []string
	// Dropped counts the oldest notices cut to keep the body under MaxBody.
	Dropped int
	acks    []func()
}

// Body is the message as one text: a line saying how many notices were cut,
// when any were, then one line per notice.
func (m Message) Body() string {
	lines := m.Lines
	if m.Dropped > 0 {
		lines = append([]string{droppedLine(m.Dropped)}, lines...)
	}
	return strings.Join(lines, "\n")
}

// Settle acknowledges every notice in m. The caller runs it once the sink
// has accepted m.
func (m Message) Settle() {
	for _, ack := range m.acks {
		ack()
	}
}

func droppedLine(n int) string {
	return fmt.Sprintf("(%d earlier notices dropped)", n)
}

// Batcher holds each team's notices until its boss may be told again. Only
// one goroutine uses it, the team poll's.
type Batcher struct {
	// Mutes, when set, are the workers whose notices are dropped (A44).
	Mutes   *Mutes
	configs Configs
	pending map[string][]Notice
	// sent is when each team's boss was last sent a message.
	sent map[string]time.Time
	// passed is when each team's subject and kind was last let through.
	passed map[repeatKey]time.Time
}

type repeatKey struct {
	project, subject string
	kind             Kind
}

// NewBatcher is a Batcher with each team's Config.
func NewBatcher(configs Configs) *Batcher {
	return &Batcher{configs: configs, pending: map[string][]Notice{}, sent: map[string]time.Time{}, passed: map[repeatKey]time.Time{}}
}

// Add queues ns for their bosses, as of now. A notice of a kind its team
// skips, one about a muted worker, or a repeat inside its team's window, is
// dropped and acknowledged here; a line longer than MaxLine is cut. Whose pane a notice is about
// never matters here: a source decides that (the poll's leaves the boss's
// own panes out; a context source may tell the boss about its own).
func (b *Batcher) Add(now time.Time, ns []Notice) {
	for _, n := range ns {
		cfg := b.configs.For(n.Project)
		if (cfg.Skip[n.Kind] && n.Kind.Skippable()) || b.Mutes.Muted(n, now) || b.repeat(cfg, n, now) {
			if n.Ack != nil {
				n.Ack()
			}
			continue
		}
		n.Text = cut(strings.ToValidUTF8(n.Text, "\uFFFD"), MaxLine)
		b.pending[n.Project] = append(b.pending[n.Project], n)
	}
}

// repeat reports whether n repeats a notice let through within its team's
// window, and otherwise records it as let through now. Only a kind that
// Repeats can be a repeat.
func (b *Batcher) repeat(cfg Config, n Notice, now time.Time) bool {
	if !n.Kind.Repeats() || cfg.Repeat <= 0 {
		return false
	}
	k := repeatKey{n.Project, n.Subject, n.Kind}
	if at, ok := b.passed[k]; ok && now.Sub(at) < cfg.Repeat {
		return true
	}
	b.passed[k] = now
	return false
}

// Flush is a message for each team, by project name, whose boss has notices
// waiting and was last sent one at least its Gap before now; those notices
// leave the Batcher. It also forgets what has left its repeat window.
func (b *Batcher) Flush(now time.Time) []Message {
	for k, at := range b.passed {
		if now.Sub(at) >= b.configs.For(k.project).Repeat {
			delete(b.passed, k)
		}
	}
	var out []Message
	for _, project := range slices.Sorted(maps.Keys(b.pending)) {
		ns := b.pending[project]
		if len(ns) == 0 {
			continue
		}
		if at, ok := b.sent[project]; ok && now.Sub(at) < b.configs.For(project).Gap {
			continue
		}
		delete(b.pending, project)
		b.sent[project] = now
		out = append(out, message(project, ns))
	}
	return out
}

// message is ns as one message, the oldest cut until its body fits MaxBody.
func message(project string, ns []Notice) Message {
	size := func(ns []Notice, dropped int) int {
		n := 0
		if dropped > 0 {
			n = len(droppedLine(dropped)) + 1
		}
		for _, x := range ns {
			n += len(x.Text) + 1
		}
		return n - 1
	}
	dropped := 0
	for len(ns) > 1 && size(ns, dropped) > MaxBody {
		ns, dropped = ns[1:], dropped+1
	}
	m := Message{Project: project, Dropped: dropped}
	for _, n := range ns {
		m.Lines = append(m.Lines, n.Text)
		if n.Ack != nil {
			m.acks = append(m.acks, n.Ack)
		}
	}
	return m
}

// cut is s, valid UTF-8, in at most n bytes, cut on a rune boundary and
// ended with "…" when it was longer.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const more = "…"
	s = s[:n-len(more)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + more
}

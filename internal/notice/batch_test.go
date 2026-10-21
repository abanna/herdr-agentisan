package notice_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
)

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func note(project, subject string, k notice.Kind, text string) notice.Notice {
	return notice.Notice{Project: project, Subject: subject, Kind: k, Text: text}
}

// lines is each message's project and lines.
func lines(ms []notice.Message) map[string][]string {
	out := map[string][]string{}
	for _, m := range ms {
		out[m.Project] = m.Lines
	}
	return out
}

// TestBatcherOneMessagePerTeam: each team's notices go out together, its
// boss's own, oldest first, the teams by name.
func TestBatcherOneMessagePerTeam(t *testing.T) {
	t.Parallel()
	b := notice.NewBatcher(notice.Configs{Default: notice.DefaultConfig()})
	b.Add(t0, []notice.Notice{note("ha", "p1", notice.TurnEnded, "a"), note("skills", "p2", notice.Blocked, "b"), note("ha", "p3", notice.Gone, "c")})
	got := b.Flush(t0)
	require.Len(t, got, 2)
	assert.Equal(t, "ha", got[0].Project)
	assert.Equal(t, []string{"a", "c"}, got[0].Lines)
	assert.Equal(t, "a\nc", got[0].Body())
	assert.Equal(t, "skills", got[1].Project)
	assert.Empty(t, b.Flush(t0), "nothing left")
}

// TestBatcherGap: a boss is sent at most one message per gap; what comes
// sooner waits and goes out together, at exactly the gap.
func TestBatcherGap(t *testing.T) {
	t.Parallel()
	b := notice.NewBatcher(notice.Configs{Default: notice.DefaultConfig(), Projects: map[string]notice.Config{"fast": {Gap: -time.Second}}})
	b.Add(t0, []notice.Notice{note("ha", "p1", notice.TurnEnded, "a"), note("fast", "p1", notice.TurnEnded, "x")})
	assert.Len(t, b.Flush(t0), 2)

	b.Add(t0.Add(time.Second), []notice.Notice{note("ha", "p1", notice.TurnEnded, "b"), note("fast", "p1", notice.TurnEnded, "y")})
	assert.Equal(t, map[string][]string{"fast": {"y"}}, lines(b.Flush(t0.Add(time.Second))), "a negative gap is none: it sends every flush")
	b.Add(t0.Add(10*time.Second), []notice.Notice{note("ha", "p2", notice.TurnEnded, "c")})
	assert.Empty(t, b.Flush(t0.Add(notice.DefaultGap-time.Nanosecond)), "inside the gap")
	assert.Equal(t, map[string][]string{"ha": {"b", "c"}}, lines(b.Flush(t0.Add(notice.DefaultGap))), "at the gap, together")
	assert.Empty(t, b.Flush(t0.Add(time.Hour)), "and nothing more")
}

// TestBatcherRepeat: the same subject and kind is dropped for the repeat
// window after it was let through, and acknowledged; a turn ending and a
// start never are.
func TestBatcherRepeat(t *testing.T) {
	t.Parallel()
	cfg := notice.Config{Repeat: notice.DefaultRepeat}
	b := notice.NewBatcher(notice.Configs{Default: cfg, Projects: map[string]notice.Config{"never": {Repeat: -time.Minute}}})
	acked := 0
	ack := func() { acked++ }
	blocked := func(project, subject string) notice.Notice {
		n := note(project, subject, notice.Blocked, subject+" is blocked")
		n.Ack = ack
		return n
	}
	b.Add(t0, []notice.Notice{blocked("ha", "p1")})
	b.Add(t0.Add(notice.DefaultRepeat-time.Nanosecond), []notice.Notice{
		blocked("ha", "p1"), blocked("ha", "p2"), note("ha", "p1", notice.Gone, "gone"),
		note("ha", "p1", notice.TurnEnded, "t1"), note("ha", "p1", notice.TurnEnded, "t2"),
		note("ha", "ha", notice.Started, "s"), note("ha", "ha", notice.Started, "s"),
		blocked("never", "p1"), blocked("never", "p1"),
	})
	assert.Equal(t, 1, acked, "the repeat is acknowledged as it is dropped")
	got := lines(b.Flush(t0.Add(notice.DefaultRepeat - time.Nanosecond)))
	assert.Equal(t, []string{"p1 is blocked", "p2 is blocked", "gone", "t1", "t2", "s", "s"}, got["ha"])
	assert.Equal(t, []string{"p1 is blocked", "p1 is blocked"}, got["never"], "a repeat of 0 or less drops nothing")

	b.Add(t0.Add(notice.DefaultRepeat), []notice.Notice{blocked("ha", "p1")})
	assert.Equal(t, []string{"p1 is blocked"}, lines(b.Flush(t0.Add(notice.DefaultRepeat)))["ha"], "at the window's end it is let through")
}

// TestBatcherSkip drops and acknowledges the kinds a team skips.
func TestBatcherSkip(t *testing.T) {
	t.Parallel()
	b := notice.NewBatcher(notice.Configs{Default: notice.DefaultConfig(), Projects: map[string]notice.Config{
		"ha": {Skip: map[notice.Kind]bool{notice.TurnEnded: true}},
	}})
	acked := false
	n := note("ha", "p1", notice.TurnEnded, "t")
	n.Ack = func() { acked = true }
	b.Add(t0, []notice.Notice{n, note("ha", "p1", notice.Blocked, "b"), note("other", "p1", notice.TurnEnded, "t")})
	assert.True(t, acked)
	assert.Equal(t, map[string][]string{"ha": {"b"}, "other": {"t"}}, lines(b.Flush(t0)))
}

// TestBatcherSettle: a message's notices are acknowledged when the caller
// settles it, once each, and not before.
func TestBatcherSettle(t *testing.T) {
	t.Parallel()
	b := notice.NewBatcher(notice.Configs{})
	acks := map[string]int{}
	add := func(text string) notice.Notice {
		n := note("ha", text, notice.Blocked, text)
		n.Ack = func() { acks[text]++ }
		return n
	}
	b.Add(t0, []notice.Notice{add("a"), note("ha", "b", notice.Blocked, "b"), add("c")})
	got := b.Flush(t0)
	require.Len(t, got, 1)
	assert.Empty(t, acks, "not before the sink accepts")
	got[0].Settle()
	assert.Equal(t, map[string]int{"a": 1, "c": 1}, acks)
}

// TestBatcherCapsTheBody: a body never passes the inbox's cap. The oldest
// notices are cut, unacknowledged, and the body says how many.
func TestBatcherCapsTheBody(t *testing.T) {
	t.Parallel()
	b := notice.NewBatcher(notice.Configs{})
	acked := 0
	line := strings.Repeat("x", notice.MaxLine-4)
	var ns []notice.Notice
	for i := range 40 {
		n := note("ha", string(rune('a'+i)), notice.TurnEnded, line[:len(line)-2]+string(rune('A'+i%26))+string(rune('a'+i%26)))
		n.Ack = func() { acked++ }
		ns = append(ns, n)
	}
	b.Add(t0, ns)
	got := b.Flush(t0)
	require.Len(t, got, 1)
	m := got[0]
	assert.LessOrEqual(t, len(m.Body()), notice.MaxBody)
	assert.Greater(t, m.Dropped, 0)
	assert.Equal(t, 40, m.Dropped+len(m.Lines))
	assert.True(t, strings.HasPrefix(m.Body(), "("), m.Body()[:40])
	assert.Contains(t, m.Body(), "earlier notices dropped)\n")
	assert.Equal(t, ns[39].Text, m.Lines[len(m.Lines)-1], "the newest stays")
	m.Settle()
	assert.Equal(t, len(m.Lines), acked, "a cut notice is not acknowledged")
}

// TestBatcherCutsALine: a line past MaxLine is cut on a rune boundary with
// "…", and invalid UTF-8 is replaced, so every line is valid text.
func TestBatcherCutsALine(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		text string
		want func(t *testing.T, got string)
	}{
		"at the cap, whole": {strings.Repeat("a", notice.MaxLine), func(t *testing.T, got string) {
			t.Helper()
			assert.Equal(t, strings.Repeat("a", notice.MaxLine), got)
		}},
		"one past the cap": {strings.Repeat("a", notice.MaxLine+1), func(t *testing.T, got string) {
			t.Helper()
			assert.Equal(t, strings.Repeat("a", notice.MaxLine-3)+"…", got)
		}},
		"a rune across the cut": {strings.Repeat("é", notice.MaxLine), func(t *testing.T, got string) {
			t.Helper()
			assert.LessOrEqual(t, len(got), notice.MaxLine)
			assert.True(t, utf8.ValidString(got))
			assert.True(t, strings.HasSuffix(got, "é…"))
		}},
		"invalid UTF-8": {"a\xffb", func(t *testing.T, got string) {
			t.Helper()
			assert.Equal(t, "a�b", got)
		}},
		"empty": {"", func(t *testing.T, got string) {
			t.Helper()
			assert.Empty(t, got)
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := notice.NewBatcher(notice.Configs{})
			b.Add(t0, []notice.Notice{note("ha", "p", notice.TurnEnded, tc.text)})
			got := b.Flush(t0)
			require.Len(t, got, 1)
			tc.want(t, got[0].Lines[0])
		})
	}
}

// TestConfigsFor: a team without settings of its own gets the default.
func TestConfigsFor(t *testing.T) {
	t.Parallel()
	c := notice.Configs{Default: notice.DefaultConfig(), Projects: map[string]notice.Config{"ha": {Gap: time.Second}}}
	assert.Equal(t, time.Second, c.For("ha").Gap)
	assert.Equal(t, notice.DefaultGap, c.For("skills").Gap)
	assert.Equal(t, notice.DefaultGap, c.For("").Gap)
}

// TestKindsAndCapsMatch: the kinds are the ones the configuration accepts,
// and a body fits the inbox it will be sent to.
func TestKindsAndCapsMatch(t *testing.T) {
	t.Parallel()
	var names []string
	for _, k := range notice.Kinds {
		names = append(names, string(k))
	}
	assert.Equal(t, settings.NoticeKinds, names)
	assert.Equal(t, store.MaxBody, notice.MaxBody)
	assert.Less(t, notice.MaxLine, notice.MaxBody/2)
}

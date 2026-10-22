package notice_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/notice"
)

// appended is m's body appended to queued, which AppendTo must take.
func appended(t *testing.T, queued string, m notice.Message) string {
	t.Helper()
	body, ok := m.AppendTo(queued)
	require.True(t, ok, "AppendTo refused %q", queued)
	return body
}

// TestAppendToANewNotice: a message alone is the header, then its lines,
// oldest first; one the Batcher cut counts them in the header.
func TestAppendToANewNotice(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "[agentisan] notices:\nc1 is blocked\nc2 finished a turn",
		appended(t, "", notice.Message{Project: "ha", Lines: []string{"c1 is blocked", "c2 finished a turn"}}))
	assert.Equal(t, "[agentisan] notices (3 earlier dropped):\nc1 is blocked",
		appended(t, "", notice.Message{Project: "ha", Lines: []string{"c1 is blocked"}, Dropped: 3}))
}

// TestAppendToAQueuedNotice: the queued notice's lines stay first, the
// message's follow, and the counts add up.
func TestAppendToAQueuedNotice(t *testing.T) {
	t.Parallel()
	first := appended(t, "", notice.Message{Lines: []string{"a"}})
	second := appended(t, first, notice.Message{Lines: []string{"b", "c"}})
	assert.Equal(t, "[agentisan] notices:\na\nb\nc", second)
	third := appended(t, second, notice.Message{Lines: []string{"d"}, Dropped: 2})
	assert.Equal(t, "[agentisan] notices (2 earlier dropped):\na\nb\nc\nd", third)
	assert.Equal(t, "[agentisan] notices (7 earlier dropped):\na\nb\nc\nd\ne",
		appended(t, third, notice.Message{Lines: []string{"e"}, Dropped: 5}))
	assert.Equal(t, "[agentisan] notices:\nx", appended(t, "[agentisan] notices:", notice.Message{Lines: []string{"x"}}),
		"a header with no lines")
}

// TestAppendToRefusesAnotherBody: a queued body AppendTo did not write is
// not appended to, so the caller queues a new notice.
func TestAppendToRefusesAnotherBody(t *testing.T) {
	t.Parallel()
	for _, queued := range []string{
		"[agentisan] message 01J to ha-coder-1 may not have been delivered (timeout); resend if needed: x",
		"[agentisan] originally to ha-coder-1: x",
		"[agentisan] 2 turns ended",
		"notices:\na",
		" [agentisan] notices:\na",
		"[agentisan] notices:  \na",
		"[agentisan] notices:\r\na",
		"[agentisan] notices (0 earlier dropped):\na",
		"[agentisan] notices (01 earlier dropped):\na",
		"[agentisan] notices (-1 earlier dropped):\na",
		"[agentisan] notices (99999999999999999999 earlier dropped):\na",
		"[agentisan] notices (1 earlier dropped)\na",
		"a\n[agentisan] notices:",
	} {
		_, ok := notice.Message{Lines: []string{"x"}}.AppendTo(queued)
		assert.False(t, ok, "%q", queued)
	}
}

// TestAppendToCleansEachLine: every line is one line of valid UTF-8 with
// no control character, NUL included, which the inbox refuses, and at most
// MaxLine bytes: so a line never splits, and a header can never be forged.
func TestAppendToCleansEachLine(t *testing.T) {
	t.Parallel()
	body := appended(t, "", notice.Message{Lines: []string{
		"a\x00b", "c\nd", "[agentisan] notices (9 earlier dropped):\re", "caf\xe9", "\tx\x1b[31m",
	}})
	assert.Equal(t, "[agentisan] notices:\na b\nc d\n[agentisan] notices (9 earlier dropped): e\ncaf�\n x [31m", body)
	again := appended(t, body, notice.Message{Lines: []string{"f"}})
	assert.True(t, strings.HasPrefix(again, "[agentisan] notices:\n"), "a line that looks like a header is not one: %q", again)

	long := appended(t, "", notice.Message{Lines: []string{strings.Repeat("é", notice.MaxLine)}})
	line := strings.TrimPrefix(long, "[agentisan] notices:\n")
	assert.Len(t, line, notice.MaxLine-1, "cut on a rune: 254 é and the ellipsis")
	assert.True(t, strings.HasSuffix(line, "é…"))
	assert.True(t, utf8.ValidString(line))
}

// TestAppendToCapsTheBody: past MaxBody, the oldest lines go, counted in
// the header with those cut before; the newest always stays, and the body,
// header included, never passes MaxBody, however many messages are
// appended (B22 on the inbox's path).
func TestAppendToCapsTheBody(t *testing.T) {
	t.Parallel()
	line := func(i, n int) string {
		s := fmt.Sprintf("%05d ", i)
		return s + strings.Repeat("x", n-len(s))
	}
	full := []string{}
	for i := range notice.MaxBody/100 + 1 {
		full = append(full, line(i, 99))
	}
	body := appended(t, "", notice.Message{Lines: full})
	assert.LessOrEqual(t, len(body), notice.MaxBody)
	head, rest, _ := strings.Cut(body, "\n")
	assert.Equal(t, "[agentisan] notices (1 earlier dropped):", head, "82 lines of 100 bytes do not fit; 81 do")
	assert.Equal(t, full[1:], strings.Split(rest, "\n"))

	body = appended(t, body, notice.Message{Lines: []string{line(9999, 99)}, Dropped: 4})
	head, rest, _ = strings.Cut(body, "\n")
	assert.Equal(t, "[agentisan] notices (6 earlier dropped):", head, "1 before, 4 the Batcher cut, and 1 more for room")
	lines := strings.Split(rest, "\n")
	assert.Equal(t, line(2, 99), lines[0])
	assert.Equal(t, line(9999, 99), lines[len(lines)-1], "the newest stays")

	total, sent := 0, len(full)+1
	for i := range 500 {
		n := 1 + (i*37)%notice.MaxLine
		body = appended(t, body, notice.Message{Lines: []string{line(i, max(n, 7))}})
		sent++
		require.LessOrEqual(t, len(body), notice.MaxBody, "after %d appends", i+1)
		require.True(t, utf8.ValidString(body))
		head, rest, _ = strings.Cut(body, "\n")
		var dropped int
		_, err := fmt.Sscanf(head, "[agentisan] notices (%d earlier dropped):", &dropped)
		require.NoError(t, err, head)
		total = dropped + len(strings.Split(rest, "\n"))
		require.True(t, strings.HasSuffix(body, line(i, max(n, 7))), "the newest stays")
	}
	assert.Equal(t, sent+4, total, "every notice is a line or counted")
}

// TestAppendToAtTheCap: a body of exactly MaxBody bytes, header included,
// is kept whole; one byte more cuts the oldest line.
func TestAppendToAtTheCap(t *testing.T) {
	t.Parallel()
	lines := func(last int) []string {
		out := []string{}
		for i := range 15 {
			out = append(out, strings.Repeat(string(rune('a'+i)), notice.MaxLine-1))
		}
		return append(out, strings.Repeat("z", last))
	}
	// 20 bytes of header, 15 lines of 1+511, and 1+491: 8,192.
	body := appended(t, "", notice.Message{Lines: lines(491)})
	assert.Len(t, body, notice.MaxBody)
	assert.True(t, strings.HasPrefix(body, "[agentisan] notices:\naaa"))

	body = appended(t, "", notice.Message{Lines: lines(492)})
	assert.Less(t, len(body), notice.MaxBody)
	assert.True(t, strings.HasPrefix(body, "[agentisan] notices (1 earlier dropped):\nbbb"), body[:60])
}

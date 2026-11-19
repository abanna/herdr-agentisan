package notice_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/notice"
)

// flushOne adds ns to a fresh Batcher and returns the one message it
// flushes.
func flushOne(t *testing.T, ns []notice.Notice) notice.Message {
	t.Helper()
	b := notice.NewBatcher(notice.Configs{})
	b.Add(t0, ns)
	got := b.Flush(t0)
	require.Len(t, got, 1)
	return got[0]
}

// longNotices are n notices of MaxLine bytes each, oldest first, each
// acknowledged into acks by its subject, so n of them cannot fit one body.
func longNotices(n int, acks map[string]int) []notice.Notice {
	ns := make([]notice.Notice, 0, n)
	for i := range n {
		subject := "p" + string(rune('a'+i))
		x := note("ha", subject, notice.TurnEnded, subject+strings.Repeat("x", notice.MaxLine-len(subject)))
		x.Ack = func() { acks[subject]++ }
		ns = append(ns, x)
	}
	return ns
}

// TestMessageEntriesKeepEachNoticesFields: a message keeps, for the board,
// each notice's kind, subject and worker beside its line, oldest first
// (A74).
func TestMessageEntriesKeepEachNoticesFields(t *testing.T) {
	t.Parallel()
	ns := []notice.Notice{
		{Project: "ha", Subject: "w14:p2", Worker: "ha-coder-1", Kind: notice.Blocked, Text: "ha-coder-1 is blocked"},
		{Project: "ha", Subject: "#50", Kind: notice.Merged, Text: "#50 merged as 1e0fb1b"},
		{Project: "ha", Subject: "w14:p3", Worker: "ha-qa-1", Kind: notice.CtxCeiling, Text: "ha-qa-1 is at 80% context"},
	}
	assert.Equal(t, []notice.Entry{
		{Kind: notice.Blocked, Subject: "w14:p2", Worker: "ha-coder-1", Body: "ha-coder-1 is blocked"},
		{Kind: notice.Merged, Subject: "#50", Body: "#50 merged as 1e0fb1b"},
		{Kind: notice.CtxCeiling, Subject: "w14:p3", Worker: "ha-qa-1", Body: "ha-qa-1 is at 80% context"},
	}, flushOne(t, ns).Entries())
}

// TestMessageEntriesIncludeTheCut: the notices a message's lines cut to fit
// the inbox's cap are still its entries, every one, oldest first.
func TestMessageEntriesIncludeTheCut(t *testing.T) {
	t.Parallel()
	ns := longNotices(20, map[string]int{})
	m := flushOne(t, ns)
	require.Positive(t, m.Dropped, "the lines are cut")
	assert.Len(t, m.Lines, 20-m.Dropped)
	entries := m.Entries()
	require.Len(t, entries, 20)
	for i, e := range entries {
		assert.Equal(t, ns[i].Subject, e.Subject, "entry %d", i)
		assert.Equal(t, ns[i].Text, e.Body, "entry %d", i)
	}
}

// TestSettleAllAcksTheCutNotices: Settle acknowledges the notices a
// message's lines carry, as before; SettleAll every notice it holds, the
// cut ones included, once each.
func TestSettleAllAcksTheCutNotices(t *testing.T) {
	t.Parallel()
	kept := map[string]int{}
	m := flushOne(t, longNotices(20, kept))
	m.Settle()
	assert.Len(t, kept, len(m.Lines), "Settle: the kept notices")
	assert.Less(t, len(kept), 20)

	all := map[string]int{}
	flushOne(t, longNotices(20, all)).SettleAll()
	assert.Len(t, all, 20, "SettleAll: every notice")
	for subject, n := range all {
		assert.Equal(t, 1, n, "%s acknowledged once", subject)
	}
}

// TestEntryBodyIsOneCleanLine: an entry's body is its notice's line as the
// inbox would write it, valid UTF-8 with every control character a space,
// and "(no text)" for an empty one, so a post is never empty.
func TestEntryBodyIsOneCleanLine(t *testing.T) {
	t.Parallel()
	m := flushOne(t, []notice.Notice{
		note("ha", "p1", notice.Blocked, "a\tb"),
		note("ha", "p2", notice.Blocked, "c\x00d"),
		note("ha", "p3", notice.Blocked, "e\nf"),
		note("ha", "p4", notice.Blocked, ""),
		note("ha", "p5", notice.Blocked, "g\xffh"),
		note("ha", "p6", notice.Blocked, "i\r\nj"),
		note("ha", "p7", notice.Blocked, "\uFEFFk"),
	})
	var bodies []string
	for _, e := range m.Entries() {
		bodies = append(bodies, e.Body)
	}
	assert.Equal(t, []string{"a b", "c d", "e f", "(no text)", "g\uFFFDh", "i  j", "\uFEFFk"}, bodies,
		"invalid UTF-8 replaced, CRLF two spaces, a BOM kept as text")
}

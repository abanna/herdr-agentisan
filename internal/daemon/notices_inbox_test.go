package daemon_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
)

// quietNotices is a [notices] table that skips every kind a team can skip.
// Since A44's part 2 each boss gets its notices in the inbox, the start's
// from the first poll, which the tests of the workforce, send, the
// dispatcher and the status events would otherwise find among the messages,
// prompts and events they count; the notices tests use configs of their own.
var quietNotices = "[notices]\nskip = [\"" + strings.Join(slices.DeleteFunc(slices.Clone(settings.NoticeKinds), func(k string) bool { return k == "ctx_ceiling" }), "\", \"") + "\"]\n"

// quietConfig is a config dir whose shared file holds only quietNotices.
func quietConfig(t *testing.T) string {
	t.Helper()
	return configDir(t, quietNotices)
}

// scratchStore is a fresh state.db in a temp dir.
func scratchStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// noticeLog is each notice line in logs, decoded.
func noticeLog(t *testing.T, logs *lockedLog) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(logs.String()) {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &e), line)
		if e["message"] == "notice" {
			out = append(out, e)
		}
	}
	return out
}

// TestANoticeIsTypedInThePollThatQueuedIt is M7 (A44): the poll flushes its
// notices before it gives the dispatcher its model, which wakes it, so a
// notice is typed into a boss that is in its place in that same pass. The
// poll runs once an hour here, so only the first poll's wake can type it.
func TestANoticeIsTypedInThePollThatQueuedIt(t *testing.T) {
	t.Parallel()
	th, p, dir := haTeam(t)
	o := teamOpts(t, dir, th, haNotices(t, ""))
	o.Poll = time.Hour
	serving(t, o, th.fakeHerdr)
	got := p.next(t)
	assert.Equal(t, typed{Target: "ha-boss", Pane: "w14:p1",
		Text: "[agentisan] notices:\nagentisan: the daemon started; turns that ended while it was down are not reported"}, got)
	assert.Equal(t, 1, lists(th.srv), "typed after the first poll, the only one")
}

// TestInboxNoticeLine: each message is logged with its boss and the inbox
// message it went into, which the next one appends to while it is queued;
// only a new one is a queued message event, with the daemon its sender and
// the boss its recipient.
func TestInboxNoticeLine(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	events, rec := telemetrytest.NewEvents()
	o := daemon.Options{Logger: logger, Now: time.Now, Team: &daemon.TeamOptions{Events: events}}
	st := scratchStore(t)
	bosses := map[string]string{"ha": "ha-boss", "": "boss"}
	require.NoError(t, daemon.InboxNotice(o, st, bosses, notice.Message{Project: "ha", Lines: []string{"a"}}))
	require.NoError(t, daemon.InboxNotice(o, st, bosses, notice.Message{Project: "ha", Lines: []string{"b"}}))
	require.NoError(t, daemon.InboxNotice(o, st, bosses, notice.Message{Project: "", Lines: []string{"c"}}))

	es := noticeLog(t, &logs)
	require.Len(t, es, 3)
	first, _ := es[0]["inbox"].(string)
	assert.Len(t, first, 26)
	assert.Equal(t, map[string]any{"boss": "ha-boss", "inbox": first, "appended": false, "project": "ha", "notices": []any{"a"}},
		map[string]any{"boss": es[0]["boss"], "inbox": es[0]["inbox"], "appended": es[0]["appended"], "project": es[0]["project"], "notices": es[0]["notices"]})
	assert.Equal(t, first, es[1]["inbox"])
	assert.Equal(t, true, es[1]["appended"])
	assert.Equal(t, "boss", es[2]["boss"], "the ◆ spaces' boss, for the unnamed project")
	assert.NotEqual(t, first, es[2]["inbox"])

	m, err := st.Message(t.Context(), first)
	require.NoError(t, err)
	assert.Equal(t, "[agentisan] notices:\na\nb", m.Body)
	assert.Equal(t, store.Address{Kind: store.KindBoss, ID: "ha-boss"}, m.To)

	var queued []telemetrytest.Event
	for _, e := range rec.Events() {
		if e.Name == telemetry.EventMessageQueued {
			queued = append(queued, e)
		}
	}
	require.Len(t, queued, 2, "one event a new notice; none for an append")
	assert.Equal(t, map[string]any{
		"agentisan.message.id": first, "agentisan.message.from.kind": "daemon", "agentisan.message.from.id": "agentisan",
		"agentisan.message.to.kind": "boss", "agentisan.message.to.id": "ha-boss",
	}, subset(queued[0].Attrs, "agentisan.message.id", "agentisan.message.from.kind", "agentisan.message.from.id", "agentisan.message.to.kind", "agentisan.message.to.id"))
	for _, r := range rec.Records() {
		assert.NotContains(t, r.Body().AsString(), "notices:", "the event carries no body")
	}
}

// subset is m's entries at keys.
func subset(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}

// TestInboxNoticeWithNoBoss (Q8): a message for a project no team's boss is
// configured for is logged and settled: queued nowhere, and no error, so a
// source that acknowledges it does not tell it again.
func TestInboxNoticeWithNoBoss(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	o := daemon.Options{Logger: logger, Now: time.Now, Team: &daemon.TeamOptions{}}
	st := scratchStore(t)
	require.NoError(t, daemon.InboxNotice(o, st, map[string]string{"ha": "ha-boss"}, notice.Message{Project: "nobody", Lines: []string{"#50 merged as 1e0fb1b"}}))
	es := noticeLog(t, &logs)
	require.Len(t, es, 1)
	assert.Empty(t, es[0]["boss"])
	assert.NotContains(t, es[0], "inbox")
	assert.Equal(t, []any{"#50 merged as 1e0fb1b"}, es[0]["notices"])
	ids, err := st.QueuedRecipients(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ids)

	acked := false
	b := notice.NewBatcher(notice.Configs{})
	b.Add(time.Unix(0, 0), []notice.Notice{{Project: "nobody", Kind: notice.Merged, Text: "x", Ack: func() { acked = true }}})
	daemon.FlushNotices(daemon.Options{Logger: logger, Now: func() time.Time { return time.Unix(0, 0) }}, b, sinkFunc(func(m notice.Message) error {
		return daemon.InboxNotice(o, st, map[string]string{}, m)
	}))
	assert.True(t, acked, "settled")
}

// sinkFunc is a notice.Sink from a func.
type sinkFunc func(notice.Message) error

func (f sinkFunc) Send(_ context.Context, m notice.Message) error { return f(m) }

// TestInboxNoticeOnAFailedStore: a message the store cannot queue is
// logged all the same, without an inbox message, and the error returned,
// so the flush leaves its notices unsettled.
func TestInboxNoticeOnAFailedStore(t *testing.T) {
	t.Parallel()
	var logs lockedLog
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	o := daemon.Options{Logger: logger, Now: time.Now, Team: &daemon.TeamOptions{}}
	st := scratchStore(t)
	require.NoError(t, st.Close())
	err = daemon.InboxNotice(o, st, map[string]string{"ha": "ha-boss"}, notice.Message{Project: "ha", Lines: []string{"a"}})
	require.ErrorIs(t, err, store.ErrStore)
	assert.Contains(t, err.Error(), "ha-boss")
	es := noticeLog(t, &logs)
	require.Len(t, es, 1)
	assert.Equal(t, "ha-boss", es[0]["boss"])
	assert.NotContains(t, es[0], "inbox")
	assert.NotContains(t, es[0], "appended")
}

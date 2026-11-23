package workforce_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/workforce"
)

// sendConfig is teamConfig plus a second tabs team, assay.
const sendConfig = teamConfig + `
[projects.assay]
repo = "REPO"
short = "as"
layout = "tabs"
`

// terminal is the terminal pane shows.
func (f *fixture) terminal(t *testing.T, pane string) string {
	t.Helper()
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	p := f.h.pane(pane)
	require.NotNil(t, p, pane)
	return p.terminal
}

// TestSendFindsTheSenderAndTheRecipient is B4 and UQ2: the sender is the
// worker a live row binds to the caller pane's terminal, else the boss of
// the team whose boss place holds the pane, else the pane by its terminal;
// with no pane confirmed, the live worker HERDR_AGENTISAN_WORKER names. boss
// is the sender's own team's boss, its <short>-boss, another team's
// included.
func TestSendFindsTheSenderAndTheRecipient(t *testing.T) {
	t.Parallel()
	f := newFixture(t, sendConfig)
	coder := f.worker(t, "ha-coder-1", "coder")
	scratch := f.h.addTab(f.ws, "scratch", "", "")
	notes := f.h.addTab(f.h.addWorkspace("notes"), "notes", "claude", "idle")
	assay := f.h.addWorkspace("assay")
	asBoss := f.h.addTab(assay, "as-boss", "claude", "working")
	asPane := f.h.addTab(assay, "as-coder-1", "claude", "idle")
	inBossTab := f.h.addPane(f.boss, "claude", "idle") // a split in the boss tab
	f.bindRow(t, "ha-coder-7", "coder", inBossTab)
	lost := f.worker(t, "ha-coder-2", "coder")
	f.markLost(t, "ha-coder-2") // the poll lost it once; its pane still shows its terminal
	f.worker(t, "ha-coder-3", "coder")
	f.exec(t, "UPDATE workers SET status = ? WHERE logical_id = 'ha-coder-3'", store.StatusStarting)
	codex := f.workerAs(t, "ha-coder-4", "coder", "codex") // send goes by terminal, whatever the agent (NERD-5507)

	worker := workforce.Party{Kind: store.KindWorker, ID: "ha-coder-1"}
	named := func(id string) workforce.Party { return workforce.Party{Kind: store.KindWorker, ID: id} }
	haBoss := workforce.Party{Kind: store.KindBoss, ID: "ha-boss"}
	theirBoss := workforce.Party{Kind: store.KindBoss, ID: "as-boss"}
	pane := func(p string) workforce.Party { return workforce.Party{Kind: store.KindPane, ID: f.terminal(t, p)} }
	tests := map[string]struct {
		req      workforce.SendRequest
		from, to workforce.Party
	}{
		"a bound worker, to its boss":          {workforce.SendRequest{To: "boss", CallerPane: coder.PaneID}, worker, haBoss},
		"the boss's pane, to a worker":         {workforce.SendRequest{To: "ha-coder-1", CallerPane: f.boss}, haBoss, worker},
		"a team pane no row binds, to boss":    {workforce.SendRequest{To: "boss", CallerPane: scratch}, pane(scratch), haBoss},
		"a pane in no team, to a boss by name": {workforce.SendRequest{To: "ha-boss", CallerPane: notes}, pane(notes), haBoss},
		"another team's boss, to boss":         {workforce.SendRequest{To: "boss", CallerPane: asBoss}, theirBoss, theirBoss},
		"another team's pane, to boss":         {workforce.SendRequest{To: "boss", CallerPane: asPane}, pane(asPane), theirBoss},
		"a boss, to another team's boss":       {workforce.SendRequest{To: "ha-boss", CallerPane: asBoss}, theirBoss, haBoss},
		"no pane, the worker's variable":       {workforce.SendRequest{To: "boss", Worker: "ha-coder-1"}, worker, haBoss},
		"a pane herdr lists nowhere":           {workforce.SendRequest{To: "boss", CallerPane: "w99:p1", Worker: "ha-coder-1"}, worker, haBoss},
		"a confirmed pane beats the variable":  {workforce.SendRequest{To: "boss", CallerPane: scratch, Worker: "ha-coder-1"}, pane(scratch), haBoss},
		"a worker bound in the boss tab":       {workforce.SendRequest{To: "boss", CallerPane: inBossTab}, named("ha-coder-7"), haBoss},
		"a lost row's pane":                    {workforce.SendRequest{To: "boss", CallerPane: lost.PaneID}, named("ha-coder-2"), haBoss},
		"no pane, a lost worker's variable":    {workforce.SendRequest{To: "boss", Worker: "ha-coder-2"}, named("ha-coder-2"), haBoss},
		"no pane, a starting worker's":         {workforce.SendRequest{To: "boss", Worker: "ha-coder-3"}, named("ha-coder-3"), haBoss},
		"a Codex worker, to its boss":          {workforce.SendRequest{To: "boss", CallerPane: codex.PaneID}, named("ha-coder-4"), haBoss},
		"the boss's pane, to a Codex worker":   {workforce.SendRequest{To: "ha-coder-4", CallerPane: f.boss}, haBoss, named("ha-coder-4")},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			tc.req.Body = "hello"
			got, err := f.svc.Send(t.Context(), tc.req)
			require.NoError(t, err)
			assert.Equal(t, tc.from, got.From)
			assert.Equal(t, tc.to, got.To)
			assert.Equal(t, store.MessageQueued, got.State)
			m, err := f.st.Message(t.Context(), got.ID)
			require.NoError(t, err)
			assert.Equal(t, store.Address(tc.from), m.From)
			assert.Equal(t, store.Address(tc.to), m.To)
			assert.Equal(t, "hello", m.Body)
		})
	}
}

func TestAPaneInAFormerBossSpaceSendsAsAPane(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	pane := f.h.addTab(f.h.addWorkspace("◆ boss"), "1", "claude", "working")
	got, err := f.svc.Send(t.Context(), workforce.SendRequest{To: "ha-boss", Body: "hi", CallerPane: pane})
	require.NoError(t, err)
	from := workforce.Party{Kind: store.KindPane, ID: f.terminal(t, pane)}
	assert.Equal(t, from, got.From)
	assert.Equal(t, workforce.Party{Kind: store.KindBoss, ID: "ha-boss"}, got.To)
	msg, err := f.st.Message(t.Context(), got.ID)
	require.NoError(t, err)
	assert.Equal(t, store.Address(from), msg.From)
}

func TestSendBossFromAFormerSpaceIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	f.h.addTab(f.h.addWorkspace("◆ boss"), "1", "claude", "working")
	coder := f.h.addTab(f.h.addWorkspace("◆ coders"), "1", "", "")
	outside := f.h.addTab(f.h.addWorkspace("notes"), "1", "", "")
	for _, pane := range []string{coder, outside} {
		_, err := f.svc.Send(t.Context(), workforce.SendRequest{To: "boss", Body: "hi", CallerPane: pane})
		assert.ErrorIs(t, err, workforce.ErrUnknownRecipient, "caller %s", pane)
	}
	ids, err := f.st.QueuedRecipients(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ids)
}

// TestSendFromTheDashboardIsAPane is the cutover's F2 in send: the team's
// dashboard pane in the boss's tab is not the boss, so a send from it is
// the pane's, by its terminal, whichever of its two labels it has.
func TestSendFromTheDashboardIsAPane(t *testing.T) {
	t.Parallel()
	for name, label := range map[string]string{"labelled by team up": "ha-dashboard", "opened with prefix+d": dashTitle} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upFixture(t, sendConfig)
			dash := f.h.addPane(f.boss, "", "unknown")
			f.h.setPane(dash, func(p *fakePane) { p.label = label })
			got, err := f.svc.Send(t.Context(), workforce.SendRequest{To: "boss", CallerPane: dash, Body: "hello"})
			require.NoError(t, err)
			assert.Equal(t, workforce.Party{Kind: store.KindPane, ID: f.terminal(t, dash)}, got.From)
			assert.Equal(t, workforce.Party{Kind: store.KindBoss, ID: "ha-boss"}, got.To)
		})
	}
}

// TestSendKnowsAWorkerByItsTerminal: herdr reissues pane ids, so a worker
// whose pane has a new id is still found by the terminal its row binds.
func TestSendKnowsAWorkerByItsTerminal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	coder := f.worker(t, "ha-coder-1", "coder")
	f.h.setPane(coder.PaneID, func(p *fakePane) { p.id = "w10:p77" })

	got, err := f.svc.Send(t.Context(), workforce.SendRequest{To: "boss", Body: "hi", CallerPane: "w10:p77"})
	require.NoError(t, err)
	assert.Equal(t, workforce.Party{Kind: store.KindWorker, ID: "ha-coder-1"}, got.From)
}

// TestSendNeverMatchesAnEmptyTerminal: a pane herdr lists with no terminal
// confirms no sender, so it never takes the worker of a row written before
// terminals were recorded, which has none either.
func TestSendNeverMatchesAnEmptyTerminal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, teamConfig)
	pane := f.h.addTab(f.ws, "ha-coder-3", "claude", "idle")
	f.h.setPane(pane, func(p *fakePane) { p.terminal = "" })
	f.exec(t, "INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at, terminal_id) VALUES ('ha-coder-3', 'herdr-agentisan', 'coder', ?, 0, 'active', 0, '')", pane)

	_, err := f.svc.Send(t.Context(), workforce.SendRequest{To: "boss", Body: "hi", CallerPane: pane})
	require.ErrorIs(t, err, workforce.ErrUnknownSender)
}

// TestSendRefuses: each refusal is its sentinel, and nothing is queued.
func TestSendRefuses(t *testing.T) {
	t.Parallel()
	f := newFixture(t, sendConfig)
	coder := f.worker(t, "ha-coder-1", "coder")
	f.worker(t, "ha-coder-2", "coder")
	require.NoError(t, f.st.SetWorkerStatus(t.Context(), store.Worker{LogicalID: "ha-coder-2"}, store.StatusRetired, time.Now()))
	notes := f.h.addTab(f.h.addWorkspace("notes"), "notes", "claude", "idle")
	tests := map[string]struct {
		req  workforce.SendRequest
		want error
	}{
		"no recipient":                  {workforce.SendRequest{Body: "x", CallerPane: coder.PaneID}, workforce.ErrBadRequest},
		"no pane and no variable":       {workforce.SendRequest{To: "boss", Body: "x"}, workforce.ErrUnknownSender},
		"a variable no live row has":    {workforce.SendRequest{To: "boss", Body: "x", Worker: "ha-coder-9"}, workforce.ErrUnknownSender},
		"a retired worker's variable":   {workforce.SendRequest{To: "boss", Body: "x", Worker: "ha-coder-2"}, workforce.ErrUnknownSender},
		"boss from a pane in no team":   {workforce.SendRequest{To: "boss", Body: "x", CallerPane: notes}, workforce.ErrUnknownRecipient},
		"a boss no team has":            {workforce.SendRequest{To: "zz-boss", Body: "x", CallerPane: coder.PaneID}, workforce.ErrUnknownRecipient},
		"a boss's name in another case": {workforce.SendRequest{To: "HA-Boss", Body: "x", CallerPane: coder.PaneID}, workforce.ErrUnknownRecipient},
		"a worker no row binds":         {workforce.SendRequest{To: "ha-coder-9", Body: "x", CallerPane: coder.PaneID}, workforce.ErrUnknownRecipient},
		"a retired worker":              {workforce.SendRequest{To: "ha-coder-2", Body: "x", CallerPane: coder.PaneID}, workforce.ErrUnknownRecipient},
		"an empty body":                 {workforce.SendRequest{To: "boss", CallerPane: coder.PaneID}, store.ErrInvalid},
		"a body with a NUL":             {workforce.SendRequest{To: "boss", Body: "a\x00b", CallerPane: coder.PaneID}, store.ErrInvalid},
		"a body over the cap":           {workforce.SendRequest{To: "boss", Body: strings.Repeat("x", store.MaxBody+1), CallerPane: coder.PaneID}, store.ErrTooLarge},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.Send(t.Context(), tc.req)
			require.ErrorIs(t, err, tc.want)
		})
	}
	queued, err := f.st.QueuedRecipients(t.Context())
	require.NoError(t, err)
	assert.Empty(t, queued, "nothing queued")

	f.h.mu.Lock()
	f.h.fail["pane.list"] = "internal"
	f.h.mu.Unlock()
	_, err = f.svc.Send(t.Context(), workforce.SendRequest{To: "boss", Body: "x", CallerPane: coder.PaneID})
	require.ErrorIs(t, err, workforce.ErrHerdr)
}

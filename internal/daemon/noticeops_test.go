package daemon_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/logging"
	"github.com/abanna/herdr-agentisan/internal/notice"
	"github.com/abanna/herdr-agentisan/internal/team"
)

// opsTeam is "ha" with its boss and one coder, bound as ha-coder-1.
func opsTeam() *team.Model {
	return &team.Model{Projects: []team.Project{{
		Name:   "ha",
		Boss:   []team.Worker{{ID: "w14:p1", Pane: team.Pane{ID: "w14:p1", Name: "ha-boss", Agent: "claude"}}},
		Groups: []team.Group{{Name: "coder", Workers: []team.Worker{{ID: "ha-coder-1", Bound: true, Pane: team.Pane{ID: "w14:p2", Name: "ha-coder-1", Agent: "claude"}}}}},
	}}}
}

// TestNoticeOps: mute answers the mute, an hour by default; list the mutes
// in force; unmute whether one ended. Each refusal has its code: a name no
// worker has, a length past a day or below nothing, args the op does not
// take, and no model yet to find the worker in.
func TestNoticeOps(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	o := daemon.Options{Now: func() time.Time { return now }}
	mutes := notice.NewMutes()
	op := func(m *team.Model, name, args string) (any, string, error) {
		return daemon.NoticeOp(o, m, mutes, name, json.RawMessage(args))
	}

	got, code, err := op(opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-coder-1"}`)
	require.NoError(t, err)
	assert.Empty(t, code)
	assert.Equal(t, notice.Mute{Worker: "ha-coder-1", Until: now.Add(time.Hour)}, got)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"worker":"ha-coder-1","until":"2026-10-22T22:00:00Z"}`, string(raw))

	got, _, err = op(opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-coder-1","for":600000000000}`)
	require.NoError(t, err)
	assert.Equal(t, now.Add(10*time.Minute), got.(notice.Mute).Until, "for is nanoseconds, as a time.Duration")

	got, _, err = op(nil, daemon.OpNoticesList, ``)
	require.NoError(t, err)
	assert.Equal(t, daemon.MutesResult{Mutes: []notice.Mute{{Worker: "ha-coder-1", Until: now.Add(10 * time.Minute)}}}, got, "list needs no model")

	got, _, err = op(nil, daemon.OpNoticesUnmute, `{"worker":"ha-coder-1"}`)
	require.NoError(t, err)
	assert.Equal(t, daemon.UnmuteResult{Worker: "ha-coder-1", Ended: true}, got, "unmute needs no model")
	got, _, err = op(nil, daemon.OpNoticesUnmute, `{"worker":"ha-coder-1"}`)
	require.NoError(t, err)
	assert.Equal(t, daemon.UnmuteResult{Worker: "ha-coder-1", Ended: false}, got)
	got, _, err = op(nil, daemon.OpNoticesList, `{}`)
	require.NoError(t, err)
	raw, err = json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"mutes":[]}`, string(raw), "none is an empty list")

	tests := map[string]struct {
		model      *team.Model
		name, args string
		code       string
		want       error
	}{
		"a name no worker has":    {opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-coder-9"}`, "unknown_worker", notice.ErrUnknownWorker},
		"the boss":                {opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-boss"}`, "unknown_worker", notice.ErrUnknownWorker},
		"no worker":               {opsTeam(), daemon.OpNoticesMute, `{}`, "unknown_worker", notice.ErrUnknownWorker},
		"past a day":              {opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-coder-1","for":86400000000001}`, "bad_request", notice.ErrMuteLength},
		"below nothing":           {opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-coder-1","for":-1}`, "bad_request", notice.ErrMuteLength},
		"for as a string":         {opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-coder-1","for":"1h"}`, "bad_request", nil},
		"an arg mute takes not":   {opsTeam(), daemon.OpNoticesMute, `{"worker":"ha-coder-1","project":"ha"}`, "bad_request", nil},
		"no model yet":            {nil, daemon.OpNoticesMute, `{"worker":"ha-coder-1"}`, "not_ready", daemon.ErrNotReady},
		"an arg unmute takes not": {nil, daemon.OpNoticesUnmute, `{"worker":"ha-coder-1","for":1}`, "bad_request", nil},
		"an arg list takes not":   {nil, daemon.OpNoticesList, `{"worker":"ha-coder-1"}`, "bad_request", nil},
		"args that are no object": {nil, daemon.OpNoticesList, `[1]`, "bad_request", nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := notice.NewMutes()
			_, code, err := daemon.NoticeOp(o, tc.model, s, tc.name, json.RawMessage(tc.args))
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			assert.Equal(t, tc.code, code)
			assert.Empty(t, s.List(now), "nothing muted")
		})
	}
}

// TestNoticeOpBudgets: each notices op has a budget of its own, 5 s, in
// the client's wait and in the daemon's op map alike.
func TestNoticeOpBudgets(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{daemon.OpNoticesList, daemon.OpNoticesMute, daemon.OpNoticesUnmute}, daemon.NoticeOpNames())
	for _, op := range daemon.NoticeOpNames() {
		assert.Equal(t, 5*time.Second, daemon.OpBudget(op, nil), "the client's wait: %s", op)
		assert.Equal(t, 5*time.Second, daemon.ServedNoticeBudget(op), "the budget the daemon enforces: %s", op)
	}
	assert.Equal(t, 5*time.Second, daemon.NoticesBudget)
	for _, op := range daemon.NoticeOpNames() {
		assert.Equal(t, 20*time.Second, daemon.NotReadyWindow+daemon.OpBudget(op, nil)+daemon.IOTimeout,
			"%s: the window, its budget and an exchange, well within the wrapper's 115 s (A52)", op)
	}
}

// TestMuteOverTheSocket is A44's mute end to end: a muted worker's notices
// are dropped, a status change and its way back alike, while another
// worker's are told; once unmuted, the next is told, since a dropped notice
// is never a repeat. The daemon answers each op over its socket.
func TestMuteOverTheSocket(t *testing.T) {
	t.Parallel()
	th, _, dir := haTeam(t)
	th.setAgents(named("w14:p1", "ha-boss"), named("w14:p2", "ha-coder-1"), turned("wN:p1", "pee01", 1))
	var logs lockedLog
	o := teamOpts(t, dir, th, haNotices(t, "[notices]\ngap = 0\n"))
	logger, err := logging.New(&logs, "info", "json")
	require.NoError(t, err)
	o.Logger = logger
	serving(t, o, th.fakeHerdr)
	sock := socketPath(t, dir, th.path)
	waitLine(t, &logs, "the daemon started")

	data, err := daemon.Workforce(t.Context(), sock, daemon.OpNoticesMute, daemon.MuteRequest{Worker: "ha-coder-1"})
	require.NoError(t, err)
	var m notice.Mute
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, "ha-coder-1", m.Worker)
	assert.WithinDuration(t, time.Now().Add(time.Hour), m.Until, time.Minute)

	th.setPane("w14:p2", "agent_status", "blocked")
	th.setPane("wN:p1", "agent_status", "blocked")
	waitLine(t, &logs, "pee01 is blocked")
	th.setPane("w14:p2", "agent_status", "working")
	polled(t, th.srv, 3)
	for _, l := range noticeLines(&logs) {
		assert.NotContains(t, l, "ha-coder-1", "muted")
	}

	data, err = daemon.Workforce(t.Context(), sock, daemon.OpNoticesList, struct{}{})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"worker":"ha-coder-1"`)
	data, err = daemon.Workforce(t.Context(), sock, daemon.OpNoticesUnmute, daemon.UnmuteRequest{Worker: "ha-coder-1"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"worker":"ha-coder-1","ended":true}`, string(data))

	th.setPane("w14:p2", "agent_status", "blocked")
	waitLine(t, &logs, "ha-coder-1 is blocked")

	_, err = daemon.Workforce(t.Context(), sock, daemon.OpNoticesMute, daemon.MuteRequest{Worker: "ha-coder-9"})
	var re *daemon.RequestError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "unknown_worker", re.Code)
}

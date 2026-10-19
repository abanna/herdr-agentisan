package daemon_test

import (
	"database/sql"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/settings"
	"github.com/abanna/herdr-agentisan/internal/store"
	"github.com/abanna/herdr-agentisan/internal/telemetry"
	"github.com/abanna/herdr-agentisan/internal/telemetry/telemetrytest"
)

// setPanes replaces the panes herdr lists. It never edits a listed pane in
// place: the server encodes a reply after the lock is released.
func (th *teamHerdr) setPanes(panes []map[string]any) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.panes = panes
}

// listedPanes is a deep copy of the panes herdr lists now, for a test to
// edit and set.
func (th *teamHerdr) listedPanes() []map[string]any {
	th.mu.Lock()
	defer th.mu.Unlock()
	out := make([]map[string]any, 0, len(th.panes))
	for _, p := range th.panes {
		out = append(out, maps.Clone(p))
	}
	return out
}

// bindWorker writes a workers row binding pane to logicalID into the state
// database of a daemon about to run in stateDir.
func bindWorker(t *testing.T, stateDir string, th *teamHerdr, logicalID, pane string) {
	t.Helper()
	p := paths(t, stateDir, th.fakeHerdr)
	require.NoError(t, os.MkdirAll(p.Dir, 0o700))
	st, err := store.Open(t.Context(), p.DB)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	raw, err := sql.Open("sqlite", p.DB)
	require.NoError(t, err)
	defer raw.Close() //nolint:errcheck // test cleanup
	_, err = raw.ExecContext(t.Context(),
		"INSERT INTO workers (logical_id, project, grp, pane_id, generation, status, created_at) VALUES (?, 'agentisan', 'coders', ?, 1, 'active', 0)",
		logicalID, pane)
	require.NoError(t, err)
}

// statusEvent is the event a liveSpaces pane in ◆ coders gives, unbound
// unless id is not its pane.
func statusEvent(id, pane, agent, from, to string) telemetrytest.Event {
	return telemetrytest.Event{Name: telemetry.EventStatusChanged, Attrs: map[string]any{
		"agentisan.worker.id": id, "agentisan.worker.bound": id != pane,
		"agentisan.project": "", "agentisan.group": "coders", "agentisan.role": "worker",
		"agentisan.agent": agent, "herdr.pane.id": pane,
		"agentisan.status.from": from, "agentisan.status.to": to,
	}}
}

// TestTeamPollEmitsStatusTransitions is D10 on the daemon: the first poll is
// the baseline and emits nothing; after it, each pane whose status changed,
// appeared or left gives one event, with the logical id state.db binds and
// the project and group the resolver placed it in; a poll that changes
// nothing gives none.
func TestTeamPollEmitsStatusTransitions(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	dir := shortDir(t)
	bindWorker(t, dir, th, "coders-1", "wN:p1")
	events, rec := telemetrytest.NewEvents()
	o := teamOpts(t, dir, th, "")
	o.Team.Events = events
	run(t, o)

	polled(t, th.srv, 2)
	assert.Empty(t, rec.Events(), "the first poll is a baseline: a restart is not every pane appearing")

	var next []map[string]any
	for _, p := range th.listedPanes() {
		switch p["pane_id"] {
		case "wN:p3": // the blocked one leaves
			continue
		case "wN:p1":
			p["agent_status"] = "idle"
		case "wJ:p1": // the boss
			p["agent_status"] = "done"
		}
		next = append(next, p)
	}
	next = append(next, map[string]any{"pane_id": "wN:p9", "workspace_id": "wN", "agent": "codex", "agent_status": "idle", "focused": false})
	th.setPanes(next) // one swap, so one poll sees every change

	boss := statusEvent("wJ:p1", "wJ:p1", "claude", "working", "done")
	boss.Attrs["agentisan.group"], boss.Attrs["agentisan.role"] = "", "boss"
	want := []telemetrytest.Event{
		boss,
		statusEvent("coders-1", "wN:p1", "claude", "working", "idle"),
		statusEvent("wN:p9", "wN:p9", "codex", "absent", "idle"),
		statusEvent("wN:p3", "wN:p3", "claude", "blocked", "absent"),
	}
	require.Eventually(t, func() bool { return len(rec.Events()) >= len(want) }, ready, 5*time.Millisecond,
		"the changes gave no events")
	polled(t, th.srv, 2)
	assert.Equal(t, want, rec.Events(), "one event per change, and none from the polls after")
}

// TestTeamPollKeepsItsBaselineAcrossAFailedList: a poll whose pane.list
// fails builds no model, and must not become the baseline, or the next good
// poll would report every pane appearing. The change made during the
// failure is reported once, against the status before it.
func TestTeamPollKeepsItsBaselineAcrossAFailedList(t *testing.T) {
	t.Parallel()
	th := newTeamHerdr(t)
	events, rec := telemetrytest.NewEvents()
	var logs lockedLog
	o := teamOpts(t, shortDir(t), th, "")
	o.Team.Events = events
	o.Logger = zerolog.New(&logs)
	run(t, o)
	polled(t, th.srv, 1)

	good := th.listedPanes()
	// A pane with no id makes the client refuse the whole list. A poll that
	// fails it never asks for the workspaces, so polled cannot count it: the
	// log does.
	th.setPanes(append(th.listedPanes(), map[string]any{"pane_id": "", "workspace_id": "wN"}))
	require.Eventually(t, func() bool { return strings.Count(logs.String(), "team poll: list panes") >= 2 }, ready, 5*time.Millisecond,
		"two polls failed to list the panes")
	assert.Empty(t, rec.Events(), "a failed poll emits nothing")

	for _, p := range good {
		if p["pane_id"] == "wY:p1" {
			p["agent_status"] = "working"
		}
	}
	th.setPanes(good)
	require.Eventually(t, func() bool { return len(rec.Events()) > 0 }, ready, 5*time.Millisecond)
	polled(t, th.srv, 2)
	got := rec.Events()
	require.Len(t, got, 1, "only the real change, no pane 'appearing' from an empty baseline: %v", got)
	assert.Equal(t, "wY:p1", got[0].Attrs["herdr.pane.id"])
	assert.Equal(t, "idle", got[0].Attrs["agentisan.status.from"])
	assert.Equal(t, "working", got[0].Attrs["agentisan.status.to"])
	assert.Equal(t, "qa", got[0].Attrs["agentisan.group"])
}

// collector is an OTLP/HTTP receiver on loopback that records each export.
type collector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func newCollector(t *testing.T, status int) *collector {
	t.Helper()
	c := &collector{status: status}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, r.URL.Path+"|"+string(body))
		c.mu.Unlock()
		w.WriteHeader(c.status)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) endpoint() string { return strings.TrimPrefix(c.srv.URL, "http://") }

func (c *collector) received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...)
}

// configWith writes a shared file holding body and returns its directory.
func configWith(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, settings.FileName), []byte("schema_version = 1\n"+body), 0o600))
	return dir
}

func telemetryConfig(endpoint string) telemetry.Config {
	return telemetry.Config{ServiceName: "herdr-agentisan", Version: "v1", Commit: "c0ffee", Env: "development", OTLPEndpoint: endpoint}
}

// TestSetupEventsReadsTheEndpoint: the daemon's events go to the endpoint
// HERDR_AGENTISAN_OTLP_ENDPOINT names, else telemetry.endpoint in the shared
// file; with neither, nothing is built and the daemon emits into a nil
// Events. A file that does not resolve is logged and the daemon runs without
// events. Each flush returns well inside its bound.
//
// Not parallel: SetupEvents routes otel's process-wide error handler to the
// daemon's log, and this test must own it while it asserts on that log.
func TestSetupEventsReadsTheEndpoint(t *testing.T) {
	tests := map[string]struct {
		file       string // the shared file's body; "-" for no config dir
		env        string // HERDR_AGENTISAN_OTLP_ENDPOINT, ENDPOINT the collector's
		wantSent   bool
		wantLogs   []string
		wantUnsaid []string
	}{
		"no config dir, no environment": {file: "-"},
		"no key":                        {file: "[spaces]\n"},
		"the key":                       {file: "[telemetry]\nendpoint = \"ENDPOINT\"\n", wantSent: true, wantLogs: []string{"workflow events export over OTLP"}},
		"the environment":               {file: "-", env: "ENDPOINT", wantSent: true},
		"the environment beats the key": {file: "[telemetry]\nendpoint = \"localhost:1\"\n", env: "ENDPOINT", wantSent: true},
		"a broken file":                 {file: "[telemetry]\nendpoint = \"http://x:1\"\n", wantLogs: []string{`"level":"error"`, "telemetry.endpoint", "without workflow events"}},
		"credentials in the environment": {
			file: "-", env: "u:s3cret@ENDPOINT",
			wantLogs: []string{`"level":"error"`, "HERDR_AGENTISAN_OTLP_ENDPOINT", "without workflow events"}, wantUnsaid: []string{"s3cret"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := newCollector(t, http.StatusOK)
			dir := ""
			if tc.file != "-" {
				dir = configWith(t, strings.ReplaceAll(tc.file, "ENDPOINT", c.endpoint()))
			}
			env := strings.ReplaceAll(tc.env, "ENDPOINT", c.endpoint())
			var logs lockedLog
			events, flush := daemon.SetupEvents(t.Context(), dir, telemetryConfig(env), zerolog.New(&logs))
			events.StatusChanged(t.Context(), telemetry.StatusChange{WorkerID: "coders-1", PaneID: "wN:p1", From: "working", To: "idle"})
			start := time.Now()
			flush()
			assert.Less(t, time.Since(start), daemon.EventsFlushTimeout+time.Second)

			if !tc.wantSent {
				assert.Nil(t, events, "no endpoint, no events")
				assert.Empty(t, c.received())
			} else {
				require.Len(t, c.received(), 1, "the flush sent the event")
				assert.Contains(t, c.received()[0], "/v1/logs|")
				assert.Contains(t, c.received()[0], "agentisan.worker.status_changed")
			}
			for _, s := range tc.wantLogs {
				assert.Contains(t, logs.String(), s)
			}
			for _, s := range tc.wantUnsaid {
				assert.NotContains(t, logs.String(), s, "the log never carries the endpoint's credentials")
			}
		})
	}
}

// TestSetupEventsLogsExportErrors: a collector that refuses the export is a
// warning in the daemon's log, never a failure of the daemon.
//
// Not parallel: it owns otel's process-wide error handler.
func TestSetupEventsLogsExportErrors(t *testing.T) {
	c := newCollector(t, http.StatusBadRequest) // not retried: the export fails at once
	var logs lockedLog
	events, flush := daemon.SetupEvents(t.Context(), "", telemetryConfig(c.endpoint()), zerolog.New(&logs))
	require.NotNil(t, events)
	events.StatusChanged(t.Context(), telemetry.StatusChange{WorkerID: "w", From: "idle", To: "working"})
	flush()

	assert.NotEmpty(t, c.received(), "the export was tried")
	assert.Contains(t, logs.String(), `"level":"warn"`)
	assert.Contains(t, logs.String(), "telemetry export")
}

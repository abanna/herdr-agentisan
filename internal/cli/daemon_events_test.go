package cli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/abanna/herdr-agentisan/internal/cli"
	"github.com/abanna/herdr-agentisan/internal/daemon"
	"github.com/abanna/herdr-agentisan/internal/herdr/herdrtest"
	"github.com/abanna/herdr-agentisan/internal/settings"
)

// TestDaemonRunExportsStatusEvents is the live check's path, end to end on
// loopback: `daemon run` reads [telemetry] endpoint from
// HERDR_PLUGIN_CONFIG_DIR, takes its first poll as the baseline, and a pane's
// status changing after it reaches the OTLP collector as a status event,
// flushed on exit at the latest.
func TestDaemonRunExportsStatusEvents(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var exports []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		exports = append(exports, r.URL.Path+"|"+string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	dir, err := os.MkdirTemp("", "dc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := daemonEnv{herdrSock: filepath.Join(dir, "h.sock"), stateDir: filepath.Join(dir, "state")}
	// The pane is working until the team poll has listed the workspaces
	// once, which it does only after a pane.list: so the baseline sees it
	// working, and every poll after sees it idle.
	var baselined atomic.Bool
	var polls atomic.Int32
	herdrtest.StartAt(t, e.herdrSock, func(r herdrtest.Request) herdrtest.Reply {
		switch r.Method {
		case "pane.list":
			status := "working"
			if baselined.Load() {
				status = "idle"
			}
			return herdrtest.Reply{Result: map[string]any{"type": "pane_list", "panes": []map[string]any{
				{"pane_id": "wN:p1", "workspace_id": "wN", "agent": "claude", "agent_status": status},
			}}}
		case "workspace.list":
			baselined.Store(true)
			polls.Add(1)
			return herdrtest.Reply{Result: map[string]any{"type": "workspace_list", "workspaces": []map[string]any{
				{"workspace_id": "wN", "label": "◆ coders"},
			}}}
		case "workspace.report_metadata":
			return herdrtest.Reply{Result: map[string]any{"type": "ok"}}
		}
		return herdrtest.Reply{Result: map[string]any{"type": "pong", "version": "0.9.3", "protocol": 22}}
	})
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, settings.FileName),
		[]byte("schema_version = 1\n[telemetry]\nendpoint = \""+strings.TrimPrefix(collector.URL, "http://")+"\"\n"), 0o600))
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(cli.WithLookupEnv(daemonCtx(t, e, nil, &logs),
		e.lookup(map[string]string{"HERDR_PLUGIN_CONFIG_DIR": configDir})))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runCtx(ctx, "daemon", "run")
		done <- err
	}()
	// Two polls after the baseline: the change is one of them, however the
	// first poll and the flip interleave.
	require.Eventually(t, func() bool { return polls.Load() >= 3 }, ready+2*daemon.PollInterval, 10*time.Millisecond,
		"the daemon did not poll three times")
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(ready):
		t.Fatal("daemon run did not stop")
	}

	mu.Lock()
	got := strings.Join(exports, "\n")
	mu.Unlock()
	require.NotEmpty(t, got, "no export reached the collector")
	assert.Contains(t, got, "/v1/logs|")
	assert.Contains(t, got, "agentisan.worker.status_changed")
	assert.Contains(t, got, "wN:p1")
	assert.Contains(t, got, "working")
	assert.Contains(t, got, "idle")
	assert.Equal(t, 1, strings.Count(got, "agentisan.worker.status_changed"), "one change, one event: the baseline emitted none")

	paths, err := daemon.PathsFor(e.stateDir, e.herdrSock)
	require.NoError(t, err)
	raw, err := os.ReadFile(paths.Log)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "workflow events export over OTLP", "the endpoint came from HERDR_PLUGIN_CONFIG_DIR")
}
